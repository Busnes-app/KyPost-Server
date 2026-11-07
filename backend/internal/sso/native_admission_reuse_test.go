//go:build linux

package sso

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A successful admission must not turn the validated assignment into a grant
// that bypasses storage checks on the next call through the same store.
func TestNativeAdmissionRechecksValidatedStorage(t *testing.T) {
	for _, damage := range []string{"hold", "state", "mailbox", "ledger"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			f := newAddressFixture(t, "one")
			id := f.ids["one"]
			admit := func() error {
				a, err := f.life.AdmitNativeMailbox(ctx, f.root, nativeIssuer, id, id, f.accounts)
				if err == nil && (a.Owner.Mailbox != id || a.Source == "") {
					t.Fatal("admitted an unvalidated assignment")
				}
				return err
			}
			if err := admit(); err != nil {
				t.Fatal(err)
			}
			var absent string
			switch damage {
			case "hold":
				if err := os.WriteFile(filepath.Join(f.root, NativeRestoreHoldFile), []byte("malformed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "state":
				absent = filepath.Join(f.root, "users", id, "state.db")
			case "mailbox":
				absent = filepath.Join(f.root, "users", id, "mailbox", "mailbox.db")
			case "ledger":
				absent = f.life.nativePath()
			}
			if absent != "" {
				if err := os.Remove(absent); err != nil {
					t.Fatal(err)
				}
			}
			if err := admit(); err == nil {
				t.Fatal("prior admission bypassed changed storage")
			}
			if absent != "" {
				if _, err := os.Stat(absent); !os.IsNotExist(err) {
					t.Fatalf("recreated missing storage: %v", err)
				}
			}
		})
	}
}

func TestNativeAdmissionSeesIndependentOffboarding(t *testing.T) {
	ctx := context.Background()
	f := newAddressFixture(t, "one")
	id := f.ids["one"]
	if _, err := f.life.AdmitNativeMailbox(ctx, f.root, nativeIssuer, id, id, f.accounts); err != nil {
		t.Fatal(err)
	}
	other := NewLifecycleStore(f.config)
	nativeDesired(t, other, "one", "one@example.test", 2, false)
	if _, err := f.life.AdmitNativeMailbox(ctx, f.root, nativeIssuer, id, id, f.accounts); err == nil {
		t.Fatal("prior validated assignment survived offboarding by another store")
	}
}
