//go:build linux

package sso

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/oidcverify"

	"github.com/Busnes-app/kypost-server/backend/internal/sso/ssotest"
)

// A lifecycle-only write reads the ledger never, so a broken ledger does not
// stop logouts, as before the seam.
func TestAuthorityLifecycleWriteIgnoresLedger(t *testing.T) {
	f := newAddressFixture(t, "one")
	if err := os.WriteFile(f.life.nativePath(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	if _, fresh, err := f.life.RecordLogout("client", oidcverify.LogoutClaims{Issuer: nativeIssuer, Subject: "one", JWTID: "jti", IssuedAt: time.Now()}, until); err != nil || !fresh {
		t.Fatal("logout refused on a broken ledger", fresh, err)
	}
	if _, err := f.life.NativeAddress("one@example.test"); err == nil {
		t.Fatal("broken ledger read as an address")
	}
}

// The point reads answer exactly what the whole-ledger reads answer.
func TestAuthorityPointReadsMatchWholeReads(t *testing.T) {
	ctx := context.Background()
	f := newAddressFixture(t, "one", "two")
	one := f.ids["one"]
	if _, err := f.life.AddNativeAlias(ctx, f.root, one, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.life.CreateNativeMailbox(ctx, f.root, one, "box@example.test"); err != nil {
		t.Fatal(err)
	}
	all, err := f.life.NativeAddresses()
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"one@example.test", "sales@example.test", "box@example.test", "absent@example.test"} {
		if x, err := f.life.NativeAddress(address); err != nil || x != all[address] {
			t.Fatal(address, x, all[address], err)
		}
	}
	boxes, err := f.life.NativeMailboxes()
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{one, f.ids["two"], "nobody", ""} {
		want := []NativeMailbox{}
		for _, m := range boxes {
			if m.User == user {
				want = append(want, m)
			}
		}
		if got, err := f.life.NativeMailboxesOf(user); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatal(user, got, want, err)
		}
	}
}

func TestAuthorityFixturePaths(t *testing.T) {
	s := NewLifecycleStore(t.TempDir())
	dir := filepath.Dir(s.path)
	if ssotest.LifecyclePath(dir) != s.path || ssotest.LedgerPath(dir) != s.nativePath() || ssotest.DirectoryLockPath(dir) != s.path+".lock" {
		t.Fatal("ssotest authority paths drifted from the JSON backend")
	}
}
