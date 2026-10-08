package sso

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cfreceiving"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// NativeRestoreReleaseCheck is one hold release precondition. CheckedAtRelease
// marks one only the release request itself can decide; OK is then false.
type NativeRestoreReleaseCheck struct {
	ID               string   `json:"id"`
	OK               bool     `json:"ok"`
	CheckedAtRelease bool     `json:"checkedAtRelease,omitempty"`
	Reasons          []string `json:"reasons"`
}

// NativeRestoreReleaseStatus reports why a held restore can or cannot be released.
type NativeRestoreReleaseStatus struct {
	Held           bool                        `json:"held"`
	Released       bool                        `json:"released"`
	ResyncSubjects []string                    `json:"resyncSubjects,omitempty"` // active floors no newer revision has passed yet
	Epoch          string                      `json:"epoch,omitempty"`
	Preconditions  []NativeRestoreReleaseCheck `json:"preconditions"`
	ReleaseEnabled bool                        `json:"releaseEnabled"`
	Cloudflare     string                      `json:"cloudflare,omitempty"` // live, fenced or error; omitted when not configured
	NextSteps      []string                    `json:"nextSteps"`
}

// NativeRestoreReleaseStatus changes no state and takes no locks; SQLite may
// create -shm/-wal companions beside mailbox databases. Lock-free reads can
// mix two moments, so status is advisory: the release re-checks the same
// functions under its fences. It never runs DNS.
func (s *LifecycleStore) NativeRestoreReleaseStatus(stateRoot, secretDir string, settings SSOSettings, key []byte, accounts []users.User, now time.Time) NativeRestoreReleaseStatus {
	st := NativeRestoreReleaseStatus{Preconditions: []NativeRestoreReleaseCheck{}, NextSteps: []string{}}
	enabled, flagErr := config.NativeRestoreReleaseEnabled()
	st.ReleaseEnabled = enabled
	if _, _, live, err := (cfreceiving.Keys{Dir: secretDir}).Load(); err == nil && live {
		st.Cloudflare = "live"
	} else if err == nil {
		st.Cloudflare = "fenced"
	} else if !errors.Is(err, cfreceiving.ErrNoCredentials) {
		st.Cloudflare = "error"
	}
	if _, err := os.Lstat(filepath.Join(stateRoot, NativeRestoreHoldFile)); errors.Is(err, os.ErrNotExist) {
		return s.releasedStatus(st, stateRoot, settings)
	}
	st.Held = true
	st.Epoch, _ = nativeRecoveryEpoch(stateRoot)
	check := func(id string, reasons []string) {
		st.Preconditions = append(st.Preconditions, NativeRestoreReleaseCheck{ID: id, OK: len(reasons) == 0, Reasons: append([]string{}, reasons...)})
	}
	atRelease := func(id string, reasons ...string) {
		st.Preconditions = append(st.Preconditions, NativeRestoreReleaseCheck{ID: id, CheckedAtRelease: true, Reasons: reasons})
	}

	f, loadErr := s.load()
	current, revisions, _, inputErr := s.nativeRecoveryInputs(stateRoot, settings, key, accounts)
	var journal, authority []string
	switch {
	case loadErr != nil:
		journal, authority = []string{"sso-lifecycle.json is unreadable"}, []string{"sso-lifecycle.json is unreadable"}
	case errors.Is(inputErr, errNativeRecoveryNoSubjects):
		journal, authority = []string{"the hold has no native subjects to repair"}, []string{"the hold has no native subjects to repair"}
	case inputErr != nil && f.RecoveryReceipt != nil && f.RecoveryRepair != nil:
		journal, authority = []string{uncomputedAuthority}, []string{uncomputedAuthority}
	default:
		// Without a receipt or repair this reports their absence first.
		journal, authority = s.completedRepairReasons(f, stateRoot, current, revisions, key, accounts, now)
		if inputErr != nil {
			authority = append(authority, uncomputedAuthority)
		}
	}
	check("P1", journal)
	check("P2", authority)

	q, qualErr := checkRestoreQualification(s, stateRoot)
	var unqualified *NativeRestoreUnqualifiedError
	if errors.As(qualErr, &unqualified) {
		check("P3", unqualified.Reasons)
	} else if qualErr != nil {
		check("P3", []string{"restore qualification cannot be checked"})
	} else {
		check("P3", nil)
	}
	atRelease("P4", "release floors are written by the release from the evidence it consumes")
	atRelease("P5", "administrator step-up is checked at release")
	atRelease("P6", "only an active local-password administrator (not native, not KySignOn) may release; checked at release")

	challenge := f.RecoveryChallenge
	if f.RecoveryReceipt != nil {
		challenge = &f.RecoveryReceipt.Challenge
	}
	if !q.CreatedAt.IsZero() && q.Epoch != st.Epoch {
		check("P7", []string{"not evaluated: the qualification marker belongs to another restore epoch (P3)"})
	} else {
		check("P7", s.nativeRestoreTokenFenceReasons(f, q, challenge))
	}
	p8 := s.nativeRestoreDomainProofReasons(now)
	if len(p8) == 1 && p8[0] == noNativeDomain {
		check("P8", p8)
	} else {
		atRelease("P8", append(p8, "fresh domain proof is required at release")...)
	}
	if errors.Is(inputErr, errNativeRecoveryNoSubjects) {
		check("P9", []string{"zero-subject hold: its release path is not available yet"})
	} else if inputErr != nil {
		check("P9", []string{uncomputedAuthority})
	} else {
		check("P9", nil)
	}

	failed := func(id string) bool {
		i := slices.IndexFunc(st.Preconditions, func(c NativeRestoreReleaseCheck) bool { return c.ID == id })
		return !st.Preconditions[i].OK && !st.Preconditions[i].CheckedAtRelease
	}
	if failed("P3") {
		st.NextSteps = append(st.NextSteps, "Resolve the P3 reasons; a missing or stale qualification marker needs a fresh restore with this version.")
	}
	if errors.Is(inputErr, errNativeRecoveryNoSubjects) {
		st.NextSteps = append(st.NextSteps, "This hold has no native subjects; wait for the zero-subject release path.")
	} else if failed("P1") || failed("P2") || failed("P7") || failed("P9") {
		st.NextSteps = append(st.NextSteps, "Request a new recovery challenge, import fresh KyIdentity evidence and run repair within the evidence window (POST /api/admin/native-recovery/challenge, /evidence, /repair).")
	}
	if failed("P8") {
		st.NextSteps = append(st.NextSteps, "Configure a mail domain; release needs fresh DNS proof for at least one.")
	} else {
		st.NextSteps = append(st.NextSteps, "Verify a configured mail domain immediately before release.")
	}
	st.NextSteps = append(st.NextSteps, "Release requires administrator step-up by an active local-password administrator.")
	switch {
	case flagErr != nil:
		st.NextSteps = append(st.NextSteps, "KYPOST_NATIVE_RESTORE_RELEASE must be true or false.")
	case !enabled:
		st.NextSteps = append(st.NextSteps, "Hold release is off: set KYPOST_NATIVE_RESTORE_RELEASE=true to release.")
	default:
		st.NextSteps = append(st.NextSteps, "Release within the evidence window: POST /api/admin/native-recovery/release (with Maddy, add \"confirm\": \"original-host-decommissioned\").")
	}
	if st.Cloudflare == "fenced" {
		st.NextSteps = append(st.NextSteps, "After release, Cloudflare receiving stays with the other host until an explicit takeover: kypost-server receiving cloudflare takeover.")
	}
	return st
}

