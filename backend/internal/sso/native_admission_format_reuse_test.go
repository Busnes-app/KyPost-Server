//go:build linux

package sso

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeAdmissionRechecksFormatBeforeDomainRead(t *testing.T) {
	for _, damage := range []string{"legacy-domain", "missing-domains", "bad-domains"} {
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
			case "legacy-domain":
				if err := os.WriteFile(filepath.Join(f.config, legacyNativeDomainFile), []byte(`{"domain":"example.test"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-domains":
				if err := os.Remove(filepath.Join(f.config, NativeDomainsFile)); err != nil {
					t.Fatal(err)
				}
			case "bad-domains":
				if err := os.WriteFile(filepath.Join(f.config, NativeDomainsFile), []byte(`{"version":99}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := admit(); err == nil {
				t.Fatal("prior admission bypassed changed domain format")
			}
		})
	}
}
