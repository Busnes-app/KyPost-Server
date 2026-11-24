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

// MH5: a re-pin on host A leaves host B's own recovery.pub stale. B's next sealing replaces
// it from the sealed record and seals to the new key; a record that does not open still
// refuses and leaves the file alone.
func TestTableRePinReplacesAnotherHostsStaleKey(t *testing.T) {
	a, b := tableBackupReplicas(t)
	totp, err := cryptutil.LoadOrCreateKey(filepath.Join(a.dirs.Secret, "totp-secret.key"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(b.dirs.Secret, "totp-secret.key"), []byte(base64.StdEncoding.EncodeToString(totp)), 0o600))
	first, second := newKey(t), newKey(t)
	must(t, pin(a, first, 2, 3))
	must(t, b.SyncRecoveryKey())
	stalePub := recoveryclient.RecoveryKeyPath(b.dirs.State)
	stale, err := os.ReadFile(stalePub)
	must(t, err)
	// The operator's rotation on host A: clear the pin and its file, pin the new key.
	must(t, os.Remove(recoveryclient.RecoveryKeyPath(a.dirs.State)))
	for _, k := range []string{"kyrecovery_key_id", "kyrecovery_threshold", "kyrecovery_total_shares", anchorSetting} {
		must(t, a.store.DeleteSetting(k))
	}
	must(t, pin(a, second, 2, 3))
	_, manifest, err := b.Export(t.Context())
	if err != nil {
		t.Fatal("host b refused the re-pinned key", err)
	}
	if manifest.RecoveryKeyID != second.Public().ID() {
		t.Fatalf("host b sealed to %s, want %s", manifest.RecoveryKeyID, second.Public().ID())
	}
	sealed, err := a.store.Setting(anchorSetting)
	must(t, err)
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