// releasedStatus reports no hold: released or never held. After a release the
// resync step stays until every active floor has a newer directory revision.
func (s *LifecycleStore) releasedStatus(st NativeRestoreReleaseStatus, stateRoot string, settings SSOSettings) NativeRestoreReleaseStatus {
	record, released, err := s.NativeRestoreReleased(stateRoot)
	switch {
	case err != nil:
		st.NextSteps = append(st.NextSteps, "A released marker exists but does not match the recorded release; preserve state and investigate.")
		return st
	case !released:
		st.NextSteps = append(st.NextSteps, "No native restore hold: nothing to release.")
		return st
	}
	st.Released, st.Epoch = true, record.Epoch
	f, err := s.load()
	if err != nil {
		st.NextSteps = append(st.NextSteps, "sso-lifecycle.json is unreadable; preserve it and repair.")
		return st
	}
	for k, floor := range f.ReleaseFloors {
		issuer, subject, _ := strings.Cut(k, "\x00")
		if floor.Active && issuer == settings.IssuerURL && f.Directory[k].Revision <= floor.Revision {
			st.ResyncSubjects = append(st.ResyncSubjects, subject)
		}
	}
	slices.Sort(st.ResyncSubjects)
	if record.CompletedAt == nil {
		st.NextSteps = append(st.NextSteps, "The release completion was not recorded; repeat the release request to record it.")
	}
	if len(st.ResyncSubjects) != 0 {
		st.NextSteps = append(st.NextSteps, "Run a KyIdentity resync now: the listed subjects cannot be provisioned or reactivated until KyIdentity sends a newer revision.")
	}
	st.NextSteps = append(st.NextSteps,
		"Restart the container: receiving starts only on restart.",
		"Quarantined inbound and outbound mail stays quarantined; devices must pair again.")
	if st.Cloudflare == "fenced" {
		st.NextSteps = append(st.NextSteps, "Cloudflare receiving stays with the other host until an explicit takeover: kypost-server receiving cloudflare takeover.")
	}
	return st
}

