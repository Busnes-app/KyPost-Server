package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

const maxNativeRecoveryUploadBytes = 384 << 10

type nativeRecoveryCredential struct {
	Password   string `json:"password"`
	AuthSecret string `json:"authSecret"`
}

func decodeNativeRecoveryRequest(w http.ResponseWriter, r *http.Request, limit int64, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if dec.Decode(into) != nil || dec.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid recovery request; use the bounded documented JSON format", http.StatusBadRequest)
		return false
	}
	return true
}

type nativeRecoveryOperator struct {
	actor    users.User
	session  Session
	token    string
	settings sso.SSOSettings
}

// Confirmation happens once; later commits recheck its original witnesses.
func (s *Server) confirmNativeRecoveryOperator(w http.ResponseWriter, r *http.Request, credential nativeRecoveryCredential) (nativeRecoveryOperator, bool) {
	var operator nativeRecoveryOperator
	ac, ok := authFromContext(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return operator, false
	}
	operator.session, operator.token, ok = s.sessionOf(r)
	if !ok || operator.session.UserID != ac.UserID {
		http.Error(w, "recovery requires a live administrator session", http.StatusUnauthorized)
		return operator, false
	}
	operator.settings = s.ssoStore.Load()
	err := s.users.WithCurrentUsers(r.Context(), func(all []users.User) error {
		for _, u := range all {
			if u.ID == ac.UserID {
				operator.actor = u
				return nil
			}
		}
		return sso.ErrNativeRecovery
	})
	actor := operator.actor
	if err != nil || !actor.Active || actor.Role != users.RoleAdmin || actor.MustChangePassword || actor.NativeMailboxIssuer != "" || actor.NativeMailboxSource != "" {
		http.Error(w, "a usable legacy recovery administrator is required; sign in again", http.StatusForbidden)
		return operator, false
	}
	return operator, s.confirmActor(w, r, ac.UserID, credential.Password, credential.AuthSecret)
}

// Caller already holds settings -> directory -> users. The returned release
// keeps the live session proof fenced through the caller's actual durable write.
func (s *Server) lockNativeRecoveryOperator(operator nativeRecoveryOperator, current sso.SSOSettings, all []users.User) (func(), error) {
	actor := operator.actor
	if current != operator.settings {
		return nil, sso.ErrNativeRecovery
	}
	found := false
	for _, u := range all {
		if u.ID == actor.ID {
			found = u.Active && u.Role == users.RoleAdmin && !u.MustChangePassword && u.NativeMailboxIssuer == "" && u.NativeMailboxSource == "" && u.NativeSendEpoch == actor.NativeSendEpoch && u.PGPRevision == actor.PGPRevision && u.PasswordHash == actor.PasswordHash && u.AuthDerivation == actor.AuthDerivation && u.LoginSalt == actor.LoginSalt && u.LoginIterations == actor.LoginIterations && u.SSOSub == actor.SSOSub && u.SSOLinkRevokedAt == actor.SSOLinkRevokedAt
			break
		}
	}
	if !found {
		return nil, sso.ErrNativeRecovery
	}
	s.sessMu.RLock()
	live, exists := s.sessions[operator.token]
	now := time.Now()
	sess := operator.session
	if !exists || live.UserID != actor.ID || live.IssuedAt != sess.IssuedAt || live.SSO != sess.SSO || live.SSOKySignOn != sess.SSOKySignOn || !now.Before(live.ExpiresAt) || now.Sub(live.IssuedAt) >= sessionMaxLifetime || ((live.SSO.Issuer != "" || live.SSOKySignOn) && (!current.Enabled || live.SSO.Issuer != current.IssuerURL || live.SSO.ClientID != current.ClientID || live.SSO.Subject != actor.SSOSub || actor.SSOSub == "" || actor.SSOLinkRevokedAt != 0 || !live.SSOAppAdmin)) {
		s.sessMu.RUnlock()
		return nil, sso.ErrNativeRecovery
	}
	return s.sessMu.RUnlock, nil
}

func (s *Server) withNativeRecoveryOperator(ctx context.Context, operator nativeRecoveryOperator, action func(context.Context, sso.SSOSettings, []users.User) error) error {
	return s.ssoStore.WithCurrentSettings(ctx, func(current sso.SSOSettings) error {
		if current != operator.settings {
			return sso.ErrNativeRecovery
		}
		release, err := s.ssoLifecycle.LockDirectoryContext(ctx)
		if err != nil {
			return err
		}
		defer release()
		return s.users.WithCurrentUsers(ctx, func(all []users.User) error {
			release, err := s.lockNativeRecoveryOperator(operator, current, all)
			if err != nil {
				return err
			}
			defer release()
			return action(ctx, current, all)
		})
	})
}

