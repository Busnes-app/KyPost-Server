package sso

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"reflect"
	"slices"

	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"

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
	if receipt == nil || f.RecoveryRepair != nil {
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

// These barriers are ordering provenance, never current identity authority.
// They survive challenge invalidation and later offline restore epochs.
type nativeRecoveryRepairBarrier struct {
	Version  int    `json:"version"`
	Epoch    string `json:"epoch"`
	Nonce    string `json:"nonce"`
	Issuer   string `json:"issuer"`
	Subject  string `json:"subject"`
	Mailbox  string `json:"mailbox"`
	Source   string `json:"source"`
	Revision int64  `json:"revision"`
}

func (b nativeRecoveryRepairBarrier) matches(u users.User, revision int64) bool {
	nonce, nonceErr := hex.DecodeString(b.Nonce)
	return b.Version == 1 && recoveryEpochPattern.MatchString(b.Epoch) && len(b.Nonce) == 64 && nonceErr == nil && hex.EncodeToString(nonce) == b.Nonce && b.Revision == revision && revision > 0 && u.ID != "" && u.SSOSub != "" && u.NativeMailboxIssuer != "" && u.NativeMailboxSource != "" && b.Issuer == u.NativeMailboxIssuer && b.Subject == u.SSOSub && b.Mailbox == u.ID && b.Source == u.NativeMailboxSource
}

type nativeRecoveryRepair struct {
	Issuer         string     `json:"issuer"`
	Epoch          string     `json:"epoch"`
	Nonce          string     `json:"nonce"`
	KeyFingerprint string     `json:"keyFingerprint"`
	BeforeDigest   string     `json:"beforeDigest"`
	AfterDigest    string     `json:"afterDigest"`
	ReceiptDigest  string     `json:"receiptDigest"`
	ExpiresAt      time.Time  `json:"expiresAt"`
	CompletedAt    *time.Time `json:"completedAt,omitempty"`
}

func nativeRecoveryReceiptDigest(receipt *nativeRecoveryReceipt) (string, error) {
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// RecordNativeRecoveryRepairIntentHeld must run inside RepairNativeAccounts'
// beforeWrite callback under settings -> directory -> users -> session fences.
// The durable barriers publish BEFORE the users file, even if that write fails.
func (s *LifecycleStore) RecordNativeRecoveryRepairIntentHeld(ctx context.Context, root string, settings SSOSettings, key []byte, current, repaired []users.User) error {
	plan, err := s.NativeRecoveryRepairPlanHeld(ctx, root, settings, key, current)
	if err != nil {
		return err
	}
	if err = validateNativeRecoveryBatch(current, repaired, plan); err != nil {
		return err
	}
	before, minRevisions, _, err := s.nativeRecoveryInputs(root, settings, key, current)
	if err != nil {
		return err
	}
	after, _, _, err := s.nativeRecoveryInputs(root, settings, key, repaired)
	if err != nil || before.Epoch != after.Epoch || !slices.Equal(before.Subjects, after.Subjects) {
		return ErrNativeRecovery
	}
	f, err := s.load()
	if err != nil || f.RecoveryReceipt == nil || f.RecoveryRepair != nil {
		return ErrNativeRecovery
	}
	receipt := f.RecoveryReceipt
	// Reverify at the intent commit; planning alone has no continuing authority.
	evidence, err := validateNativeRecoveryEvidence(before, minRevisions, &receipt.Challenge, key, receipt.Body, receipt.Headers, time.Now().UTC())
	if err != nil || !receipt.ExpiresAt.Equal(evidence.ExpiresAt) {
		return ErrNativeRecovery
	}
	digest, err := nativeRecoveryReceiptDigest(receipt)
	if err != nil {
		return err
	}
	f.RecoveryRepair = &nativeRecoveryRepair{Issuer: before.Issuer, Epoch: before.Epoch, Nonce: receipt.Challenge.Nonce, KeyFingerprint: before.KeyFingerprint, BeforeDigest: before.AuthorityDigest, AfterDigest: after.AuthorityDigest, ReceiptDigest: digest, ExpiresAt: minRecoveryExpiry(receipt)}
	if f.RecoveryFloors == nil {
		f.RecoveryFloors = map[string]int64{}
	}
	if f.RecoveryRepairBarriers == nil {
		f.RecoveryRepairBarriers = map[string]nativeRecoveryRepairBarrier{}
	}
	bySubject := make(map[string]int64, len(evidence.Subjects))
	for _, subject := range evidence.Subjects {
		bySubject[subject.ID] = *subject.Revision
	}
	for _, repair := range plan {
		k := directoryKey(before.Issuer, repair.Subject)
		floor := max(f.RecoveryFloors[k], bySubject[repair.Subject])
		f.RecoveryFloors[k] = floor
		f.RecoveryRepairBarriers[k] = nativeRecoveryRepairBarrier{Version: 1, Epoch: before.Epoch, Nonce: receipt.Challenge.Nonce, Issuer: before.Issuer, Subject: repair.Subject, Mailbox: repair.ID, Source: repair.Source, Revision: floor}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	epoch, err := nativeRecoveryEpoch(root)
	if err != nil || epoch != before.Epoch || !time.Now().Before(f.RecoveryRepair.ExpiresAt) {
		return ErrNativeRecovery
	}
	return fsutil.PersistJSONFile(s.path, f)
}

func minRecoveryExpiry(r *nativeRecoveryReceipt) time.Time {
	if r.Challenge.ExpiresAt.Before(r.ExpiresAt) {
		return r.Challenge.ExpiresAt
	}
	return r.ExpiresAt
}

// Every change is compared against the freshly verified native-only plan.
func validateNativeRecoveryBatch(current, repaired []users.User, plan []users.NativeAccountRepair) error {
	if len(current) != len(repaired) {
		return ErrNativeRecovery
	}
	byID := make(map[string]users.NativeAccountRepair, len(plan))
	for _, repair := range plan {
		byID[repair.ID] = repair
	}
	for i, old := range current {
		want := old
		if repair, exists := byID[old.ID]; exists && (old.Active != repair.Active || old.Role != repair.Role) {
			if old.NativeSendEpoch == math.MaxUint64 {
				return ErrNativeRecovery
			}
			want.Active, want.Role = repair.Active, repair.Role
			want.NativeSendEpoch++
			want.UpdatedAt = repaired[i].UpdatedAt
			if _, err := time.Parse(time.RFC3339, want.UpdatedAt); err != nil {
				return ErrNativeRecovery
			}
			if old.Active != repair.Active {
				want.DeactivatedAt = ""
				if !repair.Active {
					want.DeactivatedAt = repaired[i].DeactivatedAt
					if _, err := time.Parse(time.RFC3339, want.DeactivatedAt); err != nil {
						return ErrNativeRecovery
					}
				}
			}
		}
		if !reflect.DeepEqual(want, repaired[i]) {
			return ErrNativeRecovery
		}
	}
	return nil
}

// Caller has completed strict native credential cleanup, released cleanup locks,
// then reacquired settings -> directory -> fresh users -> session fences. This
// records that one transition, never permanent readiness or permission to release.
func (s *LifecycleStore) CompleteNativeRecoveryRepairHeld(ctx context.Context, root string, settings SSOSettings, key []byte, accounts []users.User) error {
	current, revisions, _, err := s.nativeRecoveryInputs(root, settings, key, accounts)
	if err != nil {
		return err
	}
	f, err := s.load()
	if err != nil {
		return err
	}
	repair, receipt := f.RecoveryRepair, f.RecoveryReceipt
	if repair == nil || receipt == nil || repair.CompletedAt != nil || repair.Issuer != current.Issuer || repair.Epoch != current.Epoch || repair.KeyFingerprint != current.KeyFingerprint || repair.AfterDigest != current.AuthorityDigest || repair.BeforeDigest != receipt.Challenge.AuthorityDigest || repair.Nonce != receipt.Challenge.Nonce || !repair.ExpiresAt.Equal(minRecoveryExpiry(receipt)) || !time.Now().Before(repair.ExpiresAt) {
		return ErrNativeRecovery
	}
	digest, err := nativeRecoveryReceiptDigest(receipt)
	if err != nil || digest != repair.ReceiptDigest {
		return ErrNativeRecovery
	}
	// The explicit journal must prove EXACTLY the recorded resulting state.
	// Only then verify the original signed pre-repair authority and current key.
	before := current
	before.AuthorityDigest = repair.BeforeDigest
	evidence, err := validateNativeRecoveryEvidence(before, revisions, &receipt.Challenge, key, receipt.Body, receipt.Headers, time.Now().UTC())
	if err != nil || !receipt.ExpiresAt.Equal(evidence.ExpiresAt) {
		return ErrNativeRecovery
	}
	for _, u := range accounts {
		if u.NativeMailboxIssuer == "" && u.NativeMailboxSource == "" {
			continue
		}
		k := directoryKey(u.NativeMailboxIssuer, u.SSOSub)
		barrier := f.RecoveryRepairBarriers[k]
		if !barrier.matches(u, f.RecoveryFloors[k]) || barrier.Nonce != repair.Nonce || barrier.Epoch != repair.Epoch || s.ValidateNativeUserOwnership(root, u) != nil {
			return ErrNativeRecovery
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	epoch, err := nativeRecoveryEpoch(root)
	if err != nil || epoch != repair.Epoch || !time.Now().Before(repair.ExpiresAt) {
		return ErrNativeRecovery
	}
	now := time.Now().UTC()
	repair.CompletedAt = &now
	return fsutil.PersistJSONFile(s.path, f)
}
