package sso

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

const MaxNativeRecoveryEvidenceBytes = 256 << 10

var ErrNativeRecovery = errors.New("native recovery evidence refused; preserve the hold and request fresh complete evidence for the current authority")
var recoveryIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var recoveryEpochPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// NativeRecoveryChallenge binds one export to retained authority, never access.
type NativeRecoveryChallenge struct {
	Epoch           string    `json:"epoch"`
	Issuer          string    `json:"issuer"`
	SystemID        string    `json:"systemId"`
	KeyFingerprint  string    `json:"keyFingerprint"`
	Nonce           string    `json:"nonce"`
	Subjects        []string  `json:"subjects"`
	AuthorityDigest string    `json:"authorityDigest"`
	CreatedAt       time.Time `json:"createdAt"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

type nativeRecoverySubject struct {
	ID       string          `json:"id"`
	Revision *int64          `json:"revision"`
	Profile  json.RawMessage `json:"profile"`
}
type nativeRecoveryEvidence struct {
	Version   int                     `json:"version"`
	Issuer    string                  `json:"issuer"`
	SystemID  string                  `json:"systemId"`
	Nonce     string                  `json:"nonce"`
	IssuedAt  time.Time               `json:"issuedAt"`
	ExpiresAt time.Time               `json:"expiresAt"`
	Subjects  []nativeRecoverySubject `json:"subjects"`
}
type nativeRecoveryReceipt struct {
	Challenge NativeRecoveryChallenge `json:"challenge"`
	Body      []byte                  `json:"body"`
	Headers   syncauth.Headers        `json:"headers"`
	ExpiresAt time.Time               `json:"expiresAt"`
}

func nativeRecoveryIdentifier(s string) bool { return recoveryIdentifierPattern.MatchString(s) }
func nativeRecoveryEpoch(root string) (string, error) {
	path := filepath.Join(root, NativeRestoreHoldFile)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		return "", ErrNativeRecovery
	}
	body, err := os.ReadFile(path)
	var hold struct {
		Version int    `json:"version"`
		Epoch   string `json:"epoch"`
		Reason  string `json:"reason"`
	}
	if err != nil || json.Unmarshal(body, &hold) != nil || hold.Version != 1 || !recoveryEpochPattern.MatchString(hold.Epoch) {
		return "", ErrNativeRecovery
	}
	return hold.Epoch, nil
}

// Caller holds settings -> directory -> users fences, including fresh accounts.
// Offline restore staging must be stopped/exclusive; it does not share these locks.
func (s *LifecycleStore) nativeRecoveryInputs(root string, settings SSOSettings, key []byte, accounts []users.User) (NativeRecoveryChallenge, map[string]int64, map[string]bool, error) {
	epoch, err := nativeRecoveryEpoch(root)
	if err != nil || !settings.Enabled || settings.IssuerURL == "" || settings.ClientID == "" || len(key) < syncauth.MinKeyBytes {
		return NativeRecoveryChallenge{}, nil, nil, ErrNativeRecovery
	}
	f, err := s.load()
	if err != nil {
		return NativeRecoveryChallenge{}, nil, nil, err
	}
	ledger, err := s.loadNative()
	if err != nil {
		return NativeRecoveryChallenge{}, nil, nil, err
	}
	revisions := map[string]int64{}
	directory := map[string]DirectoryState{}
	reservations := map[string]NativeAssignment{}
	reservedIDs := map[string]bool{}
	add := func(subject string, rev int64) error {
		if !nativeRecoveryIdentifier(subject) || rev < 0 {
			return ErrNativeRecovery
		}
		revisions[subject] = max(revisions[subject], rev)
		return nil
	}
	for k, d := range f.Directory {
		issuer, subject, ok := strings.Cut(k, "\x00")
		if !ok {
			return NativeRecoveryChallenge{}, nil, nil, ErrNativeRecovery
		}
		if issuer == settings.IssuerURL {
			if err = add(subject, d.Revision); err != nil {
				return NativeRecoveryChallenge{}, nil, nil, err
			}
			directory[k] = d
		}
	}
	for k, a := range ledger.Accounts {
		if a.Owner.Issuer != settings.IssuerURL || k != directoryKey(a.Owner.Issuer, a.Owner.Subject) || !fsutil.SafePathComponent(a.Owner.Mailbox) || reservedIDs[a.Owner.Mailbox] || a.Revision <= 0 || a.Digest == "" {
			return NativeRecoveryChallenge{}, nil, nil, ErrNativeRecovery
		}
		if err = add(a.Owner.Subject, a.Revision); err != nil {
			return NativeRecoveryChallenge{}, nil, nil, err
		}
		reservations[k] = a
		reservedIDs[a.Owner.Mailbox] = true
	}
	// Stable credential/access witnesses omit correspondence and login timestamps.
	type accountAuthority struct {
		ID, Subject, Issuer, Source, Password, Derivation, Salt string
		Iterations                                              int
		Role                                                    users.Role
		Active, Forced                                          bool
		Revoked                                                 int64
		Epoch, PGPRevision                                      uint64
	}
	authority := []accountAuthority{}
	nativeOwners := map[string]users.User{}
	seen := map[string]bool{}
	accountIDs := map[string]bool{}
	for _, u := range accounts {
		accountIDs[u.ID] = true
		if (u.NativeMailboxIssuer != "" || u.NativeMailboxSource != "") && (u.NativeMailboxIssuer != settings.IssuerURL || u.NativeMailboxSource == "" || !nativeRecoveryIdentifier(u.SSOSub)) {
			return NativeRecoveryChallenge{}, nil, nil, ErrNativeRecovery
		}
		if u.SSOSub == "" {
			if reservedIDs[u.ID] {
				authority = append(authority, accountAuthority{u.ID, u.SSOSub, u.NativeMailboxIssuer, u.NativeMailboxSource, u.PasswordHash, u.AuthDerivation, u.LoginSalt, u.LoginIterations, u.Role, u.Active, u.MustChangePassword, u.SSOLinkRevokedAt, u.NativeSendEpoch, u.PGPRevision})
			}
			continue
		}
		if seen[u.SSOSub] || !fsutil.SafePathComponent(u.ID) {
			return NativeRecoveryChallenge{}, nil, nil, ErrNativeRecovery
		}
		seen[u.SSOSub] = true
		if err = add(u.SSOSub, 0); err != nil {
			return NativeRecoveryChallenge{}, nil, nil, err
		}
		if u.NativeMailboxIssuer != "" || u.NativeMailboxSource != "" {
			a, ok := ledger.Accounts[directoryKey(settings.IssuerURL, u.SSOSub)]
			nativeOwners[directoryKey(settings.IssuerURL, u.SSOSub)] = u
			if !ok || a.Owner.Mailbox != u.ID || a.Source != u.NativeMailboxSource || u.NativeMailboxIssuer != settings.IssuerURL {
				return NativeRecoveryChallenge{}, nil, nil, ErrNativeRecovery
			}
		}
		authority = append(authority, accountAuthority{u.ID, u.SSOSub, u.NativeMailboxIssuer, u.NativeMailboxSource, u.PasswordHash, u.AuthDerivation, u.LoginSalt, u.LoginIterations, u.Role, u.Active, u.MustChangePassword, u.SSOLinkRevokedAt, u.NativeSendEpoch, u.PGPRevision})
	}
	// Only unpublished reservations have no account authority to revoke.
	// Published accounts must continue consuming ordinary offboarding/demotion.
	eligible := map[string]bool{}
	published := map[string]bool{}
	for sub := range seen {
		published[directoryKey(settings.IssuerURL, sub)] = true
	}
	for k, a := range reservations {
		if accountIDs[a.Owner.Mailbox] {
			published[k] = true
		}
		eligible[a.Owner.Subject] = !published[k]
	}
	for k, rev := range f.RecoveryFloors {
		issuer, subject, ok := strings.Cut(k, "\x00")
		if !ok || rev <= 0 {
			return NativeRecoveryChallenge{}, nil, nil, ErrNativeRecovery
		}
		if issuer == settings.IssuerURL {
			// Never discard an unsafe preview barrier or a partial-publication fence.
			if published[k] && rev >= directory[k].Revision && !f.RecoveryRepairBarriers[k].matches(nativeOwners[k], rev) {
				return NativeRecoveryChallenge{}, nil, nil, ErrNativeRecovery
			}
			if err = add(subject, rev); err != nil {
				return NativeRecoveryChallenge{}, nil, nil, err
			}
		}
	}
	subjects := make([]string, 0, len(revisions))
	for sub := range revisions {
		subjects = append(subjects, sub)
	}
	slices.Sort(subjects)
	if len(subjects) == 0 || len(subjects) > 256 {
		return NativeRecoveryChallenge{}, nil, nil, ErrNativeRecovery
	}
	slices.SortFunc(authority, func(a, b accountAuthority) int { return strings.Compare(a.ID, b.ID) })
	fingerprint := sha256.Sum256(key)
	payload, err := json.Marshal(struct {
		Settings     SSOSettings
		Directory    map[string]DirectoryState
		Reservations map[string]NativeAssignment
		Accounts     []accountAuthority
	}{settings, directory, reservations, authority})
	if err != nil {
		return NativeRecoveryChallenge{}, nil, nil, err
	}
	digest := sha256.Sum256(payload)
	return NativeRecoveryChallenge{Epoch: epoch, Issuer: settings.IssuerURL, KeyFingerprint: hex.EncodeToString(fingerprint[:]), Subjects: subjects, AuthorityDigest: hex.EncodeToString(digest[:])}, revisions, eligible, nil
}

// BeginNativeRecoveryHeld requires the caller's authority fences. A new challenge
// invalidates the receipt but never weakens revision barriers or removes the hold.
func (s *LifecycleStore) BeginNativeRecoveryHeld(ctx context.Context, root string, settings SSOSettings, key []byte, accounts []users.User, systemID, fingerprint string) (NativeRecoveryChallenge, error) {
	c, _, _, err := s.nativeRecoveryInputs(root, settings, key, accounts)
	if err != nil {
		return c, err
	}
	if !nativeRecoveryIdentifier(systemID) || fingerprint != c.KeyFingerprint {
		return NativeRecoveryChallenge{}, ErrNativeRecovery
	}
	c.SystemID = systemID
	c.Nonce, err = RandomToken(32)
	if err != nil {
		return NativeRecoveryChallenge{}, err
	}
	c.CreatedAt = time.Now().UTC()
	c.ExpiresAt = c.CreatedAt.Add(15 * time.Minute)
	f, err := s.load()
	if err != nil {
		return NativeRecoveryChallenge{}, err
	}
	f.RecoveryChallenge = &c
	f.RecoveryReceipt = nil
	f.RecoveryRepair = nil
	if err = ctx.Err(); err != nil {
		return NativeRecoveryChallenge{}, err
	}
	epoch, err := nativeRecoveryEpoch(root)
	if err != nil || epoch != c.Epoch {
		return NativeRecoveryChallenge{}, ErrNativeRecovery
	}
	err = fsutil.PersistJSONFile(s.path, f)
	return c, err
}

// AcceptNativeRecoveryHeld verifies exact provider bytes against the selected
// pairing key only. Receipt, one-use consumption and floors publish in one write.
// Caller retains settings -> directory -> users fences through this commit.
func (s *LifecycleStore) AcceptNativeRecoveryHeld(ctx context.Context, root string, settings SSOSettings, key []byte, accounts []users.User, body []byte, headers syncauth.Headers) error {
	if len(body) == 0 || len(body) > MaxNativeRecoveryEvidenceBytes {
		return ErrNativeRecovery
	}
	current, minRevisions, eligible, err := s.nativeRecoveryInputs(root, settings, key, accounts)
	if err != nil {
		return err
	}
	f, err := s.load()
	if err != nil {
		return err
	}
	c := f.RecoveryChallenge
	evidence, err := validateNativeRecoveryEvidence(current, minRevisions, c, key, body, headers, time.Now().UTC())
	if err != nil {
		return err
	}
	for _, sub := range evidence.Subjects {
		if eligible[sub.ID] {
			if f.RecoveryFloors == nil {
				f.RecoveryFloors = map[string]int64{}
			}
			k := directoryKey(c.Issuer, sub.ID)
			f.RecoveryFloors[k] = max(f.RecoveryFloors[k], *sub.Revision)
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	epoch, err := nativeRecoveryEpoch(root)
	if err != nil || epoch != c.Epoch || !time.Now().Before(evidence.ExpiresAt) || !time.Now().Before(c.ExpiresAt) {
		return ErrNativeRecovery
	}
	f.RecoveryReceipt = &nativeRecoveryReceipt{Challenge: *c, Body: bytes.Clone(body), Headers: headers, ExpiresAt: evidence.ExpiresAt}
	f.RecoveryChallenge = nil
	return fsutil.PersistJSONFile(s.path, f)
}

// The same strict verifier checks imports and stored bytes immediately before
// planning repair. A receipt is historical data until all current inputs match.
func validateNativeRecoveryEvidence(current NativeRecoveryChallenge, minRevisions map[string]int64, c *NativeRecoveryChallenge, key, body []byte, headers syncauth.Headers, now time.Time) (nativeRecoveryEvidence, error) {
	if len(body) == 0 || len(body) > MaxNativeRecoveryEvidenceBytes {
		return nativeRecoveryEvidence{}, ErrNativeRecovery
	}
	if c == nil || !now.Before(c.ExpiresAt) || c.Epoch != current.Epoch || c.Issuer != current.Issuer || c.KeyFingerprint != current.KeyFingerprint || c.AuthorityDigest != current.AuthorityDigest || !slices.Equal(c.Subjects, current.Subjects) || headers.EventType != "recovery.evidence" || headers.EventID != c.Nonce {
		return nativeRecoveryEvidence{}, ErrNativeRecovery
	}
	event, err := syncauth.Verify(key, headers, body, syncauth.Options{Now: func() time.Time { return now }})
	if err != nil {
		return nativeRecoveryEvidence{}, ErrNativeRecovery
	}
	var evidence nativeRecoveryEvidence
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if dec.Decode(&evidence) != nil || dec.Decode(new(any)) != io.EOF || evidence.Version != 1 || evidence.Issuer != c.Issuer || evidence.SystemID != c.SystemID || evidence.Nonce != c.Nonce || evidence.IssuedAt.Before(c.CreatedAt.Add(-time.Second)) || evidence.IssuedAt.After(now.Add(time.Second)) || !event.At.Equal(evidence.IssuedAt.Truncate(time.Second)) || !evidence.ExpiresAt.After(evidence.IssuedAt) || evidence.ExpiresAt.Sub(evidence.IssuedAt) > 5*time.Minute || !now.Before(evidence.ExpiresAt) || len(evidence.Subjects) != len(c.Subjects) {
		return nativeRecoveryEvidence{}, ErrNativeRecovery
	}
	seen := map[string]bool{}
	for _, sub := range evidence.Subjects {
		minimum, ok := minRevisions[sub.ID]
		if !ok || seen[sub.ID] || sub.Revision == nil || *sub.Revision <= 0 || *sub.Revision < minimum {
			return nativeRecoveryEvidence{}, ErrNativeRecovery
		}
		seen[sub.ID] = true
		var profile DirectoryUser
		var roles []json.RawMessage
		if json.Unmarshal(sub.Profile, &profile) != nil || profile.ID != sub.ID || profile.ExternalID != sub.ID || profile.Active == nil || json.Unmarshal(profile.Roles, &roles) != nil || roles == nil || *profile.Active && !directoryIdentifier(profile.UserName) {
			return nativeRecoveryEvidence{}, ErrNativeRecovery
		}
	}
	return evidence, nil
}
