package sso

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Busness-app/ky-primitives/syncauth"

	"github.com/Busness-app/kypost-server/backend/internal/fsutil"
)

// AdminAppRole is the KySignOn application role that makes a subject a
// KyPost administrator. Roles are scoped to this app at the provider, so no
// other name maps to it, and a central global admin never does.
const AdminAppRole = "kypost.admin"

// HasAdminRole reports whether a SCIM `roles` value names AdminAppRole. An
// entry may be a bare string or an object with a `value`; anything else is
// ignored rather than refused, so an unrelated role name neither grants
// admin nor blocks the event that carries it.
func HasAdminRole(raw json.RawMessage) bool {
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return false
	}
	for _, e := range entries {
		var name string
		var obj struct {
			Value string `json:"value"`
		}
		if json.Unmarshal(e, &name) != nil && json.Unmarshal(e, &obj) == nil {
			name = obj.Value
		}
		if name == AdminAppRole {
			return true
		}
	}
	return false
}

const scimUserSchema = "urn:ietf:params:scim:schemas:core:2.0:User"

// DirectoryUser is the versioned SCIM User a KySignOn directory event
// carries: the desired state of one subject, at one revision.
type DirectoryUser struct {
	Schemas    []string        `json:"schemas"`
	ID         string          `json:"id"`
	ExternalID string          `json:"externalId"`
	UserName   string          `json:"userName"`
	Active     *bool           `json:"active"`
	Roles      json.RawMessage `json:"roles"`
	Emails     []struct {
		Value   string `json:"value"`
		Primary bool   `json:"primary"`
	} `json:"emails"`
	Meta struct {
		Version string `json:"version"`
	} `json:"meta"`
}

func directoryIdentifier(s string) bool {
	return s != "" && len(s) <= 256 && strings.TrimSpace(s) == s &&
		!strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) })
}

// Revision validates the resource for an event of the given type and returns
// its revision, the n in `W/"n"`.
func (u DirectoryUser) Revision(eventType string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(u.Meta.Version, `W/"`), `"`), 10, 64)
	switch {
	case err != nil || n <= 0 || u.Meta.Version != fmt.Sprintf(`W/"%d"`, n):
		return 0, errors.New("meta.version is not a monotonic weak ETag")
	case len(u.Schemas) != 1 || u.Schemas[0] != scimUserSchema:
		return 0, errors.New("not a SCIM User")
	case !directoryIdentifier(u.ID) || u.ExternalID != u.ID:
		return 0, errors.New("id and externalId must be one valid identifier")
	case u.Active == nil:
		return 0, errors.New("active is required")
	case u.UserName != "" && !directoryIdentifier(u.UserName), *u.Active && u.UserName == "":
		return 0, errors.New("an active user needs a valid userName")
	}
	switch eventType {
	case "user.created", "user.updated":
	case "user.deleted":
		if *u.Active {
			return 0, errors.New("a deleted user cannot be active")
		}
	default:
		return 0, errors.New("unsupported event type")
	}
	return n, nil
}

// Email returns the primary address, else the first, else nothing.
func (u DirectoryUser) Email() string {
	for _, e := range u.Emails {
		if e.Primary {
			return e.Value
		}
	}
	if len(u.Emails) > 0 {
		return u.Emails[0].Value
	}
	return ""
}

// EventDigest identifies the exact resource an event delivered, so a retry
// is recognised and a different body under a reused event ID or revision is
// refused.
func EventDigest(eventType string, body []byte) string {
	sum := sha256.Sum256(append([]byte(eventType+"\n"), body...))
	return hex.EncodeToString(sum[:])
}

// DirectoryState is the directory's last applied word on one subject: the
// fence that refuses a stale event, and what a login checks before trusting
// a token for that subject.
type DirectoryState struct {
	Revision int64  `json:"revision"`
	Digest   string `json:"digest"`
	Active   bool   `json:"active"`
	EventID  string `json:"eventId"`
	// RevokedBefore fences ID tokens: one issued before it was minted under
	// access the directory has since changed, and is refused at sign-in.
	RevokedBefore int64 `json:"revokedBefore,omitempty"`
}

type directoryEvent struct {
	Issuer    string `json:"issuer"`
	Digest    string `json:"digest"`
	ExpiresAt int64  `json:"expiresAt"`
}

// ErrDirectoryConflict refuses an event the fence cannot order: a revision
// older than the one applied, or the same revision or event ID with a
// different body. The handler answers 422, because the sender counts 409 on
// user.created as success.
var ErrDirectoryConflict = errors.New("stale or conflicting directory event")

const (
	DirectoryApplied        = "applied"
	DirectoryAlreadyApplied = "already_applied"
)

func directoryKey(issuer, subject string) string { return issuer + "\x00" + subject }

// ApplyDirectory admits one verified directory event under the revision
// fence and, when it carries new state, runs apply and records the result.
//
// The order is deliberate: the fence and replay checks run first, apply
// mutates the account, and only then is the state written. A crash between
// the last two leaves the event unrecorded, so the sender's retry applies it
// again, every apply being idempotent, rather than being told it already
// happened. apply reports whether the change invalidates ID tokens issued
// before it, such as a role change or a deactivation.
func (s *LifecycleStore) ApplyDirectory(issuer string, ev syncauth.Event, subject string, revision int64, digest string, active bool, apply func() (fence bool, err error)) (string, error) {
	status := DirectoryAlreadyApplied
	err := fsutil.WithFileLock(s.path, func() error {
		f, err := s.load()
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		for id, e := range f.Events {
			if e.ExpiresAt < now {
				delete(f.Events, id)
			}
		}
		if seen, ok := f.Events[ev.ID]; ok {
			if seen.Issuer != issuer || seen.Digest != digest {
				return ErrDirectoryConflict
			}
			return nil
		}
		key := directoryKey(issuer, subject)
		prior := f.Directory[key]
		if revision < prior.Revision || (revision == prior.Revision && digest != prior.Digest) {
			return ErrDirectoryConflict
		}
		if revision > prior.Revision {
			fence, err := apply()
			if err != nil {
				return err
			}
			state := DirectoryState{Revision: revision, Digest: digest, Active: active, EventID: ev.ID, RevokedBefore: prior.RevokedBefore}
			if fence {
				state.RevokedBefore = max(prior.RevokedBefore, now)
			}
			f.Directory[key] = state
			status = DirectoryApplied
		}
		f.Events[ev.ID] = directoryEvent{Issuer: issuer, Digest: digest, ExpiresAt: ev.At.Add(syncauth.DefaultWindow).Unix()}
		return fsutil.PersistJSONFile(s.path, f)
	})
	return status, err
}

// Directory returns the last applied state for a subject, and whether the
// directory has ever spoken about it.
func (s *LifecycleStore) Directory(issuer, subject string) (DirectoryState, bool, error) {
	f, err := s.load()
	if err != nil {
		return DirectoryState{}, false, err
	}
	st, ok := f.Directory[directoryKey(issuer, subject)]
	return st, ok, nil
}
