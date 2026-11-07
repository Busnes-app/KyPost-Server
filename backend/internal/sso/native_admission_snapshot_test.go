//go:build linux

package sso

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

func TestNativeAdmissionSnapshotRechecksLifecycleFence(t *testing.T) {
	for _, damage := range []string{"uninitialized", "unreadable", "version-one"} {
		t.Run(damage, func(t *testing.T) {
			f := newAddressFixture(t, "one")
			id := f.ids["one"]
			admit := func() error {
				_, err := f.life.AdmitNativeMailbox(context.Background(), f.root, nativeIssuer, id, id, f.accounts)
				return err
			}
			if err := admit(); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "uninitialized":
				current, err := f.life.load()
				if err != nil {
					t.Fatal(err)
				}
				current.NativeProvisioningInitialized = false
				if err := fsutil.PersistJSONFile(f.life.path, current); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				if err := os.WriteFile(f.life.path, []byte("not-json"), 0600); err != nil {
					t.Fatal(err)
				}
			case "version-one":
				if err := WriteNativeV1ForTest(f.config, filepath.Join(f.config, "unused-relay-key")); err != nil {
					t.Fatal(err)
				}
			}
			err := admit()
			if err == nil {
				t.Fatal("prior admission bypassed changed lifecycle or storage format")
			}
			if damage == "version-one" && !errors.Is(err, ErrNativeMigration) {
				t.Fatalf("version-one admission error: %v", err)
			}
		})
	}
}
