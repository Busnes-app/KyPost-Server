package api

import (
	"context"
	"net/http"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func (s *Server) handleNativeRecoveryRepair(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var credential nativeRecoveryCredential
	if !decodeNativeRecoveryRequest(w, r, 8192, &credential) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	operator, ok := s.confirmNativeRecoveryOperator(w, r, credential)
	if !ok {
		return
	}
	var plan []users.NativeAccountRepair
	err := s.withNativeRecoveryOperator(ctx, operator, func(ctx context.Context, settings sso.SSOSettings, all []users.User) error {
		var err error
		plan, err = s.ssoLifecycle.NativeRecoveryRepairPlanHeld(ctx, s.stateDir, settings, []byte(s.pairingSecret), all)
		return err
	})
	if err == nil {
		err = s.ssoStore.WithCurrentSettings(ctx, func(settings sso.SSOSettings) error {
			if settings != operator.settings {
				return sso.ErrNativeRecovery
			}
			release, err := s.ssoLifecycle.LockDirectoryContext(ctx)
			if err != nil {
				return err
			}
			defer release()
			return s.users.RepairNativeAccounts(ctx, settings.IssuerURL, plan, func(current, repaired []users.User) (func(), error) {
				release, err := s.lockNativeRecoveryOperator(operator, settings, current)
				if err != nil {
					return nil, err
				}
				// The users sink retains this session fence through its write.
				return release, s.ssoLifecycle.RecordNativeRecoveryRepairIntentHeld(ctx, s.stateDir, settings, []byte(s.pairingSecret), current, repaired)
			})
		})
	}
	if err == nil {
		// Account/session fences are released before cleanup reenters them.
		for _, repair := range plan {
			if err = ctx.Err(); err != nil {
				break
			}
			u, lookupErr := s.users.Get(repair.ID)
			if lookupErr != nil || u.NativeMailboxIssuer != operator.settings.IssuerURL || u.SSOSub != repair.Subject || u.NativeMailboxSource != repair.Source {
				err = sso.ErrNativeRecovery
				break
			}
			if err = s.revokeNativeRecoveryCredentials(u); err != nil {
				break
			}
		}
	}
	if err == nil {
		err = s.withNativeRecoveryOperator(ctx, operator, func(ctx context.Context, settings sso.SSOSettings, all []users.User) error {
			return s.ssoLifecycle.CompleteNativeRecoveryRepairHeld(ctx, s.stateDir, settings, []byte(s.pairingSecret), all)
		})
	}
	if err != nil {
		s.logger.Error("native recovery repair incomplete; restore remains held", "actor", operator.actor.ID)
		http.Error(w, "repair incomplete; preserve the hold, resolve storage or credential cleanup failures, sign in again and request fresh complete evidence", http.StatusConflict)
		return
	}
	s.logger.Info("native accounts repaired; restore remains held", "actor", operator.actor.ID)
	writeJSON(w, http.StatusOK, map[string]any{"accountsRepaired": true, "restoreHeld": true, "accounts": len(plan)})
}
