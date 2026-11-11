//go:build linux

package sso

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/oidcverify"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/sso/ssotest"
)

// A lifecycle-only write reads the ledger never, so a broken ledger does not
// stop logouts, as before the seam.
func TestAuthorityLifecycleWriteIgnoresLedger(t *testing.T) {
	f := newAddressFixture(t, "one")
	editLedgerForTest(t, f.life, func(l *nativeLedger) {
		x := l.Addresses["one@example.test"]
		x.History = nil // breaks validHistory
		l.Addresses["one@example.test"] = x
	})
	until := time.Now().Add(time.Hour)
	if _, fresh, err := f.life.RecordLogout("client", oidcverify.LogoutClaims{Issuer: nativeIssuer, Subject: "one", JWTID: "jti", IssuedAt: time.Now()}, until); err != nil || !fresh {
		t.Fatal("logout refused on a broken ledger", fresh, err)
	}
	if _, err := f.life.NativeAddress("one@example.test"); err == nil {
		t.Fatal("broken ledger read as an address")
	}
	// Per-row validation (finding H3): other owners' reads still work, the
	// whole-ledger reads refuse.
	if _, err := f.life.NativeMailboxesOf(""); err != nil {
		t.Fatal("unrelated read refused", err)
	}
	if _, err := f.life.NativeMailboxes(); err == nil {
		t.Fatal("broken ledger read whole")
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

// A v1 account key without the issuer separator migrates byte for byte as
// persistNative wrote it: writeNative puts the stored key, never a re-join.
func TestAuthorityMigrationKeepsLegacyAccountKey(t *testing.T) {
	ctx := context.Background()
	config, keyPath, _ := v1Fixture(t)
	path := filepath.Join(config, nativeProvisioningFile)
	v1, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(v1), `\u0000one"`) != 1 {
		t.Fatal("fixture key not found")
	}
	v1 = []byte(strings.Replace(string(v1), `\u0000one"`, `one"`, 1))
	if err := os.WriteFile(path, v1, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateNative(ctx, config, keyPath); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The pre-seam write: the whole rendered ledger, persisted as is.
	life := NewLifecycleStore(config).staged()
	f, err := life.migratedNativeLedger(v1)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := life.load()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(t.TempDir(), "want.json")
	if err := fsutil.PersistJSONFile(want, f.ledger(lifecycle.Directory)); err != nil {
		t.Fatal(err)
	}
	wantRaw, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, wantRaw) || !strings.Contains(string(got), nativeIssuer+`one"`) {
		t.Fatalf("legacy key bytes changed:\n%s\nwant\n%s", got, wantRaw)
	}
}

// SetInitialized after PutLifecycle would write the flag before the put
// lifecycle; it is refused.
func TestAuthoritySetInitializedAfterPutLifecycle(t *testing.T) {
	life := NewLifecycleStore(t.TempDir()).staged() // the JSON backend
	err := life.update(func(w authorityWriter) error {
		life, err := w.Lifecycle()
		if err != nil {
			return err
		}
		if err := w.PutLifecycle(life); err != nil {
			return err
		}
		return w.SetInitialized()
	})
	if !errors.Is(err, errAuthorityOrder) {
		t.Fatal("SetInitialized after PutLifecycle", err)
	}
}

// MarkRoutesPending needs a ledger to find the state root in.
func TestAuthorityMarkRoutesPendingNeedsLedger(t *testing.T) {
	life := NewLifecycleStore(t.TempDir()).staged() // the JSON backend
	err := life.update(func(w authorityWriter) error {
		return w.MarkRoutesPending(map[string]int64{"one@example.test": 1})
	})
	if !errors.Is(err, errAuthorityOrder) {
		t.Fatal("MarkRoutesPending without a ledger", err)
	}
	err = life.update(func(w authorityWriter) error {
		if _, _, err := w.Snapshot(); err != nil {
			return err
		}
		return w.MarkRoutesPending(map[string]int64{})
	})
	if err != nil {
		t.Fatal("MarkRoutesPending after Snapshot", err)
	}
}

// Lifecycle hands out copies: a caller's edit reaches later reads, and the
// file, only through PutLifecycle.
func TestAuthorityLifecycleEditsOnlyByPut(t *testing.T) {
	f := newAddressFixture(t, "one")
	key := directoryKey(nativeIssuer, "one")
	err := f.life.update(func(w authorityWriter) error {
		life, err := w.Lifecycle()
		if err != nil {
			return err
		}
		life.Directory[key] = DirectoryState{Revision: 99}
		life.SignOns["jti"] = 1
		if life.RecoveryFloors == nil {
			life.RecoveryFloors = map[string]int64{}
		}
		life.RecoveryFloors[key] = 7
		if d, _, err := w.Directory(nativeIssuer, "one"); err != nil || d.Revision == 99 {
			t.Fatal("unput edit seen by Directory", d.Revision, err)
		}
		again, err := w.Lifecycle()
		if err != nil || again.Directory[key].Revision == 99 || again.SignOns["jti"] != 0 || again.RecoveryFloors[key] != 0 {
			t.Fatal("unput edit seen by Lifecycle", err)
		}
		if err := w.PutLifecycle(life); err != nil {
			return err
		}
		life.SignOns["late"] = 2 // after the put: not written
		put, err := w.Lifecycle()
		if err != nil || put.Directory[key].Revision != 99 || put.SignOns["late"] != 0 {
			t.Fatal("put not seen, or a post-put edit seen", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := f.life.load()
	if err != nil || after.Directory[key].Revision != 99 || after.SignOns["jti"] != 1 || after.SignOns["late"] != 0 || after.RecoveryFloors[key] != 7 {
		t.Fatal("written lifecycle", err)
	}
}
