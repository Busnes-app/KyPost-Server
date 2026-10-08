package sso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// NativeReleaseFloor is the signed evidence a restore hold release consumed
// for one subject. Active directory state below Revision, or at it when the
// evidence was inactive, predates or contradicts that evidence, so it cannot
// provision, allocate or reactivate. Newer revisions and inactive state pass.
type NativeReleaseFloor struct {
	Revision int64 `json:"revision"`
	Active   bool  `json:"active"`
}

var ErrNativeReleaseFloor = errors.New("native subject is at or below its restore release floor; only a newer KyIdentity revision (resync) can provision or reactivate it")

// RecordNativeReleaseFloors raises floors monotonically: a lower revision never
// replaces a higher one, and disagreeing evidence at one revision keeps inactive.
func (s *LifecycleStore) RecordNativeReleaseFloors(issuer string, floors map[string]NativeReleaseFloor) error {
	return fsutil.WithFileLock(s.path, func() error {
		f, err := s.load()
		if err != nil {
			return err
		}
		if err = f.raiseReleaseFloors(issuer, floors); err != nil {
			return err
		}
		return fsutil.PersistJSONFile(s.path, f)
	})
}

func (f *lifecycleFile) raiseReleaseFloors(issuer string, floors map[string]NativeReleaseFloor) error {
	if !directoryIdentifier(issuer) {
		return ErrNativeRecovery
	}
	// Validate every floor before changing the loaded file.
	for subject, floor := range floors {
		if !nativeRecoveryIdentifier(subject) || floor.Revision <= 0 {
			return ErrNativeRecovery
		}
	}
	if f.ReleaseFloors == nil {
		f.ReleaseFloors = map[string]NativeReleaseFloor{}
	}
	for subject, floor := range floors {
		k := directoryKey(issuer, subject)
		prior, ok := f.ReleaseFloors[k]
		switch {
		case !ok || floor.Revision > prior.Revision:
			f.ReleaseFloors[k] = floor
		case floor.Revision == prior.Revision:
			prior.Active = prior.Active && floor.Active
			f.ReleaseFloors[k] = prior
		}
	}
	return nil
}

// refuses reports whether active state at revision predates the evidence.
// KyIdentity bumps the revision on every desired-state change, so the evidence
// revision with the evidence's own activity is that state, not a stale one.
// Inactive state always passes: it can only fail closed.
func (floor NativeReleaseFloor) refuses(revision int64, active bool) bool {
	return active && (revision < floor.Revision || revision == floor.Revision && !floor.Active)
}

// CheckNativeReleaseFloor refuses provisioning from retained directory state
// the release evidence superseded; the disable path always passes.
func (s *LifecycleStore) CheckNativeReleaseFloor(issuer, subject string, d DirectoryState) error {
	f, err := s.load()
	if err != nil {
		return err
	}
	if floor, ok := f.ReleaseFloors[directoryKey(issuer, subject)]; ok && floor.refuses(d.Revision, d.Active) {
		return ErrNativeReleaseFloor
	}
	return nil
}

// NativeRestoreReleasedFile is the hold, renamed by a completed release. It
// keeps the hold's epoch; RequireNativeRestoreReleased only looks for the hold.
const NativeRestoreReleasedFile = "native-restore-released.json"

// nativeReleaseEventID marks directory rows the release wrote from evidence.
const nativeReleaseEventID = "native-restore-release"

// NativeRestoreReleaseRecord is written as the release intent before any other
// release write. While the hold exists it refuses a second attempt; only a new
// challenge, and so fresh evidence, clears it.
type NativeRestoreReleaseRecord struct {
	Epoch         string     `json:"epoch"`
	Nonce         string     `json:"nonce"`
	ReceiptDigest string     `json:"receiptDigest"`
	AfterDigest   string     `json:"afterDigest"`
	Actor         string     `json:"actor"`
	At            time.Time  `json:"at"`
	Deactivated   []string   `json:"deactivated,omitempty"` // non-native accounts (a)
	Demoted       []string   `json:"demoted,omitempty"`     // non-native administrators (a)
	CompletedAt   *time.Time `json:"completedAt,omitempty"`
}

