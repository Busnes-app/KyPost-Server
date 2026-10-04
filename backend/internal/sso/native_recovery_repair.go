package sso

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// NativeRecoveryRepairPlanHeld requalifies exact stored evidence against current
// authority and existing mailbox ownership. Caller holds settings -> directory ->
// users fences. Planning performs no write, credential cleanup or hold release.
func (s *LifecycleStore) NativeRecoveryRepairPlanHeld(ctx context.Context, root string, settings SSOSettings, key []byte, accounts []users.User) ([]users.NativeAccountRepair, error) {
	current, revisions, _, err := s.nativeRecoveryInputs(root, settings, key, accounts)
	if err != nil {
		return nil, err
	}
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	receipt := f.RecoveryReceipt
	if receipt == nil {
		return nil, ErrNativeRecovery
	}
	evidence, err := validateNativeRecoveryEvidence(current, revisions, &receipt.Challenge, key, receipt.Body, receipt.Headers, time.Now().UTC())
	if err != nil || !receipt.ExpiresAt.Equal(evidence.ExpiresAt) {
		return nil, ErrNativeRecovery
	}
	profiles := make(map[string]DirectoryUser, len(evidence.Subjects))
	for _, subject := range evidence.Subjects {
		var profile DirectoryUser
		if json.Unmarshal(subject.Profile, &profile) != nil {
			return nil, ErrNativeRecovery
		}
		profiles[subject.ID] = profile
	}
	var repairs []users.NativeAccountRepair
	for _, u := range accounts {
		if u.NativeMailboxIssuer == "" && u.NativeMailboxSource == "" {
			continue
		}
		profile, exists := profiles[u.SSOSub]
		if !exists || s.ValidateNativeUserOwnership(root, u) != nil {
			return nil, ErrNativeRecovery
		}
		role := users.RoleUser
		if *profile.Active && HasAdminRole(profile.Roles) {
			role = users.RoleAdmin
		}
		repairs = append(repairs, users.NativeAccountRepair{ID: u.ID, Subject: u.SSOSub, Source: u.NativeMailboxSource, Active: *profile.Active, Role: role})
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	// This component plans published accounts only; an empty result does not
	// certify unpublished reservations or current recovery readiness.
	if len(repairs) == 0 {
		return nil, ErrNativeRecovery
	}
	return repairs, nil
}
