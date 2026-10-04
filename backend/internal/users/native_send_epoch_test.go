package users

import (
	"context"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeSendEpochFencesLegacyAdministratorAcrossStores(t *testing.T) {
	store := newTestStore(t)
	u, err := store.Create(context.Background(), "operator", "long-test-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	other, err := OpenExisting(context.Background(), filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := other.Deactivate(u.ID)
	if err != nil || disabled.NativeSendEpoch != u.NativeSendEpoch+1 {
		t.Fatal("deactivation witness", err)
	}
	restored, err := store.Reactivate(u.ID)
	if err != nil || restored.NativeSendEpoch != u.NativeSendEpoch+2 {
		t.Fatal("reactivation witness", err)
	}
	profile, err := other.mutate(u.ID, func(u *User) error { u.Username = "operator-renamed"; return nil })
	if err != nil || profile.NativeSendEpoch != restored.NativeSendEpoch {
		t.Fatal("profile changed authority epoch", err)
	}
	role, err := store.SetRole(u.ID, RoleUser)
	if err != nil || role.NativeSendEpoch != profile.NativeSendEpoch+1 {
		t.Fatal("role witness", err)
	}
	credential, err := other.SetPassword(context.Background(), u.ID, "replacement-test-password", false, nil)
	if err != nil || credential.NativeSendEpoch != role.NativeSendEpoch+1 {
		t.Fatal("credential witness", err)
	}
	mfa, err := store.SetPendingTOTPSecret(u.ID, "sealed-test-secret")
	if err != nil || mfa.NativeSendEpoch != credential.NativeSendEpoch+1 {
		t.Fatal("MFA witness", err)
	}
}
func TestNativeSendEpochOverflowRefusesLegacyMutation(t *testing.T) {
	store := newTestStore(t)
	all, err := store.readFileUnlocked()
	if err != nil {
		t.Fatal(err)
	}
	all.Users[0].NativeSendEpoch = math.MaxUint64
	if err = fsutil.PersistJSONFile(store.path, all); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(store.path)
	if _, err = store.ClearMustChangePassword(all.Users[0].ID); err == nil {
		t.Fatal("epoch overflow permitted")
	}
	after, _ := os.ReadFile(store.path)
	if string(before) != string(after) {
		t.Fatal("overflow mutation committed")
	}
}
