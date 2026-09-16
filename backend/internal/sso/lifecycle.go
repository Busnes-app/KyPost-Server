package sso

import (
	"path/filepath"
	"time"

	"github.com/Busness-app/ky-primitives/oidcverify"

	"github.com/Busness-app/kypost-server/backend/internal/fsutil"
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
// process, but a replayed logout token or a login racing a logout must still
// be refused afterwards.
//
// ponytail: one JSON file under a file lock, like sso.json. It holds a few
// dozen entries pruned on every write; a database is not warranted.
type LifecycleStore struct {
	path string
}

type lifecycleFile struct {
	Logouts map[string]LogoutEvent `json:"logouts"`
}

// NewLifecycleStore returns the store backed by <configDir>/sso-lifecycle.json.
func NewLifecycleStore(configDir string) *LifecycleStore {
	return &LifecycleStore{path: filepath.Join(configDir, "sso-lifecycle.json")}
}

func (s *LifecycleStore) load() (lifecycleFile, error) {
	f := lifecycleFile{}
	err := fsutil.LoadJSONFile(s.path, func(v lifecycleFile) { f = v }, nil)
	if f.Logouts == nil {
		f.Logouts = map[string]LogoutEvent{}
	}
	return f, err
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
	err := fsutil.WithFileLock(s.path, func() error {
		f, err := s.load()
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
		return fsutil.PersistJSONFile(s.path, f)
	})
	return ev, fresh, err
}

// LoggedOut reports whether an accepted logout already covers a login with
// this identity, so the callback refuses to mint a session for a login the
// provider ended while the browser was still on its way back.
func (s *LifecycleStore) LoggedOut(id SessionIdentity) (bool, error) {
	f, err := s.load()
	if err != nil {
		return false, err
	}
	for _, ev := range f.Logouts {
		if ev.Covers(id) {
			return true, nil
		}
	}
	return false, nil
}
