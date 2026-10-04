package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/Busnes-app/ky-primitives/syncauth"
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

// Confirmation precedes disk fences; recheck its account/settings witnesses and
// live session under settings -> directory -> users -> session commit fences.
func (s *Server) nativeRecoveryAdmin(w http.ResponseWriter, r *http.Request, credential nativeRecoveryCredential, action func(context.Context, sso.SSOSettings, []users.User) error) bool {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	ac, ok := authFromContext(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	sess, token, ok := s.sessionOf(r)
	if !ok || sess.UserID != ac.UserID {
		http.Error(w, "recovery requires a live administrator session", http.StatusUnauthorized)
		return false
	}
	settings := s.ssoStore.Load()
	var actor users.User
	err := s.users.WithCurrentUsers(r.Context(), func(all []users.User) error {
		for _, u := range all {
			if u.ID == ac.UserID {
				actor = u
				return nil
			}
		}
		return sso.ErrNativeRecovery
	})
	if err != nil || !actor.Active || actor.Role != users.RoleAdmin || actor.MustChangePassword {
		http.Error(w, "administrator authority unavailable; sign in again", http.StatusForbidden)
		return false
	}
	if !s.confirmActor(w, r, ac.UserID, credential.Password, credential.AuthSecret) {
		return false
	}
	err = s.ssoStore.WithCurrentSettings(ctx, func(current sso.SSOSettings) error {
		if current != settings {
			return sso.ErrNativeRecovery
		}
		release, err := s.ssoLifecycle.LockDirectoryContext(ctx)
		if err != nil {
			return err
		}
		defer release()
		return s.users.WithCurrentUsers(ctx, func(all []users.User) error {
			found := false
			for _, u := range all {
				if u.ID == actor.ID {
					found = u.Active && u.Role == users.RoleAdmin && !u.MustChangePassword && u.NativeSendEpoch == actor.NativeSendEpoch && u.PGPRevision == actor.PGPRevision && u.PasswordHash == actor.PasswordHash && u.AuthDerivation == actor.AuthDerivation && u.LoginSalt == actor.LoginSalt && u.LoginIterations == actor.LoginIterations && u.SSOSub == actor.SSOSub && u.SSOLinkRevokedAt == actor.SSOLinkRevokedAt
					break
				}
			}
			if !found {
				return sso.ErrNativeRecovery
			}
			s.sessMu.RLock()
			defer s.sessMu.RUnlock()
			live, exists := s.sessions[token]
			now := time.Now()
			if !exists || live.UserID != actor.ID || live.IssuedAt != sess.IssuedAt || live.SSO != sess.SSO || live.SSOKySignOn != sess.SSOKySignOn || !now.Before(live.ExpiresAt) || now.Sub(live.IssuedAt) >= sessionMaxLifetime {
				return sso.ErrNativeRecovery
			}
			if (live.SSO.Issuer != "" || live.SSOKySignOn) && (!current.Enabled || live.SSO.Issuer != current.IssuerURL || live.SSO.ClientID != current.ClientID || live.SSO.Subject != actor.SSOSub || actor.SSOSub == "" || actor.SSOLinkRevokedAt != 0 || !live.SSOAppAdmin) {
				return sso.ErrNativeRecovery
			}
			return action(ctx, current, all)
		})
	})
	if err != nil {
		http.Error(w, "recovery evidence refused; preserve the hold, sign in again and request fresh evidence for current authority", http.StatusConflict)
		return false
	}
	return true
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
