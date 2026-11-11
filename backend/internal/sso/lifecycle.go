package sso

import (
	"context"
	"encoding/json"
	"maps"
	"path/filepath"
	"time"

	"github.com/Busnes-app/ky-primitives/oidcverify"
)

// SessionIdentity is what a session remembers about the login that minted
// it: enough for a back-channel logout token to find it, and no more.
type SessionIdentity struct {
	Issuer    string
	ClientID  string
	Subject   string
	SessionID string
	IssuedAt  time.Time
}

// LogoutEvent is one accepted back-channel logout, kept as a durable replay
// record and as a fence against a login for the same session that is still
// in flight when the logout lands.
type LogoutEvent struct {
	Issuer      string `json:"issuer"`
	ClientID    string `json:"clientId"`
	Subject     string `json:"subject,omitempty"`
	SessionID   string `json:"sid,omitempty"`
	IssuedAt    int64  `json:"issuedAt"`
	RetainUntil int64  `json:"retainUntil"`
}

// Covers reports whether this logout ends the session with identity id.
//
// A token naming a session ends exactly that session; a subject check when
// the token carries one guards a provider that reuses sids across subjects.
// A token naming only a subject ends every session of that subject issued at
// or before the token, and never a later re-login. An unknown session id is
// never widened into a subject-wide logout.
func (e LogoutEvent) Covers(id SessionIdentity) bool {
	if id.Issuer != e.Issuer || id.ClientID != e.ClientID {
		return false
	}
	if e.SessionID != "" {
		return id.SessionID == e.SessionID && (e.Subject == "" || id.Subject == e.Subject)
	}
	return e.Subject != "" && id.Subject == e.Subject && !id.IssuedAt.After(time.Unix(e.IssuedAt, 0))
}

// LifecycleStore persists the KySignOn access-lifecycle facts that must
// outlive a restart: sessions themselves are in memory and die with the
// process, but a replayed logout token, a login racing a logout, and a
// stale directory event must still be refused afterwards.
//
// It sits with the native ledger behind authorityStore: native-authority.db
// at runtime (docs/NATIVE_AUTHORITY_PER_ACCOUNT.md), the v1/v2 JSON files
// only offline. path stays sso-lifecycle.json, whose ".lock" is the
// directory fence.
type LifecycleStore struct {
	path string
	// offline is an exclusive, stopped store (restore staging, migrate-native):
	// it also reads and writes the v1/v2 files and leaves no WAL after a write.
	offline bool
}

// staged is this store for offline work on stopped storage.
func (s *LifecycleStore) staged() *LifecycleStore {
	return &LifecycleStore{path: s.path, offline: true}
}

type lifecycleFile struct {
	RecoveryRepair                *nativeRecoveryRepair                  `json:"recoveryRepair,omitempty"`
	RecoveryRepairBarriers        map[string]nativeRecoveryRepairBarrier `json:"recoveryRepairBarriers,omitempty"`
	RecoveryChallenge             *NativeRecoveryChallenge               `json:"recoveryChallenge,omitempty"`
	RecoveryReceipt               *nativeRecoveryReceipt                 `json:"recoveryReceipt,omitempty"`
	RecoveryFloors                map[string]int64                       `json:"recoveryFloors,omitempty"`
	ReleaseFloors                 map[string]NativeReleaseFloor          `json:"releaseFloors,omitempty"`
	RestoreRelease                *NativeRestoreReleaseRecord            `json:"restoreRelease,omitempty"`
	NativeProvisioningInitialized bool                                   `json:"nativeProvisioningInitialized,omitempty"`
	Logouts                       map[string]LogoutEvent                 `json:"logouts"`
	Directory                     map[string]DirectoryState              `json:"directory"`
	Events                        map[string]directoryEvent              `json:"events"`
	// SignOns maps a spent native sign-on jti to its expiry (unix seconds).
	SignOns map[string]int64 `json:"signOns"`
}

// NewLifecycleStore returns the store backed by <configDir>/sso-lifecycle.json.
func NewLifecycleStore(configDir string) *LifecycleStore {
	return &LifecycleStore{path: filepath.Join(configDir, "sso-lifecycle.json")}
}

func (s *LifecycleStore) load() (f lifecycleFile, err error) {
	err = s.authority().View(context.Background(), func(r authorityReader) error {
		f, err = r.Lifecycle()
		return err
	})
	return f, err
}

func decodeLifecycle(raw []byte, f *lifecycleFile) error {
	var v lifecycleFile
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	*f = v
	return nil
}

