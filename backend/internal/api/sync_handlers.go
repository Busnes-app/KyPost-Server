package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/Busness-app/ky-primitives/syncauth"

	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// maxSyncEventBytes bounds a directory event body. A SCIM User is a few
// hundred bytes.
const maxSyncEventBytes = 1 << 18

// handleSyncWebhook receives KySignOn's desired state for one subject: a
// SCIM User signed with syncauth, whose meta.version is a monotonic
// revision. It is applied behind the revision fence in sso.LifecycleStore,
// so a retry, a reordered delivery and a replayed capture all converge on
// what the directory said last.
//
// Every persistence error reaches the sender: a 5xx means retry, a 4xx means
// this will never work and an operator must look. Conflicts answer 422, not
// 409, because the sender counts 409 on user.created as success.
func (s *Server) handleSyncWebhook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSyncEventBytes+1))
	if err != nil || len(body) > maxSyncEventBytes {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	event, settings, ok := s.verifySyncEvent(r, body)
	if !ok {
		http.Error(w, "unauthorized sync request", http.StatusUnauthorized)
		return
	}
	if settings.IssuerURL == "" {
		http.Error(w, "Single Sign-On issuer is not configured", http.StatusServiceUnavailable)
		return
	}
	var u sso.DirectoryUser
	if err := json.Unmarshal(body, &u); err != nil {
		http.Error(w, "invalid SCIM User", http.StatusBadRequest)
		return
	}
	revision, err := u.Revision(event.Type)
	if err != nil {
		http.Error(w, "invalid directory event: "+err.Error(), http.StatusBadRequest)
		return
	}

	status, err := s.ssoLifecycle.ApplyDirectory(settings.IssuerURL, event, u.ID, revision, sso.EventDigest(event.Type, body), *u.Active, func() (bool, error) {
		return s.applyDirectoryUser(u)
	})
	var refusal *syncRefusal
	switch {
	case errors.Is(err, sso.ErrDirectoryConflict):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	case errors.As(err, &refusal):
		http.Error(w, refusal.Error(), refusal.status)
		return
	case err != nil:
		s.logger.Error("directory event was not applied", "event_id", event.ID, "error", err.Error())
		http.Error(w, "directory event was not applied", http.StatusInternalServerError)
		return
	}
	s.logger.Info("directory event "+status, "event_id", event.ID, "revision", strconv.FormatInt(revision, 10))
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "eventId": event.ID, "version": u.Meta.Version})
}

// verifySyncEvent checks the syncauth signature against the pairing secret,
// then the SSO client secret, so KySignOn can be paired with either, and
// returns the settings the event is applied under. The pairing secret is in
// memory and tried first, on its own, so an unauthenticated flood costs one
// HMAC and never a settings-file read. A key shorter than syncauth allows is
// skipped, never tried. The reason for a refusal goes to the log, where the
// operator pairing the two can read it.
func (s *Server) verifySyncEvent(r *http.Request, body []byte) (syncauth.Event, sso.SSOSettings, bool) {
	h := syncauth.FromRequest(r)
	verify := func(key string) (syncauth.Event, error) {
		if len(key) < syncauth.MinKeyBytes {
			return syncauth.Event{}, errors.New("no sync secret of usable length is configured")
		}
		return syncauth.Verify([]byte(key), h, body, syncauth.Options{})
	}
	ev, err := verify(s.pairingSecret)
	if err == nil {
		return ev, s.ssoStore.Load(), true
	}
	// Only a well-formed signature under some other key is worth a second
	// try; a request with no signature is refused without any disk read.
	if errors.Is(err, syncauth.ErrBadSignature) || len(s.pairingSecret) < syncauth.MinKeyBytes {
		settings := s.ssoStore.Load()
		if ev, err = verify(settings.ClientSecret); err == nil {
			return ev, settings, true
		}
	}
	s.logger.Info("directory event refused", "reason", err.Error())
	return syncauth.Event{}, sso.SSOSettings{}, false
}

