package cfreceiving

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
)

// MH7: in table mode (provisioned keys active), Cloudflare receiving key files are
// operator-provisioned read-only keys; Lock and save must refuse to write to host disk.
func TestKeysRefuseWriteWhenProvisioned(t *testing.T) {
	dir := t.TempDir()
	unprovision := cryptutil.ProvisionKeys(dir)
	defer unprovision()

	keys := Keys{Dir: dir}
	if _, err := keys.Lock(context.Background()); !errors.Is(err, cryptutil.ErrKeyNotProvisioned) {
		t.Fatalf("Lock in table mode: got %v, want ErrKeyNotProvisioned", err)
	}
	if err := keys.save(CredentialsFile, Material{Epoch: 1}); !errors.Is(err, cryptutil.ErrKeyNotProvisioned) {
		t.Fatalf("save credentials in table mode: got %v, want ErrKeyNotProvisioned", err)
	}
	if err := keys.save(HostFile, hostRecord{Live: "e1"}); !errors.Is(err, cryptutil.ErrKeyNotProvisioned) {
		t.Fatalf("save host file in table mode: got %v, want ErrKeyNotProvisioned", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("refused writes left host files: entries=%v err=%v", entries, err)
	}
}