func (s *Server) nativeRecoveryAdmin(w http.ResponseWriter, r *http.Request, credential nativeRecoveryCredential, action func(context.Context, sso.SSOSettings, []users.User) error) bool {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	operator, ok := s.confirmNativeRecoveryOperator(w, r, credential)
	if !ok {
		return false
	}
	if err := s.withNativeRecoveryOperator(ctx, operator, action); err != nil {
		http.Error(w, "recovery evidence refused; preserve the hold, sign in again and request fresh evidence for current authority", http.StatusConflict)
		return false
	}
	return true
}

// handleNativeRecoveryStatus changes no state and takes no locks (SQLite may
// create -shm/-wal companions beside mailbox databases). It returns reasons
// only, never evidence, digests or nonces; the release re-checks under fences.
func (s *Server) handleNativeRecoveryStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	all, err := s.users.List()
	if err != nil {
		http.Error(w, "account authority is unreadable; preserve it and repair", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, s.ssoLifecycle.NativeRestoreReleaseStatus(s.stateDir, config.SecretDir(), s.ssoStore.Load(), []byte(s.pairingSecret), all, time.Now().UTC()))
}

func (s *Server) handleNativeRecoveryChallenge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request struct {
		nativeRecoveryCredential
		SystemID       string `json:"systemId"`
		KeyFingerprint string `json:"keyFingerprint"`
	}
	if !decodeNativeRecoveryRequest(w, r, 8192, &request) {
		return
	}
	var challenge sso.NativeRecoveryChallenge
	if !s.nativeRecoveryAdmin(w, r, request.nativeRecoveryCredential, func(ctx context.Context, settings sso.SSOSettings, all []users.User) error {
		var err error
		challenge, err = s.ssoLifecycle.BeginNativeRecoveryHeld(ctx, s.stateDir, settings, []byte(s.pairingSecret), all, request.SystemID, request.KeyFingerprint)
		return err
	}) {
		return
	}
	ac, _ := authFromContext(r)
	s.logger.Info("native recovery challenge recorded", "actor", ac.UserID, "correlation_id", challenge.Epoch)
	writeJSON(w, 200, map[string]any{"challenge": challenge, "restoreHeld": true})
}
func (s *Server) handleNativeRecoveryEvidence(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request struct {
		nativeRecoveryCredential
		BodyBase64 string `json:"bodyBase64"`
		Signature  string `json:"signature"`
		Timestamp  string `json:"timestamp"`
		EventType  string `json:"eventType"`
		EventID    string `json:"eventId"`
	}
	if !decodeNativeRecoveryRequest(w, r, maxNativeRecoveryUploadBytes, &request) {
		return
	}
	body, err := base64.StdEncoding.Strict().DecodeString(request.BodyBase64)
	if err != nil || len(body) == 0 || len(body) > sso.MaxNativeRecoveryEvidenceBytes || base64.StdEncoding.EncodeToString(body) != request.BodyBase64 {
		http.Error(w, "invalid evidence; preserve exact provider bytes in canonical base64", http.StatusBadRequest)
		return
	}
	headers := syncauth.Headers{Signature: request.Signature, Timestamp: request.Timestamp, EventType: request.EventType, EventID: request.EventID}
	if !s.nativeRecoveryAdmin(w, r, request.nativeRecoveryCredential, func(ctx context.Context, settings sso.SSOSettings, all []users.User) error {
		return s.ssoLifecycle.AcceptNativeRecoveryHeld(ctx, s.stateDir, settings, []byte(s.pairingSecret), all, body, headers)
	}) {
		return
	}
	ac, _ := authFromContext(r)
	s.logger.Info("native recovery evidence recorded; restore remains held", "actor", ac.UserID)
	writeJSON(w, 200, map[string]any{"evidenceRecorded": true, "restoreHeld": true, "accountsRepaired": false})
}

// nativeReleaseConfirm is the Maddy confirmation (owner decision Q3): Maddy has
// no fence, so the operator states the original host can no longer receive.
const nativeReleaseConfirm = "original-host-decommissioned"

