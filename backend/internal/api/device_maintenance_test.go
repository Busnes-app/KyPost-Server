//go:build linux

package api

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/sso/ssotest"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

func TestSweepDeviceIndexDoesNotRetainMaintenanceStores(t *testing.T) {
	t.Setenv("STATE_DIR", t.TempDir())
	s := newTestServer(t)
	first := s.mustBootstrapUserID(t)
	second := newStoreCacheUser(t, s, "sweep-cold").ID
	pairNativeDevice(t, s, first, "sweep-live")
	pairNativeDevice(t, s, second, "sweep-second")
	for _, st := range s.userStores {
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
	clear(s.userStores)
	clear(s.userLastSeen)
	warm := newStoreCacheUser(t, s, "sweep-held").ID
	held, err := s.userStore(warm)
	if err != nil {
		t.Fatal(err)
	}
	seen := s.userLastSeen[warm]
	s.deviceIndex["sweep-stale"] = second
	reservation, ok := s.reserveDeviceID(first, "sweep-inflight")
	if !ok {
		t.Fatal("reservation refused")
	}
	defer reservation.Release()
	for n := 0; n < 3; n++ {
		want := 0
		if n == 0 {
			want = 1
		}
		if got := s.sweepDeviceIndex(); got != want {
			t.Fatalf("pass%d removed%d want%d", n, got, want)
		}
		if len(s.userStores) != 1 || len(s.userLastSeen) != 1 || s.userStores[warm] != held || s.userLastSeen[warm] != seen {
			t.Fatalf("maintenance retained stores/timestamps or replaced a live holder: %d/%d", len(s.userStores), len(s.userLastSeen))
		}
		if _, err := held.Checkpoint(); err != nil {
			t.Fatal("maintenance closed held store", err)
		}
		for id, owner := range map[string]string{"sweep-live": first, "sweep-second": second, "sweep-inflight": first} {
			if s.deviceIndex[id] != owner {
				t.Fatalf("lost owner hint%s", id)
			}
		}
	}
	otherStore, err := state.New(s.userStateDir(second))
	if err != nil {
		t.Fatal(err)
	}
	removed, err := otherStore.RemoveNativeDevice("sweep-second")
	if closeErr := otherStore.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || !removed {
		t.Fatalf("independent unpair: %v/%v", removed, err)
	}
	if n := s.sweepDeviceIndex(); n != 1 || s.deviceIndex["sweep-second"] != "" {
		t.Fatal("maintenance used stale device rows after independent unpair")
	}
	if other, ok := s.reserveDeviceID(warm, "sweep-live"); ok {
		other.Release()
		t.Fatal("maintenance allowed cross-owner device collision")
	}
}

func TestSweepDeviceIndexNativeRefusalKeepsOwnerHints(t *testing.T) {
	for _, damage := range []string{"hold", "owner", "source", "ledger", "state", "mailbox"} {
		t.Run(damage, func(t *testing.T) {
			t.Setenv("STATE_DIR", t.TempDir())
			s := newNativeRuntimeServer(t)
			directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "maintenance-create", 1, runtimeDirectoryUser(true)))
			u, err := s.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
			if err != nil {
				t.Fatal(err)
			}
			pairNativeDevice(t, s, u.ID, "maintenance-live")
			// A normally prunable stale hint must survive when this owner's current
			// storage authority cannot be validated. Empty and unreadable differ.
			s.deviceIndex["maintenance-stale"] = u.ID
			var absent string
			switch damage {
			case "hold":
				if err := os.WriteFile(filepath.Join(s.stateDir, sso.NativeRestoreHoldFile), []byte("held"), 0600); err != nil {
					t.Fatal(err)
				}
			case "source":
				markAuthRestoreNative(t, s, u.ID, "source")
			case "owner":
				p := filepath.Join(s.userStateDir(u.ID), "native-mailbox.json")
				raw, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				var m map[string]any
				if err := json.Unmarshal(raw, &m); err != nil {
					t.Fatal(err)
				}
				m["owner"].(map[string]any)["subject"] = "foreign-subject"
				raw, err = json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "ledger":
				absent = ssotest.LedgerPath(s.configDir)
			case "state":
				absent = filepath.Join(s.userStateDir(u.ID), "state.db")
			case "mailbox":
				absent = filepath.Join(s.userStateDir(u.ID), "mailbox", "mailbox.db")
			}
			if absent != "" {
				if err := os.Remove(absent); err != nil {
					t.Fatal(err)
				}
			}
			s.sweepDeviceIndex()
			if s.deviceIndex["maintenance-stale"] != u.ID {
				t.Fatal("invalid native storage was treated as a complete empty device list")
			}
			if absent != "" {
				if _, err := os.Stat(absent); !os.IsNotExist(err) {
					t.Fatalf("maintenance recreated refused storage: %v", err)
				}
			}
		})
	}
}

func TestSweepDeviceIndexUnreadableRowsPreserveOwner(t *testing.T) {
	t.Setenv("STATE_DIR", t.TempDir())
	s := newTestServer(t)
	u := s.mustBootstrapUserID(t)
	pairNativeDevice(t, s, u, "unreadable-live")
	s.deviceIndex["unreadable-stale"] = u
	db, err := sql.Open("sqlite", filepath.Join(s.userStateDir(u), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DROP TABLE native_devices"); err != nil {
		t.Fatal(err)
	}
	if got := s.sweepDeviceIndex(); got != 0 || s.deviceIndex["unreadable-stale"] != u || s.deviceIndex["unreadable-live"] != u {
		t.Fatal("unreadable device rows treated as an empty set")
	}
}
