//go:build linux

package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The single-tenant profile is byte-for-byte what it was before the hosted transport
// (RX PR 2): no hosted exit code, the same order and spacing. UPDATE_GOLDEN=1 rewrites it.
func TestSingleTenantReceivingConfigIsUnchanged(t *testing.T) {
	r, _ := receivingFixture(t)
	cert, key, leaf := receivingTestCertificate(t, t.TempDir())
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, spam := range []string{"", ` "{source_ip}" "{source_host}"`} {
		var out bytes.Buffer
		if err := r.writeConfig(context.Background(), executable, "127.0.0.1:2525", leaf.DNSNames[0], cert, key, spam, &out); err != nil {
			t.Fatal(err)
		}
		got := strings.NewReplacer(executable, "EXE", r.stateDir, "STATE", cert, "CERT", key, "KEY", leaf.DNSNames[0], "HOST").Replace(out.String())
		name := filepath.Join("testdata", "receiving-single.conf")
		if spam != "" {
			name = filepath.Join("testdata", "receiving-single-rspamd.conf")
		}
		if os.Getenv("UPDATE_GOLDEN") == "1" {
			if err := os.WriteFile(name, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(name)
		if err != nil || got != string(want) {
			t.Fatalf("%s changed (%v):\n%s", name, err, got)
		}
		if strings.Contains(got, "code 12") || strings.Contains(got, "code 13") {
			t.Fatal("single-tenant profile carries a hosted exit code")
		}
	}
}
