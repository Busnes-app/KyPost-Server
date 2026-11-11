//go:build linux

package sso

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeStorageViewPreservesRevocationAndHoldRoles(t *testing.T) {
	f := newAddressFixture(t, "one")
	u, err := f.accounts.Get(f.ids["one"])
	if err != nil {
		t.Fatal(err)
	}
	if err := f.life.ValidateNativeUserStorage(f.root, u); err != nil {
		t.Fatal(err)
	}
	nativeDesired(t, NewLifecycleStore(f.config), "one", "one@example.test", 2, false)
	// Inactive users' storage must stay available to credential revocation.
	if err := f.life.ValidateNativeUserStorage(f.root, u); err != nil {
		t.Fatal("ownership read became access admission", err)
	}
	if _, err := f.life.AdmitNativeMailbox(context.Background(), f.root, nativeIssuer, u.ID, u.ID, f.accounts); err == nil {
		t.Fatal("decoded contents became a stale access grant")
	}
	if err := os.WriteFile(filepath.Join(f.root, NativeRestoreHoldFile), []byte("hold"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.life.ValidateNativeUserOwnership(f.root, u); err != nil {
		t.Fatal("hold must permit ownership-only revocation", err)
	}
	if err := f.life.ValidateNativeUserStorage(f.root, u); !errors.Is(err, ErrNativeRestoreHold) {
		t.Fatal("ordinary state access bypassed new hold", err)
	}
}