// syncRefusal is an apply error with the status the sender should see.
type syncRefusal struct {
	status int
	err    error
}

func (e *syncRefusal) Error() string { return e.err.Error() }

// applyDirectoryUser makes the local account match u, and reports whether
// the change invalidates ID tokens issued before it. Nothing here erases
// data: a disabled or deleted user keeps their mailbox and keys and loses
// only access.
func (s *Server) applyDirectoryUser(u sso.DirectoryUser) (bool, error) {
	active := *u.Active
	role := users.RoleUser
	if active && sso.HasAdminRole(u.Roles) {
		role = users.RoleAdmin
	}

	existing, err := s.users.GetBySSOSub(u.ID)
	if errors.Is(err, users.ErrNotFound) {
		if !active {
			return false, nil // nothing to disable; the fence alone refuses a login
		}
		return false, s.provisionDirectoryUser(u, role)
	}
	if err != nil {
		return false, errors.New("failed to look up user")
	}

	if !active {
		if status, err := s.deactivateAndRevoke(existing.ID); err != nil {
			return false, &syncRefusal{status, err}
		}
		return true, nil
	}

	fence := false
	if existing.Role != role {
		// A demotion of the last active admin is refused, not swallowed: the
		// fence stays at the prior revision, so the same event applies once
		// the directory has promoted somebody else.
		if _, err := s.users.SetRole(existing.ID, role); err != nil {
			status, err := syncErrStatus(err, "failed to set role")
			return false, &syncRefusal{status, err}
		}
		fence = true
		s.revokeUserSessions(existing.ID, "")
	}
	if !existing.Active {
		if _, err := s.users.Reactivate(existing.ID); err != nil {
			return false, errors.New("failed to reactivate user")
		}
	}
	return fence, nil
}

// provisionDirectoryUser creates the account, under a distinct name when a
// local account already owns the directory's userName.
func (s *Server) provisionDirectoryUser(u sso.DirectoryUser, role users.Role) error {
	username := ssoUsername(u.UserName, u.ID)
	_, err := s.users.CreateSSOUser(username, role, u.ID, u.UserName, u.Email())
	if errors.Is(err, users.ErrUsernameTaken) {
		_, err = s.users.CreateSSOUser(ssoUsernameWithSuffix(username, u.ID), role, u.ID, u.UserName, u.Email())
	}
	if err != nil {
		status, err := syncErrStatus(err, "failed to create user")
		return &syncRefusal{status, err}
	}
	return nil
}

// deactivateAndRevoke runs the removal unconditionally rather than only when
// the local user still looks active.
//
// That is what makes a retry converge. If a previous delivery deactivated the
// user and then failed to revoke their credentials, the user is already
// inactive; a "deactivate only if active" guard would skip straight past the
// revocation that never happened and report success. Both steps are
// idempotent, so repeating them costs nothing and closes that gap.
func (s *Server) deactivateAndRevoke(userID string) (int, error) {
	deactivated, err := s.users.Deactivate(userID)
	if err != nil {
		return syncErrStatus(err, "failed to deactivate user")
	}
	if err := s.revokeAllUserCredentials(deactivated); err != nil {
		return http.StatusInternalServerError, errors.New("failed to revoke user credentials")
	}
	return 0, nil
}

// syncErrStatus separates "try again" from "this cannot be applied here".
func syncErrStatus(err error, what string) (int, error) {
	switch {
	case errors.Is(err, users.ErrLastActiveAdmin):
		// Retrying forever will not conjure a second administrator. Say so
		// once, loudly, so the sender stops and an operator intervenes.
		return http.StatusUnprocessableEntity, errors.New(what + ": refusing to remove the last active admin")
	case errors.Is(err, users.ErrUsernameTaken):
		return http.StatusUnprocessableEntity, errors.New(what + ": username already in use by a local account")
	case errors.Is(err, users.ErrUsernameInvalid):
		return http.StatusBadRequest, errors.New(what + ": username is not representable here")
	default:
		return http.StatusInternalServerError, errors.New(what)
	}
}
