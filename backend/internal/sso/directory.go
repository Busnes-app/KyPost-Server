package sso

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Busnes-app/ky-primitives/syncauth"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
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

// directoryDemoted is an active resource without the administrator role, the
// only state that clears legacyMixedUse. Deactivation is not demotion.
func directoryDemoted(u DirectoryUser) bool {
	return u.Active != nil && *u.Active && !HasAdminRole(u.Roles)
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
	// Resource retains the supported verified SCIM fields for later repair.
	// Nil on legacy records: never infer missing desired state from a login.
	Resource *DirectoryUser `json:"resource,omitempty"`
	Revision int64          `json:"revision"`
	Digest   string         `json:"digest"`
	Active   bool           `json:"active"`
	EventID  string         `json:"eventId"`
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
	return s.applyDirectory(issuer, ev, subject, revision, digest, active, nil, apply)
}

// ApplyDirectoryUser is called only after transport signature verification.
// Desired resource and the access/replay fence publish in the same write.
// Unknown SCIM extensions are not captured; emails are not inferred aliases.
func (s *LifecycleStore) ApplyDirectoryUser(issuer string, ev syncauth.Event, resource DirectoryUser, digest string, apply func() (bool, error)) (string, error) {
	revision, err := resource.Revision(ev.Type)
	if err != nil {
		return "", err
	}
	return s.applyDirectory(issuer, ev, resource.ID, revision, digest, *resource.Active, &resource, apply)
}

func (s *LifecycleStore) applyDirectory(issuer string, ev syncauth.Event, subject string, revision int64, digest string, active bool, resource *DirectoryUser, apply func() (bool, error)) (string, error) {
	status := DirectoryAlreadyApplied
	err := fsutil.WithFileLock(s.path, func() error {
		f, err := s.load()
		if err != nil {
			return err
		}
		key := directoryKey(issuer, subject)
		if floor, held := f.RecoveryFloors[key]; held && revision <= floor {
			return ErrDirectoryConflict
		}
		if floor, held := f.ReleaseFloors[key]; held && floor.refuses(revision, active) {
			return ErrDirectoryConflict
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
		prior := f.Directory[key]
		if revision < prior.Revision || (revision == prior.Revision && digest != prior.Digest) {
			return ErrDirectoryConflict
		}
		if revision > prior.Revision {
			fence, err := apply()
			if err != nil {
				return err
			}
			state := DirectoryState{Resource: resource, Revision: revision, Digest: digest, Active: active, EventID: ev.ID, RevokedBefore: prior.RevokedBefore}
			if fence {
				state.RevokedBefore = max(prior.RevokedBefore, now)
			}
			if err := s.syncNativeDirectory(key, state); err != nil {
				return err
			}
			f.Directory[key] = state
			if f.RecoveryRepair != nil && f.RecoveryRepair.Issuer == issuer {
				f.RecoveryRepair = nil
			}
			if f.RecoveryReceipt != nil && f.RecoveryReceipt.Challenge.Issuer == issuer {
				f.RecoveryReceipt = nil
			}
			if f.RecoveryChallenge != nil && f.RecoveryChallenge.Issuer == issuer {
				f.RecoveryChallenge = nil
			}
			status = DirectoryApplied
		}
		f.Events[ev.ID] = directoryEvent{Issuer: issuer, Digest: digest, ExpiresAt: ev.At.Add(syncauth.DefaultWindow).Unix()}
		return fsutil.PersistJSONFile(s.path, f)
	})
	return status, err
}

// syncNativeDirectory runs under the directory lock before the revision is
// recorded: a demotion clears legacyMixedUse, and every ledger address is set
// to its desired state with a generation bump (commitNative), its route
// written inactive. Any load or write failure fails the event, so the sender
// retries it and the rule converges.
func (s *LifecycleStore) syncNativeDirectory(key string, d DirectoryState) error {
	// While version-1 domain data awaits migration, directory sync must keep
	// working, IMAP deployments included: migration recomputes the flag from
	// the directory resource recorded here, so the demotion still lands. Any
	// other load failure (a lost or corrupt ledger) fails the event.
	if _, v1, err := legacyNativeDomain(filepath.Dir(s.path)); err == nil && v1 {
		return nil
	}
	f, err := s.loadNative()
	if err != nil {
		return err
	}
	if len(f.Accounts) == 0 {
		return nil
	}
	if a := f.Accounts[key]; a.LegacyMixedUse && d.Resource != nil && directoryDemoted(*d.Resource) {
		a.LegacyMixedUse = false
		f.Accounts[key] = a
	}
	return s.commitNative(f, map[string]DirectoryState{key: d})
}

// LockDirectory holds the lock ApplyDirectory applies under, so a caller can
// read Directory and act on the answer before any directory event lands. The
// caller must defer release and must not call a recording method meanwhile:
// the lock is not reentrant. It is a flock shared by every lifecycle write in
// every process, so hold it briefly: local disk work only, never network I/O.
func (s *LifecycleStore) LockDirectory() (release func(), err error) {
	return fsutil.LockFile(s.path)
}

func (s *LifecycleStore) LockDirectoryContext(ctx context.Context) (release func(), err error) {
	return fsutil.LockFileContext(ctx, s.path)
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

// NativeDirectorySubjects supplies retained signed resources for startup repair.
// It grants no access: each repair reloads the resource under its directory fence.
func (s *LifecycleStore) NativeDirectorySubjects(issuer string) ([]string, error) {
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	var subjects []string
	for key, d := range f.Directory {
		if d.Resource != nil && key == directoryKey(issuer, d.Resource.ID) {
			subjects = append(subjects, d.Resource.ID)
		}
	}
	return subjects, nil
}