// handleNativeRecoveryRelease releases a native restore hold (P1-P9 in
// docs/NATIVE_RESTORE_RELEASE.md). Fences: domain -> settings -> directory ->
// users -> session, held from the precondition checks through the rename.
func (s *Server) handleNativeRecoveryRelease(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	enabled, err := config.NativeRestoreReleaseEnabled()
	if err != nil {
		http.Error(w, "KYPOST_NATIVE_RESTORE_RELEASE must be true or false", http.StatusConflict)
		return
	}
	if !enabled {
		http.Error(w, "native restore hold release is off; set KYPOST_NATIVE_RESTORE_RELEASE=true to enable it", http.StatusNotFound)
		return
	}
	var request struct {
		nativeRecoveryCredential
		Confirm string `json:"confirm"`
	}
	if !decodeNativeRecoveryRequest(w, r, 8192, &request) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	r = r.WithContext(ctx)
	// P6 (owner decision Q2): only a password session of an unlinked local
	// administrator, checked before step-up so no SSO challenge is minted. A
	// linked one could be deactivated by the release itself (decision a).
	const p6 = "P6: hold release requires a password session of an active local administrator without an SSO link"
	if sess, _, ok := s.sessionOf(r); ok && (sess.SSO.Issuer != "" || sess.SSOKySignOn) {
		http.Error(w, p6, http.StatusForbidden)
		return
	}
	operator, ok := s.confirmNativeRecoveryOperator(w, r, request.nativeRecoveryCredential)
	if !ok {
		return
	}
	actor := operator.actor
	if operator.session.SSO.Issuer != "" || operator.session.SSOKySignOn || actor.SSOSub != "" || actor.PasswordHash == "" {
		http.Error(w, p6, http.StatusForbidden)
		return
	}
	if s.backup == nil {
		http.Error(w, "audit log unavailable: the state store did not open; the hold stays", http.StatusServiceUnavailable)
		return
	}
	// Reasons name preconditions, account/mailbox IDs, subjects and domains;
	// never evidence, digests, nonces or credentials (LOGGING.md).
	refuse := func(reasons ...string) {
		s.logger.Info("native restore hold release refused; hold stays", "actor", actor.ID, "reason", strings.Join(reasons, "; "))
		writeJSON(w, http.StatusConflict, map[string]any{"error": "release refused; the hold stays", "reasons": reasons, "restoreHeld": true})
	}
	if record, released, err := s.ssoLifecycle.NativeRestoreReleased(s.stateDir); err != nil {
		refuse("a released marker exists but does not match the recorded release")
		return
	} else if released {
		s.completeNativeRelease(w, actor.ID, record, true)
		return
	}
	if _, err := os.Lstat(filepath.Join(s.stateDir, sso.NativeRestoreHoldFile)); errors.Is(err, os.ErrNotExist) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "no native restore hold: nothing to release", "restoreHeld": false})
		return
	}
	maddy, err := maddyProfile(s.configDir)
	if err != nil {
		refuse("the receiver configuration cannot be checked; preserve it and retry")
		return
	}
	if maddy && request.Confirm != nativeReleaseConfirm {
		refuse(`the bundled receiver (Maddy) has no fence: stop the original host's receiver and send "confirm": "` + nativeReleaseConfirm + `"`)
		return
	}
	// P8: fresh DNS proof before any fence; DNS never runs under the directory lock.
	set, err := s.nativeDomains.ReadSet()
	if err != nil {
		refuse("P8: mail domain set is unreadable")
		return
	}
	var proofs []sso.NativeDomain
	for _, name := range slices.Sorted(maps.Keys(set.Domains)) {
		if proof, err := s.nativeDomains.VerifyDomain(ctx, name); err == nil {
			proofs = append(proofs, proof)
		}
	}
	var plan *sso.NativeRestoreReleasePlan
	var refused *sso.NativeRestoreRefusedError
	errAudit := errors.New("release audit unavailable")
	err = func() error {
		release, err := fsutil.LockFileContext(ctx, filepath.Join(s.configDir, sso.NativeDomainsFile))
		if err != nil {
			return err
		}
		defer release()
		current, err := s.nativeDomains.ReadSet()
		if err != nil || !slices.ContainsFunc(proofs, current.CurrentProof) {
			return &sso.NativeRestoreRefusedError{Reasons: []string{"P8: no configured mail domain has a fresh DNS proof; publish the challenge record and retry"}}
		}
		return s.ssoStore.WithCurrentSettings(ctx, func(settings sso.SSOSettings) error {
			if settings != operator.settings {
				return sso.ErrNativeRecovery
			}
			release, err := s.ssoLifecycle.LockDirectoryContext(ctx)
			if err != nil {
				return err
			}
			defer release()
			return s.users.DeactivateForRestoreRelease(ctx, func(all []users.User) ([]string, []string, func(), error) {
				release, err := s.lockNativeRecoveryOperator(operator, settings, all)
				if err != nil {
					return nil, nil, nil, err
				}
				if plan, err = s.ssoLifecycle.PlanNativeRestoreReleaseHeld(s.stateDir, settings, []byte(s.pairingSecret), all, actor.ID, time.Now().UTC()); err != nil {
					return nil, nil, release, err
				}
				rec := plan.Record
				if err = s.backup.Audit("admin.native_restore_release", actor.ID, rec.Epoch, "started", map[string]any{"deactivate": rec.Deactivated, "demote": rec.Demoted}); err != nil {
					return nil, nil, release, errAudit
				}
				if err = plan.RecordIntent(ctx); err == nil {
					err = s.releaseHit("intent")
				}
				return rec.Deactivated, rec.Demoted, release, err
			}, func() error { return plan.Commit(ctx, s.nativeReleaseHit) })
		})
	}()
	switch {
	case errors.As(err, &refused):
		refuse(refused.Reasons...)
		return
	case errors.Is(err, errAudit):
		s.logger.Error("native restore hold release audit unavailable; nothing written", "actor", actor.ID)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "audit log unavailable; nothing was written and the hold stays. Retry", "restoreHeld": true})
		return
	case err != nil && plan == nil:
		refuse("current authority changed or is unreadable; sign in again and retry")
		return
	case errors.Is(err, sso.ErrNativeReleaseUnconfirmed):
		// The rename happened: the host is released. The retry confirms it
		// and writes the completion audit.
		s.revokeReleasedSessions(plan.Record)
		s.logger.Error("native restore hold released; durability unconfirmed", "actor", actor.ID, "correlation_id", plan.Record.Epoch)
		writeJSON(w, http.StatusOK, map[string]any{"released": true, "confirmed": false, "epoch": plan.Record.Epoch, "error": "the hold is released but its durability is unconfirmed; repeat the request to confirm"})
		return
	case err != nil:
		s.logger.Error("native restore hold release failed after its intent; hold stays", "actor", actor.ID, "correlation_id", plan.Record.Epoch)
		_ = s.backup.Audit("admin.native_restore_release", actor.ID, plan.Record.Epoch, "failed", nil)
		writeJSON(w, http.StatusConflict, map[string]any{"error": "release interrupted; the hold stays. Request a new challenge, import fresh evidence, repair and release again", "restoreHeld": true})
		return
	}
	s.completeNativeRelease(w, actor.ID, plan.Record, false)
}

