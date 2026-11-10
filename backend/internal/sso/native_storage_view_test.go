//go:build linux

package sso

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// A warmed ownership read must retain the full reader's refusal semantics,
// including unrelated malformed fields and writes that keep size and mtime.
func TestNativeStorageViewRechecksCurrentFiles(t *testing.T) {
	for _, damage := range []string{"source-same-stat", "lifecycle-syntax", "lifecycle-unrelated", "uninitialized", "ledger-syntax", "ledger-history", "missing-ledger", "missing-lifecycle", "missing-state", "missing-mailbox", "format"} {
		t.Run(damage, func(t *testing.T) {
			f := newAddressFixture(t, "one")
			u, err := f.accounts.Get(f.ids["one"])
			if err != nil {
				t.Fatal(err)
			}
			if err := f.life.ValidateNativeUserStorage(f.root, u); err != nil {
				t.Fatal(err)
			}
			path := f.life.nativePath()
			var absent string
			switch damage {
			case "source-same-stat":
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				editAssignment(t, NewLifecycleStore(f.config), func(a *NativeAssignment) {
					if a.Source[0] == 'x' {
						a.Source = "y" + a.Source[1:]
					} else {
						a.Source = "x" + a.Source[1:]
					}
				})
				after, err := os.Stat(path)
				if err != nil || after.Size() != info.Size() {
					t.Fatal("test must preserve size", err)
				}
				if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "lifecycle-syntax", "ledger-syntax":
				if damage == "lifecycle-syntax" {
					path = f.life.path
				}
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "lifecycle-unrelated":
				raw, err := os.ReadFile(f.life.path)
				if err != nil {
					t.Fatal(err)
				}
				var object map[string]json.RawMessage
				if err := json.Unmarshal(raw, &object); err != nil {
					t.Fatal(err)
				}
				object["logouts"] = json.RawMessage(`[]`)
				if err := fsutil.PersistJSONFile(f.life.path, object); err != nil {
					t.Fatal(err)
				}
			case "uninitialized":
				current, err := f.life.load()
				if err != nil {
					t.Fatal(err)
				}
				current.NativeProvisioningInitialized = false
				if err := fsutil.PersistJSONFile(f.life.path, current); err != nil {
					t.Fatal(err)
				}
			case "ledger-history":
				ledger, err := f.life.loadNative()
				if err != nil {
					t.Fatal(err)
				}
				for key, address := range ledger.stored.Addresses {
					address.History = nil
					ledger.stored.Addresses[key] = address
				}
				if err := fsutil.PersistJSONFile(path, ledger.stored); err != nil {
					t.Fatal(err)
				}
			case "missing-ledger", "missing-lifecycle", "missing-state", "missing-mailbox":
				absent = path
				switch damage {
				case "missing-lifecycle":
					absent = f.life.path
				case "missing-state":
					absent = filepath.Join(f.root, "users", u.ID, "state.db")
				case "missing-mailbox":
					absent = filepath.Join(f.root, "users", u.ID, "mailbox", "mailbox.db")
				}
				if err := os.Remove(absent); err != nil {
					t.Fatal(err)
				}
			case "format":
				if err := os.WriteFile(filepath.Join(f.config, "native-domain.json"), []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Keep the original full reader as an independent semantic reference.
			a, ok, want := f.life.NativeAssignment(u.NativeMailboxIssuer, u.SSOSub)
			if want == nil {
				_, want = validateNativeUserAssignment(f.root, u, a, ok, nil)
			}
			if want == nil {
				t.Fatal("damaged fixture must fail the original reader")
			}
			for _, got := range []error{f.life.ValidateNativeUserOwnership(f.root, u), f.life.ValidateNativeUserStorage(f.root, u)} {
				if got == nil || reflect.TypeOf(got) != reflect.TypeOf(want) || got.Error() != want.Error() {
					t.Fatalf("refusal changed: got %T %v; want %T %v", got, got, want, want)
				}
			}
			if absent != "" {
				if _, err := os.Stat(absent); !os.IsNotExist(err) {
					t.Fatalf("missing storage recreated: %v", err)
				}
			}
		})
	}
}

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
