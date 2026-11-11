//go:build linux

package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/sso/ssotest"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

func TestDeviceDiscoveryFallsBackForOlderStorage(t *testing.T) {
	for _, layout := range []string{"json", "older-schema"} {
		for _, sweep := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/sweep=%t", layout, sweep), func(t *testing.T) {
				t.Setenv("STATE_DIR", t.TempDir())
				s := newTestServer(t)
				u := newStoreCacheUser(t, s, "discovery-upgrade")
				dir := s.userStateDir(u.ID)
				if layout == "json" {
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
					raw, err := json.Marshal(map[string]any{"nativeDevices": []state.NativeDevice{{DeviceID: "discovery-old", Platform: "android", UserID: u.ID}}})
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "state.json"), raw, 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					pairNativeDevice(t, s, u.ID, "discovery-old")
					if err := s.userStores[u.ID].Close(); err != nil {
						t.Fatal(err)
					}
					clear(s.userStores)
					db, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					if _, err := db.Exec("DROP TABLE backup_audit"); err != nil {
						t.Fatal(err)
					}
				}
				clear(s.deviceIndex)
				if sweep {
					s.deviceIndex["discovery-old"] = u.ID
					s.sweepDeviceIndex()
				} else {
					s.rescanDeviceIndex()
				}
				if s.deviceIndex["discovery-old"] != u.ID {
					t.Fatalf("%s device lost during compatible fallback", layout)
				}
				if layout == "json" {
					if _, err := os.Stat(filepath.Join(dir, "state.json.migrated")); err != nil {
						t.Fatal("legacy input was not preserved", err)
					}
				}
			})
		}
	}
}

func TestDeviceDiscoveryDoesNotRetainAccountStores(t *testing.T) {
	t.Setenv("STATE_DIR", t.TempDir())
	s := newTestServer(t)
	first := s.mustBootstrapUserID(t)
	second := newStoreCacheUser(t, s, "discovery-other").ID
	pairNativeDevice(t, s, first, "discovery-first")
	pairNativeDevice(t, s, second, "discovery-second")
	for _, store := range s.userStores {
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	clear(s.userStores)
	clear(s.userLastSeen)
	clear(s.deviceIndex)
	reservation, ok := s.reserveDeviceID(first, "discovery-pending")
	if !ok {
		t.Fatal("reserve pending device")
	}
	defer reservation.Release()
	s.rescanDeviceIndex()
	if len(s.userStores) != 0 || len(s.userLastSeen) != 0 {
		t.Fatalf("discovery retained %d stores / %d timestamps", len(s.userStores), len(s.userLastSeen))
	}
	for id, owner := range map[string]string{"discovery-first": first, "discovery-second": second, "discovery-pending": first} {
		if s.deviceIndex[id] != owner {
			t.Fatalf("owner hint for %s = %q, want %q", id, s.deviceIndex[id], owner)
		}
	}
	if other, ok := s.reserveDeviceID(second, "discovery-first"); ok {
		other.Release()
		t.Fatal("read-only discovery lost the persisted cross-owner collision")
	}
}

func TestDeviceDiscoveryHintsDoNotAuthorizeNativeMail(t *testing.T) {
	for _, damage := range []string{"unpair", "hold", "mailbox", "ledger", "state", "source", "owner"} {
		t.Run(damage, func(t *testing.T) {
			t.Setenv("STATE_DIR", t.TempDir())
			s := newNativeRuntimeServer(t)
			directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "discovery-create", 1, runtimeDirectoryUser(true)))
			u, err := s.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
			if err != nil {
				t.Fatal(err)
			}
			id, secret := pairNativeDevice(t, s, u.ID, "native-discovery")
			r := httptest.NewRequest(http.MethodGet, "/api/inbox", nil)
			setDeviceHeaders(r, id, secret)
			if _, err := s.resolveMailAuthContext(r); err != nil {
				t.Fatalf("initial native credential refused: %v", err)
			}
			clear(s.deviceIndex)
			s.rescanDeviceIndex()
			if s.deviceIndex[id] != u.ID {
				t.Fatal("cold discovery failed to find the owner")
			}
			var absent string
			switch damage {
			case "unpair":
				other, err := state.OpenNative(s.userStateDir(u.ID), u.NativeMailboxSource)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
				if removed, err := other.RemoveNativeDevice(id); err != nil || !removed {
					t.Fatalf("independent unpair: %v %v", removed, err)
				}
			case "source":
				markAuthRestoreNative(t, s, u.ID, "source")
			case "owner":
				path := filepath.Join(s.userStateDir(u.ID), "native-mailbox.json")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var manifest map[string]any
				if err := json.Unmarshal(raw, &manifest); err != nil {
					t.Fatal(err)
				}
				manifest["owner"].(map[string]any)["subject"] = "foreign-subject"
				raw, err = json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "hold":
				if err := os.WriteFile(filepath.Join(s.stateDir, sso.NativeRestoreHoldFile), []byte("held"), 0600); err != nil {
					t.Fatal(err)
				}
			case "mailbox":
				absent = filepath.Join(s.userStateDir(u.ID), "mailbox", "mailbox.db")
			case "ledger":
				absent = ssotest.LedgerPath(s.configDir)
			case "state":
				absent = filepath.Join(s.userStateDir(u.ID), "state.db")
			}
			if absent != "" {
				if err := os.Remove(absent); err != nil {
					t.Fatal(err)
				}
			}
			for _, cold := range []bool{false, true} {
				if cold {
					clear(s.deviceIndex)
					s.rescanDeviceIndex()
				}
				if _, err := s.resolveMailAuthContext(r); err == nil {
					t.Fatalf("owner hint bypassed %s (cold=%v)", damage, cold)
				}
			}
			if absent != "" {
				if _, err := os.Stat(absent); !os.IsNotExist(err) {
					t.Fatalf("recreated missing storage: %v", err)
				}
			}
		})
	}
}