// ErrNativeReleaseUnconfirmed: the hold was renamed, so the host is released,
// but a later step failed; repeating the request confirms and records it.
var ErrNativeReleaseUnconfirmed = errors.New("native restore hold released, but its durability is unconfirmed; repeat the request to confirm")

// NativeRestoreRefusedError names every release precondition that failed.
type NativeRestoreRefusedError struct{ Reasons []string }

func (e *NativeRestoreRefusedError) Error() string {
	return "native restore hold release refused: " + strings.Join(e.Reasons, "; ")
}

// NativeRestoreReleasePlan is everything one release writes, computed and
// validated before the first write. Record.Deactivated and Record.Demoted are
// the account changes (a); the release never activates or promotes.
type NativeRestoreReleasePlan struct {
	Record NativeRestoreReleaseRecord
	s      *LifecycleStore
	root   string
	issuer string
	floors map[string]NativeReleaseFloor
	rows   map[string]DirectoryState // evidence-inactive subjects whose row was active
}

// PlanNativeRestoreReleaseHeld checks P1, P2, P3, P7 and P9 with the functions
// status uses, under the caller's settings -> directory -> users fences, and
// writes nothing. P5, P6 and P8 belong to the caller.
func (s *LifecycleStore) PlanNativeRestoreReleaseHeld(root string, settings SSOSettings, key []byte, accounts []users.User, actor string, now time.Time) (*NativeRestoreReleasePlan, error) {
	current, revisions, _, inputErr := s.nativeRecoveryInputs(root, settings, key, accounts)
	if errors.Is(inputErr, errNativeRecoveryNoSubjects) {
		return nil, &NativeRestoreRefusedError{[]string{"P9: zero-subject hold: its release path is not available yet"}}
	}
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	if inputErr != nil {
		return nil, &NativeRestoreRefusedError{[]string{"P2: " + uncomputedAuthority}}
	}
	var reasons []string
	add := func(id string, list []string) {
		for _, r := range list {
			reasons = append(reasons, id+": "+r)
		}
	}
	journal, authority := s.completedRepairReasons(f, root, current, revisions, key, accounts, now)
	add("P1", journal)
	add("P2", authority)
	q, qualErr := checkRestoreQualification(s, root)
	var unqualified *NativeRestoreUnqualifiedError
	switch {
	case errors.As(qualErr, &unqualified):
		add("P3", unqualified.Reasons)
		add("P7", []string{"needs the restore qualification marker (P3)"})
	case qualErr != nil:
		add("P3", []string{"restore qualification cannot be checked"})
		add("P7", []string{"needs the restore qualification marker (P3)"})
	case f.RecoveryReceipt == nil:
		add("P7", []string{"no recovery challenge is recorded"})
	default:
		add("P7", s.nativeRestoreTokenFenceReasons(f, q, &f.RecoveryReceipt.Challenge))
	}
	// A record from another epoch belongs to an earlier, restored-over release.
	if f.RestoreRelease != nil && f.RestoreRelease.Epoch == current.Epoch {
		reasons = append(reasons, "an earlier release attempt for this hold was interrupted; request a new challenge and fresh evidence")
	}
	if len(reasons) != 0 {
		return nil, &NativeRestoreRefusedError{reasons}
	}
	receipt, repair := f.RecoveryReceipt, f.RecoveryRepair
	before := current
	before.AuthorityDigest = repair.BeforeDigest
	evidence, err := validateNativeRecoveryEvidence(before, revisions, &receipt.Challenge, key, receipt.Body, receipt.Headers, now)
	if err != nil {
		return nil, &NativeRestoreRefusedError{[]string{"P1: recorded evidence no longer verifies"}}
	}
	p := &NativeRestoreReleasePlan{
		Record: NativeRestoreReleaseRecord{Epoch: current.Epoch, Nonce: receipt.Challenge.Nonce, ReceiptDigest: repair.ReceiptDigest, AfterDigest: repair.AfterDigest, Actor: actor, At: now},
		s:      s, root: root, issuer: current.Issuer,
		floors: map[string]NativeReleaseFloor{}, rows: map[string]DirectoryState{},
	}
	inactive, notAdmin := map[string]bool{}, map[string]bool{}
	for _, sub := range evidence.Subjects {
		var profile DirectoryUser
		if json.Unmarshal(sub.Profile, &profile) != nil {
			return nil, ErrNativeRecovery
		}
		revision := *sub.Revision
		p.floors[sub.ID] = NativeReleaseFloor{Revision: revision, Active: *profile.Active}
		if *profile.Active {
			notAdmin[sub.ID] = !HasAdminRole(profile.Roles)
			continue
		}
		inactive[sub.ID] = true
		k := directoryKey(current.Issuer, sub.ID)
		prior, known := f.Directory[k]
		if known && !prior.Active {
			continue // already inactive; nothing to revive
		}
		// Expiry need not allocate a revision, so the evidence may deactivate
		// the row at its own revision; validation already proved it is not older.
		resource := profile
		resource.Schemas = []string{scimUserSchema}
		resource.Meta.Version = fmt.Sprintf(`W/"%d"`, revision)
		if _, err := resource.Revision("user.updated"); err != nil {
			reasons = append(reasons, "P1: evidence profile for subject "+sub.ID+" is not a directory resource")
			continue
		}
		p.rows[k] = DirectoryState{Resource: &resource, Revision: revision, Digest: EventDigest("recovery.evidence", sub.Profile), EventID: nativeReleaseEventID, RevokedBefore: prior.RevokedBefore}
	}
	for _, u := range accounts {
		switch {
		case !u.Active || u.NativeMailboxIssuer != "" || u.NativeMailboxSource != "":
		case inactive[u.SSOSub]:
			p.Record.Deactivated = append(p.Record.Deactivated, u.ID)
		case notAdmin[u.SSOSub] && u.Role == users.RoleAdmin:
			p.Record.Demoted = append(p.Record.Demoted, u.ID)
		}
	}
	if len(reasons) != 0 {
		return nil, &NativeRestoreRefusedError{reasons}
	}
	return p, nil
}

