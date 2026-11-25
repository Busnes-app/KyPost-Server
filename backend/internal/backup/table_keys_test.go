package backup

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
)

// MH5: a re-pin on host A leaves host B's own recovery.pub stale. The sync every sealing
// operation runs first replaces it from the sealed record with the new key; a record that does not open still
// refuses and leaves the file alone.
func TestTableRePinReplacesAnotherHostsStaleKey(t *testing.T) {
	a, b := tableBackupReplicas(t)
	totp, err := cryptutil.LoadOrCreateKey(filepath.Join(a.dirs.Secret, "totp-secret.key"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(b.dirs.Secret, "totp-secret.key"), []byte(base64.StdEncoding.EncodeToString(totp)), 0o600))
	first, second := newKey(t), newKey(t)
	must(t, pin(a, first, 2, 3))
	must(t, b.SyncRecoveryKey())
	pinKeys := []string{"kyrecovery_key_id", "kyrecovery_threshold", "kyrecovery_total_shares", anchorSetting}
	retired := map[string]string{}
	for _, k := range pinKeys {
		v, err := a.store.Setting(k)
		must(t, err)
		retired[k] = v
	}
	stalePub := recoveryclient.RecoveryKeyPath(b.dirs.State)
	stale, err := os.ReadFile(stalePub)
	must(t, err)
	// The operator's rotation on host A: clear the pin and its file, pin the new key.
	must(t, os.Remove(recoveryclient.RecoveryKeyPath(a.dirs.State)))
	for _, k := range pinKeys {
		must(t, a.store.DeleteSetting(k))
	}
	must(t, pin(a, second, 2, 3))
	// A database writer points the pin back at the retired key with no record: host b still
	// holds that key's file, but must not seal a fresh record that host a would then accept.
	current := map[string]string{}
	for _, k := range pinKeys {
		v, err := a.store.Setting(k)
		must(t, err)
		current[k] = v
	}
	for _, k := range pinKeys[:3] {
		must(t, a.store.SetSetting(k, retired[k]))
	}
	must(t, a.store.DeleteSetting(anchorSetting))
	if err := b.SyncRecoveryKey(); !errors.Is(err, ErrAnchor) {
		t.Fatal("a stale host recorded the retired pin", err)
	}
	_ = a.SyncRecoveryKey()
	if got, err := os.ReadFile(recoveryclient.RecoveryKeyPath(a.dirs.State)); err != nil || string(got) != string(second.Public().Bytes()) {
		t.Fatal("a rotated host moved back to the retired key", err)
	}
	for k, v := range current {
		must(t, a.store.SetSetting(k, v))
	}
	// Every sealing operation syncs first; the fixture's host B has no volumes.db to seal.
	if err := b.SyncRecoveryKey(); err != nil {
		t.Fatal("host b refused the re-pinned key", err)
	}
	f, err := b.snapshot()
	must(t, err)
	if k, err := recoveryclient.LoadRecoveryKey(b.dirs.State, f); err != nil || k.Public.ID() != second.Public().ID() {
		t.Fatal("host b would seal to another key", err)
	}
	// A database writer replays the retired first pin: neither host moves back to it.
	current = map[string]string{}
	for _, k := range pinKeys {
		v, err := a.store.Setting(k)
		must(t, err)
		current[k] = v
		must(t, a.store.SetSetting(k, retired[k]))
	}
	for name, host := range map[string]*Service{"a": a, "b": b} {
		if err := host.SyncRecoveryKey(); !errors.Is(err, ErrAnchor) {
			t.Fatalf("host %s accepted a replayed retired pin: %v", name, err)
		}
		if got, err := os.ReadFile(recoveryclient.RecoveryKeyPath(host.dirs.State)); err != nil || string(got) != string(second.Public().Bytes()) {
			t.Fatalf("host %s moved recovery.pub back to the retired key: %v", name, err)
		}
	}
	for k, v := range current {
		must(t, a.store.SetSetting(k, v))
	}
	sealed := current[anchorSetting]
	raw, _ := base64.StdEncoding.DecodeString(sealed)
	raw[len(raw)-1] ^= 1
	must(t, a.store.SetSetting(anchorSetting, base64.StdEncoding.EncodeToString(raw)))
	must(t, os.WriteFile(stalePub, stale, 0o600))
	if err := b.SyncRecoveryKey(); !errors.Is(err, ErrAnchor) {
		t.Fatal("an unopenable record replaced recovery.pub", err)
	}
	if got, err := os.ReadFile(stalePub); err != nil || string(got) != string(stale) {
		t.Fatal("a refused sync changed recovery.pub", err)
	}
}