// clone copies f one level deep: its maps and the records its pointers name.
// Values nested deeper (a DirectoryState's Resource, a record's slices) are
// shared; no caller edits them in place.
func (f lifecycleFile) clone() lifecycleFile {
	f.RecoveryRepair = clonePtr(f.RecoveryRepair)
	f.RecoveryChallenge = clonePtr(f.RecoveryChallenge)
	f.RecoveryReceipt = clonePtr(f.RecoveryReceipt)
	f.RestoreRelease = clonePtr(f.RestoreRelease)
	f.RecoveryRepairBarriers = maps.Clone(f.RecoveryRepairBarriers)
	f.RecoveryFloors = maps.Clone(f.RecoveryFloors)
	f.ReleaseFloors = maps.Clone(f.ReleaseFloors)
	f.Logouts = maps.Clone(f.Logouts)
	f.Directory = maps.Clone(f.Directory)
	f.Events = maps.Clone(f.Events)
	f.SignOns = maps.Clone(f.SignOns)
	return f
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}

func (f *lifecycleFile) ensureMaps() {
	if f.Logouts == nil {
		f.Logouts = map[string]LogoutEvent{}
	}
	if f.Directory == nil {
		f.Directory = map[string]DirectoryState{}
	}
	if f.Events == nil {
		f.Events = map[string]directoryEvent{}
	}
	if f.SignOns == nil {
		f.SignOns = map[string]int64{}
	}
}

// persistLifecycle writes a lifecycle the caller loaded under the directory lock.
func (s *LifecycleStore) persistLifecycle(f lifecycleFile) error {
	return s.authority().Update(context.Background(), func(w authorityWriter) error { return w.PutLifecycle(f) })
}

func logoutKey(issuer, clientID, jti string) string {
	return issuer + "\x00" + clientID + "\x00" + jti
}

// RecordLogout admits one verified logout token. It returns the event and
// true when the token is new, and false when its jti was already accepted:
// the caller must then refuse it, because a replay after the first delivery
// carries no new information and answering it revokes nothing.
//
// The record is retained through retainUntil, which the caller sets to cover
// both the token's own replay window and the longest login that could still
// complete after it.
func (s *LifecycleStore) RecordLogout(clientID string, c oidcverify.LogoutClaims, retainUntil time.Time) (LogoutEvent, bool, error) {
	ev := LogoutEvent{
		Issuer:      c.Issuer,
		ClientID:    clientID,
		Subject:     c.Subject,
		SessionID:   c.SessionID,
		IssuedAt:    c.IssuedAt.Unix(),
		RetainUntil: retainUntil.Unix(),
	}
	fresh := false
	err := s.update(func(w authorityWriter) error {
		f, err := w.Lifecycle()
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		for k, old := range f.Logouts {
			if old.RetainUntil < now {
				delete(f.Logouts, k)
			}
		}
		key := logoutKey(c.Issuer, clientID, c.JWTID)
		if _, seen := f.Logouts[key]; seen {
			return nil
		}
		f.Logouts[key] = ev
		fresh = true
		return w.PutLifecycle(f)
	})
	return ev, fresh, err
}

// RecordSignOnJTI spends a native sign-on token's jti. It returns false when
// the jti is already spent and unexpired. Durable, so a restart inside the
// token's window cannot replay it; expired entries are pruned on every write.
func (s *LifecycleStore) RecordSignOnJTI(issuer, clientID, jti string, expiresAt time.Time) (bool, error) {
	fresh := false
	err := s.update(func(w authorityWriter) error {
		f, err := w.Lifecycle()
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		for k, exp := range f.SignOns {
			if exp < now {
				delete(f.SignOns, k)
			}
		}
		key := logoutKey(issuer, clientID, jti)
		if _, seen := f.SignOns[key]; seen {
			return nil
		}
		f.SignOns[key] = expiresAt.Unix()
		fresh = true
		return w.PutLifecycle(f)
	})
	return fresh, err
}

// LoggedOut reports whether an accepted logout already covers a login with
// this identity, so the callback refuses to mint a session for a login the
// provider ended while the browser was still on its way back.
func (s *LifecycleStore) LoggedOut(id SessionIdentity) (bool, error) {
	var logouts map[string]LogoutEvent
	err := s.authority().View(context.Background(), func(r authorityReader) (err error) {
		logouts, err = r.Logouts()
		return err
	})
	if err != nil {
		return false, err
	}
	for _, ev := range logouts {
		if ev.Covers(id) {
			return true, nil
		}
	}
	return false, nil
}