// maddyProfile detects the bundled receiver as receiving does: the supervised
// flag or a rendered config (Q3). An unreadable config state fails closed.
func maddyProfile(configDir string) (bool, error) {
	_, err := os.Lstat(filepath.Join(configDir, "receiving.conf"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	return os.Getenv("KYPOST_NATIVE_RECEIVER") == "true" || err == nil, nil
}

func (s *Server) releaseHit(point string) error {
	if s.nativeReleaseHit == nil {
		return nil
	}
	return s.nativeReleaseHit(point)
}

// revokeReleasedSessions signs out every account the release changed (a).
func (s *Server) revokeReleasedSessions(record sso.NativeRestoreReleaseRecord) {
	for _, id := range append(slices.Clone(record.Deactivated), record.Demoted...) {
		s.revokeUserSessions(id, "")
	}
}

// completeNativeRelease audits completion under the original release's actor
// and time, then records it; a retry after the rename lands here too and only
// records what is missing.
func (s *Server) completeNativeRelease(w http.ResponseWriter, operator string, record sso.NativeRestoreReleaseRecord, already bool) {
	s.revokeReleasedSessions(record)
	if record.CompletedAt == nil {
		details := map[string]any{"deactivated": record.Deactivated, "demoted": record.Demoted, "releasedAt": record.At, "confirmedBy": operator}
		if err := s.backup.Audit("admin.native_restore_release", record.Actor, record.Epoch, "completed", details); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the hold is released but the completion audit failed; repeat the request to record it", "released": true})
			return
		}
		if err := s.ssoLifecycle.CompleteNativeRestoreRelease(s.stateDir); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the hold is released but its completion was not recorded; repeat the request", "released": true})
			return
		}
	}
	result := "released"
	if already {
		result = "already_released"
	}
	s.logger.Info("native restore hold released", "actor", operator, "correlation_id", record.Epoch, "result", result)
	writeJSON(w, http.StatusOK, map[string]any{"released": true, "confirmed": true, "alreadyReleased": already, "epoch": record.Epoch, "deactivatedAccounts": len(record.Deactivated), "demotedAccounts": len(record.Demoted),
		"nextSteps": []string{"Run a KyIdentity resync now.", "Restart the container to start receiving.", "Cloudflare receiving stays fenced until kypost-server receiving cloudflare takeover."}})
}
