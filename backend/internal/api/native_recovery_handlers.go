package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"io"
	"net/http"
	"time"
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

// handleNativeRecoveryStatus is a read-only, lock-free snapshot: reasons only,
// never evidence, digests or nonces. The release reverifies under its fences.
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