// RecordIntent persists the release record before any account, ledger or
// directory write. The caller holds the plan's fences.
func (p *NativeRestoreReleasePlan) RecordIntent(ctx context.Context) error {
	f, err := p.s.load()
	if err != nil {
		return err
	}
	if f.RestoreRelease != nil && f.RestoreRelease.Epoch == p.Record.Epoch {
		return ErrNativeRecovery
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if epoch, err := nativeRecoveryEpoch(p.root); err != nil || epoch != p.Record.Epoch {
		return ErrNativeRecovery
	}
	record := p.Record
	f.RestoreRelease = &record
	return fsutil.PersistJSONFile(p.s.path, f)
}

// Commit runs after the intent and account writes, under the plan's fences.
// Its writes, each a crash point:
//   - "ledger": reservations and address states follow the evidence-inactive
//     rows (fail closed: only deactivates; any later commit recomputes them);
//   - "lifecycle": one write of rows, release floors and token fences;
//   - "rename": the hold becomes the released marker, then SyncDir.
//
// Before the rename the hold stays and the intent refuses a retry until a new
// challenge. After it the host is released: a later failure returns
// ErrNativeReleaseUnconfirmed. hit injects crashes in tests (nil in production).
func (p *NativeRestoreReleasePlan) Commit(ctx context.Context, hit func(string) error) error {
	if hit == nil {
		hit = func(string) error { return nil }
	}
	load := func() (lifecycleFile, error) {
		f, err := p.s.load()
		if err == nil && (f.RestoreRelease == nil || !reflect.DeepEqual(*f.RestoreRelease, p.Record)) {
			err = ErrNativeRecovery
		}
		return f, err
	}
	if _, err := load(); err != nil {
		return err
	}
	ledger, err := p.s.loadNative()
	if err != nil {
		return err
	}
	// A reservation at the row's revision mirrors that row's desired state.
	for k, d := range p.rows {
		if a, ok := ledger.Accounts[k]; ok && a.Revision == d.Revision {
			a.DesiredActive, a.Digest = false, d.Digest
			ledger.Accounts[k] = a
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// As applyDirectory: address states follow the rows before they are
	// recorded. Pending routes are retried by every later commit and the worker;
	// import already quarantines on the ledger generation.
	if len(p.rows) != 0 && len(ledger.Accounts) != 0 {
		if err = p.s.commitNative(ledger, p.rows); err != nil && !errors.Is(err, ErrNativeRoutesPending) {
			return err
		}
	}
	if err = hit("ledger"); err != nil {
		return err
	}
	// Re-read: commitNative may have written the lifecycle (initialization).
	f, err := load()
	if err != nil {
		return err
	}
	// Every token the verifier could still admit, as FenceRestoredNativeTokens.
	cutoff := time.Now().Unix() + 31
	directory := maps.Clone(f.Directory)
	maps.Copy(directory, p.rows)
	fence := func(k string) bool {
		d, ok := directory[k]
		d.RevokedBefore = max(d.RevokedBefore, cutoff)
		directory[k] = d
		return ok
	}
	for subject := range p.floors {
		if _, ok := directory[directoryKey(p.issuer, subject)]; ok {
			fence(directoryKey(p.issuer, subject))
		}
	}
	for k, a := range ledger.Accounts {
		if a.Source != "" && !fence(k) {
			return ErrNativeProvisioning
		}
	}
	f.Directory = directory
	f.ReleaseFloors = maps.Clone(f.ReleaseFloors)
	if err = f.raiseReleaseFloors(p.issuer, p.floors); err != nil {
		return err
	}
	if err = fsutil.PersistJSONFile(p.s.path, f); err != nil {
		return err
	}
	if err = hit("lifecycle"); err != nil {
		return err
	}
	if epoch, err := nativeRecoveryEpoch(p.root); err != nil || epoch != p.Record.Epoch {
		return ErrNativeRecovery
	}
	if err = os.Rename(filepath.Join(p.root, NativeRestoreHoldFile), filepath.Join(p.root, NativeRestoreReleasedFile)); err != nil {
		return err
	}
	if err = errors.Join(fsutil.SyncDir(p.root), hit("rename")); err != nil {
		return fmt.Errorf("%w: %w", ErrNativeReleaseUnconfirmed, err)
	}
	return nil
}

// NativeRestoreReleased reports a completed release: no hold, and a released
// marker whose epoch matches the recorded release.
func (s *LifecycleStore) NativeRestoreReleased(root string) (NativeRestoreReleaseRecord, bool, error) {
	if _, err := os.Lstat(filepath.Join(root, NativeRestoreHoldFile)); !errors.Is(err, os.ErrNotExist) {
		return NativeRestoreReleaseRecord{}, false, nil
	}
	if _, err := os.Lstat(filepath.Join(root, NativeRestoreReleasedFile)); errors.Is(err, os.ErrNotExist) {
		return NativeRestoreReleaseRecord{}, false, nil
	}
	epoch, err := readRestoreEpoch(filepath.Join(root, NativeRestoreReleasedFile))
	if err != nil {
		return NativeRestoreReleaseRecord{}, false, err
	}
	f, err := s.load()
	if err != nil {
		return NativeRestoreReleaseRecord{}, false, err
	}
	if f.RestoreRelease == nil || f.RestoreRelease.Epoch != epoch {
		return NativeRestoreReleaseRecord{}, false, ErrNativeRecovery
	}
	return *f.RestoreRelease, true, nil
}

// CompleteNativeRestoreRelease records completion once, after its audit.
func (s *LifecycleStore) CompleteNativeRestoreRelease(root string) error {
	return fsutil.WithFileLock(s.path, func() error {
		record, released, err := s.NativeRestoreReleased(root)
		if err != nil || !released {
			return errors.Join(err, ErrNativeRecovery)
		}
		if record.CompletedAt != nil {
			return nil
		}
		f, err := s.load()
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		f.RestoreRelease.CompletedAt = &now
		return fsutil.PersistJSONFile(s.path, f)
	})
}
