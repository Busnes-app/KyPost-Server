package users

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

func recoveryNativeOwner(t *testing.T, s *Store, id, sub string) User {
	t.Helper()
	source := "native:" + strings.Repeat("ab", 32)
	u, err := s.PublishPreparedSSOUser(context.Background(), id, id, RoleAdmin, "https://identity.test", sub, id, id+"@example.test", func() (string, error) { return source, nil })
	if err != nil {
		t.Fatal(err)
	}
	// Populate opaque retained credential/key material without interpreting it.
	u, err = s.mutate(u.ID, func(u *User) error {
		u.PasswordHash = "retained-password-hash"
		u.PGPPrivateKeyWrapped = "retained-client-envelope"
		u.PGPPrivateKeyEnc = "retained-server-envelope"
		u.PGPRevision = 7
		u.RecoveryCodesHash = []string{"retained-recovery-code"}
		u.SSOLinkRevokedAt = 123
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestNativeRecoveryBatchPreservesOwnershipAndCredentials(t *testing.T) {
	s := newTestStore(t)
	one := recoveryNativeOwner(t, s, "native-one", "subject-one")
	two := recoveryNativeOwner(t, s, "native-two", "subject-two")
	legacy, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	plan := []NativeAccountRepair{
		{one.ID, one.SSOSub, one.NativeMailboxSource, false, RoleUser},
		{two.ID, two.SSOSub, two.NativeMailboxSource, true, RoleUser},
	}
	intentRecorded := false
	err = s.RepairNativeAccounts(context.Background(), one.NativeMailboxIssuer, plan, func(current, repaired []User) (func(), error) {
		// The persisted account file must still contain the entire old batch.
		persisted, err := s.readFileUnlocked()
		if err != nil || !reflect.DeepEqual(persisted.Users, current) {
			t.Fatal("accounts changed before intent", err)
		}
		for _, u := range repaired {
			if u.ID == one.ID && (u.Active || u.Role != RoleUser || u.NativeSendEpoch != one.NativeSendEpoch+1) {
				t.Fatal("incorrect proposed inactive authority")
			}
			if u.ID == two.ID && (!u.Active || u.Role != RoleUser || u.NativeSendEpoch != two.NativeSendEpoch+1) {
				t.Fatal("incorrect proposed demotion authority")
			}
		}
		intentRecorded = true
		return nil, nil
	})
	if err != nil || !intentRecorded {
		t.Fatal("repair refused", err)
	}
	for _, before := range legacy {
		after, err := s.Get(before.ID)
		if err != nil {
			t.Fatal(err)
		}
		if before.NativeMailboxIssuer == "" {
			if !reflect.DeepEqual(before, after) {
				t.Fatal("legacy account changed")
			}
			continue
		}
		// Compare every retained field, including all key/credential witnesses.
		before.Active, before.Role, before.UpdatedAt, before.DeactivatedAt, before.NativeSendEpoch = after.Active, after.Role, after.UpdatedAt, after.DeactivatedAt, after.NativeSendEpoch
		if !reflect.DeepEqual(before, after) {
			t.Fatal("repair changed non-access data")
		}
	}
}

func TestNativeRecoveryBatchRefusesWithoutChangingAccounts(t *testing.T) {
	s := newTestStore(t)
	u := recoveryNativeOwner(t, s, "native-one", "subject-one")
	valid := NativeAccountRepair{u.ID, u.SSOSub, u.NativeMailboxSource, false, RoleUser}
	before, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		issuer string
		plan   []NativeAccountRepair
	}{
		{"foreign issuer", "https://foreign.test", []NativeAccountRepair{valid}},
		{"duplicate", u.NativeMailboxIssuer, []NativeAccountRepair{valid, valid}},
		{"foreign source", u.NativeMailboxIssuer, []NativeAccountRepair{{u.ID, u.SSOSub, "native:foreign", false, RoleUser}}},
		{"foreign subject", u.NativeMailboxIssuer, []NativeAccountRepair{{u.ID, "foreign", u.NativeMailboxSource, false, RoleUser}}},
		{"legacy adoption", u.NativeMailboxIssuer, []NativeAccountRepair{{"missing", u.SSOSub, u.NativeMailboxSource, false, RoleUser}}},
		{"unknown role", u.NativeMailboxIssuer, []NativeAccountRepair{{u.ID, u.SSOSub, u.NativeMailboxSource, false, Role("owner")}}},
		{"empty", u.NativeMailboxIssuer, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.RepairNativeAccounts(context.Background(), tc.issuer, tc.plan, func([]User, []User) (func(), error) { t.Fatal("intent accepted invalid plan"); return nil, nil }); err == nil {
				t.Fatal("invalid plan accepted")
			}
			after, _ := os.ReadFile(s.path)
			if string(before) != string(after) {
				t.Fatal("refusal changed file")
			}
		})
	}
	failure := errors.New("intent persistence failed")
	if err = s.RepairNativeAccounts(context.Background(), u.NativeMailboxIssuer, []NativeAccountRepair{valid}, func([]User, []User) (func(), error) { return nil, failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err = s.RepairNativeAccounts(ctx, u.NativeMailboxIssuer, []NativeAccountRepair{valid}, func([]User, []User) (func(), error) { cancel(); return nil, nil }); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel after intent committed users", err)
	}
	after, _ := os.ReadFile(s.path)
	if string(before) != string(after) {
		t.Fatal("intent failure/cancellation changed file")
	}
}

func TestNativeRecoveryBatchUsesFreshDiskFence(t *testing.T) {
	s := newTestStore(t)
	u := recoveryNativeOwner(t, s, "native-one", "subject-one")
	release, err := fsutil.LockFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err = s.RepairNativeAccounts(ctx, u.NativeMailboxIssuer, []NativeAccountRepair{{u.ID, u.SSOSub, u.NativeMailboxSource, false, RoleUser}}, func([]User, []User) (func(), error) { t.Fatal("intent while disk lock blocked"); return nil, nil })
	release()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestNativeRecoveryBatchValidatesWholePlanBeforeIntent(t *testing.T) {
	s := newTestStore(t)
	u := recoveryNativeOwner(t, s, "native-one", "subject-one")
	before, _ := os.ReadFile(s.path)
	plan := []NativeAccountRepair{
		{u.ID, u.SSOSub, u.NativeMailboxSource, false, RoleUser},
		{"missing-owner", "missing-subject", u.NativeMailboxSource, false, RoleUser},
	}
	if err := s.RepairNativeAccounts(context.Background(), u.NativeMailboxIssuer, plan, func([]User, []User) (func(), error) {
		t.Fatal("intent persisted a partially valid batch")
		return nil, nil
	}); err == nil {
		t.Fatal("partially valid plan accepted")
	}
	after, _ := os.ReadFile(s.path)
	if string(before) != string(after) {
		t.Fatal("earlier valid account changed before late refusal")
	}
	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	legacy := all[0]
	for _, candidate := range all {
		if candidate.NativeMailboxIssuer == "" {
			legacy = candidate
			break
		}
	}
	if err := s.RepairNativeAccounts(context.Background(), u.NativeMailboxIssuer, []NativeAccountRepair{{legacy.ID, u.SSOSub, u.NativeMailboxSource, false, RoleUser}}, func([]User, []User) (func(), error) { t.Fatal("legacy adopted"); return nil, nil }); err == nil {
		t.Fatal("existing legacy owner adopted")
	}
}

func TestNativeRecoveryBatchEpochExhaustionAndUnchangedPlan(t *testing.T) {
	s := newTestStore(t)
	u := recoveryNativeOwner(t, s, "native-one", "subject-one")
	noop := []NativeAccountRepair{{u.ID, u.SSOSub, u.NativeMailboxSource, u.Active, u.Role}}
	before, _ := os.ReadFile(s.path)
	if err := s.RepairNativeAccounts(context.Background(), u.NativeMailboxIssuer, noop, func(current, repaired []User) (func(), error) {
		if !reflect.DeepEqual(current, repaired) {
			t.Fatal("unchanged repair advanced account authority")
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(s.path)
	if string(before) != string(after) {
		t.Fatal("unchanged repair changed persisted bytes")
	}
	f, err := s.readFileUnlocked()
	if err != nil {
		t.Fatal(err)
	}
	for i := range f.Users {
		if f.Users[i].ID == u.ID {
			f.Users[i].NativeSendEpoch = ^uint64(0)
		}
	}
	if err = fsutil.PersistJSONFile(s.path, f); err != nil {
		t.Fatal(err)
	}
	before, _ = os.ReadFile(s.path)
	noop[0].Active = false
	if err = s.RepairNativeAccounts(context.Background(), u.NativeMailboxIssuer, noop, func([]User, []User) (func(), error) {
		t.Fatal("intent before epoch exhaustion refusal")
		return nil, nil
	}); err == nil {
		t.Fatal("exhausted epoch accepted")
	}
	after, _ = os.ReadFile(s.path)
	if string(before) != string(after) {
		t.Fatal("epoch exhaustion changed persisted bytes")
	}
}

func TestNativeRecoveryBatchRetainsCommitFenceThroughWriteAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "commit"
		if fail {
			name = "intent-failure"
		}
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			u := recoveryNativeOwner(t, s, "native-one", "subject-one")
			failure := errors.New("intent failed")
			released := 0
			err := s.RepairNativeAccounts(context.Background(), u.NativeMailboxIssuer, []NativeAccountRepair{{u.ID, u.SSOSub, u.NativeMailboxSource, false, RoleUser}}, func([]User, []User) (func(), error) {
				release := func() {
					released++
					f, err := s.readFileUnlocked()
					if err != nil {
						t.Fatal(err)
					}
					for _, after := range f.Users {
						if after.ID == u.ID && after.Active != fail {
							t.Fatal("commit fence released before expected write/refusal")
						}
					}
				}
				if fail {
					return release, failure
				}
				return release, nil
			})
			if released != 1 || (err != nil) != fail {
				t.Fatal("commit fence not released exactly once", released, err)
			}
		})
	}
}