// checkRestoreQualification is P3; a variable only so tests can inject an error.
var checkRestoreQualification = (*LifecycleStore).CheckNativeRestoreQualification

// nativeRestoreTokenFenceReasons is P7: the qualification precedes the
// challenge, and every published native subject's token fence is at or
// after the qualification, so no pre-restore ID token survives.
func (s *LifecycleStore) nativeRestoreTokenFenceReasons(f lifecycleFile, q NativeRestoreQualification, challenge *NativeRecoveryChallenge) []string {
	if q.CreatedAt.IsZero() {
		return []string{"needs the restore qualification marker (P3)"}
	}
	var reasons []string
	if challenge == nil {
		reasons = append(reasons, "no recovery challenge is recorded")
	} else if challenge.CreatedAt.Before(q.CreatedAt) {
		reasons = append(reasons, "the recovery challenge predates the restore qualification; request a new challenge")
	}
	ledger, _, err := s.loadNativeLedger(true)
	if err != nil {
		return append(reasons, "native mailbox ledger is unreadable")
	}
	keys := make([]string, 0, len(ledger.Accounts))
	for k, a := range ledger.Accounts {
		if a.Source != "" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		d, ok := f.Directory[k]
		if !ok || time.Unix(d.RevokedBefore, 0).Before(q.CreatedAt) {
			reasons = append(reasons, "mailbox "+ledger.Accounts[k].Owner.Mailbox+" token fence predates the restore qualification")
		}
	}
	return reasons
}

const uncomputedAuthority = "current authority cannot be computed: needs a usable hold epoch, enabled KySignOn settings, the pairing key and consistent native accounts"

const noNativeDomain = "no mail domain is configured"

// nativeRestoreDomainProofReasons reports the last recorded proofs (P8). It
// performs no DNS lookup: only a fresh proof at release counts.
func (s *LifecycleStore) nativeRestoreDomainProofReasons(now time.Time) []string {
	set, err := NewNativeDomainStore(filepath.Dir(s.path)).ReadSet()
	if err != nil {
		return []string{"mail domain set is unreadable"}
	}
	if len(set.Domains) == 0 {
		return []string{noNativeDomain}
	}
	names := make([]string, 0, len(set.Domains))
	for name := range set.Domains {
		names = append(names, name)
	}
	slices.Sort(names)
	var reasons []string
	for _, name := range names {
		switch until := set.Domains[name].VerifiedUntil; {
		case until == 0:
			reasons = append(reasons, name+": no recorded proof")
		case now.Unix() < until:
			reasons = append(reasons, name+": last proof valid until "+time.Unix(until, 0).UTC().Format(time.RFC3339))
		default:
			reasons = append(reasons, name+": last proof expired at "+time.Unix(until, 0).UTC().Format(time.RFC3339))
		}
	}
	return reasons
}
