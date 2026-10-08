//go:build linux

package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

func TestNativeRestoreQualification(t *testing.T) {
	ctx := context.Background()
	s, u := nativeService(t)
	life := sso.NewLifecycleStore(s.dirs.Config)
	m, err := life.CreateNativeMailbox(ctx, s.dirs.State, u.ID, "sales@example.test")
	if err != nil {
		t.Fatal(err)
	}
	extra, ok, err := life.NativeMailboxAssignment(m.ID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	// A marker on the live host must never reach a capsule.
	if err := os.WriteFile(filepath.Join(s.dirs.State, sso.NativeRestoreQualificationFile), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	payload, err := s.Collect()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range payload.Files {
		if filepath.Base(f.Path) == sso.NativeRestoreQualificationFile {
			t.Fatalf("collected %s", f.Path)
		}
	}
	key := pinTestKey(t, s)
	result, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := os.ReadFile(result.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	dbs := map[string]string{
		u.ID:                "state/users/" + u.ID + "/mailbox/mailbox.db",
		extra.Owner.Mailbox: "state/mailboxes/" + extra.Owner.Mailbox + "/mailbox/mailbox.db",
	}
	restore := func(t *testing.T) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "restored")
		if _, _, err := capsule.Open(sealed, key, dir); err != nil {
			t.Fatal(err)
		}
		if native, err := QuarantineNativeRestore(dir); !native || err != nil {
			t.Fatalf("native=%v err=%v", native, err)
		}
		return dir
	}
	check := func(dir string) (sso.NativeRestoreQualification, error) {
		return sso.NewLifecycleStore(filepath.Join(dir, "config")).CheckNativeRestoreQualification(filepath.Join(dir, "state"))
	}
	markerPath := func(dir string) string { return filepath.Join(dir, "state", sso.NativeRestoreQualificationFile) }
	readJSON := func(t *testing.T, path string) map[string]any {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	writeJSON := func(t *testing.T, path string, v any) {
		t.Helper()
		if err := fsutil.PersistJSONFile(path, v); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("fresh-restore", func(t *testing.T) {
		start := time.Now().Add(-time.Second)
		dir := restore(t)
		hold := readJSON(t, filepath.Join(dir, "state", sso.NativeRestoreHoldFile))
		q, err := check(dir)
		if err != nil {
			t.Fatal(err)
		}
		if q.Version != 1 || q.Epoch != hold["epoch"] || q.CreatedAt.Before(start) || q.CreatedAt.After(time.Now()) || len(q.Mailboxes) != len(dbs) {
			t.Fatalf("marker %+v, hold epoch %v", q, hold["epoch"])
		}
		for id, rel := range dbs {
			generation, _, err := mailbox.InspectRestored(filepath.Join(dir, rel))
			if err != nil || generation == "" || q.Mailboxes[id] != generation {
				t.Fatalf("mailbox %s: marker %q, current %q, %v", id, q.Mailboxes[id], generation, err)
			}
		}
		if !errors.Is(sso.RequireNativeRestoreReleased(filepath.Join(dir, "state")), sso.ErrNativeRestoreHold) {
			t.Fatal("qualification released the hold")
		}
	})

	failures := []struct {
		name, reason string
		damage       func(t *testing.T, dir string)
	}{
		{"missing-marker", "restore again with this version to qualify for release", func(t *testing.T, dir string) {
			if err := os.Remove(markerPath(dir)); err != nil {
				t.Fatal(err)
			}
		}},
		{"epoch-mismatch", "another restore epoch", func(t *testing.T, dir string) {
			path := filepath.Join(dir, "state", sso.NativeRestoreHoldFile)
			hold := readJSON(t, path)
			epoch, _ := fsutil.NewUUIDv4()
			hold["epoch"] = epoch
			writeJSON(t, path, hold)
		}},
		{"mailbox-missing", "mailbox " + extra.Owner.Mailbox + " is missing from the qualification marker", func(t *testing.T, dir string) {
			q := readJSON(t, markerPath(dir))
			delete(q["mailboxes"].(map[string]any), extra.Owner.Mailbox)
			writeJSON(t, markerPath(dir), q)
		}},
		{"generation-changed", "mailbox " + u.ID + " reference generation changed since the restore", func(t *testing.T, dir string) {
			if err := mailbox.RotateRestoredMessageReferences(filepath.Join(dir, dbs[u.ID]), u.NativeMailboxSource); err != nil {
				t.Fatal(err)
			}
		}},
		{"generation-dropped", "mailbox " + u.ID + " has no reference generation", func(t *testing.T, dir string) {
			execSQL(t, filepath.Join(dir, dbs[u.ID]), "DROP TABLE reference_generation")
		}},
		{"queued-outbox", "has 1 queued or retryable outbound deliveries", func(t *testing.T, dir string) {
			execSQL(t, filepath.Join(dir, dbs[u.ID]), "INSERT INTO outbox(id,ciphertext,sent_bytes,sent_reserved) VALUES('job',x'00',0,0); INSERT INTO outbox_deliveries(job,sequence,state) VALUES('job',1,'queued')")
		}},
		{"retryable-outbox", "mailboxes/" + extra.Owner.Mailbox + "/mailbox/mailbox.db has 1 queued or retryable", func(t *testing.T, dir string) {
			execSQL(t, filepath.Join(dir, dbs[extra.Owner.Mailbox]), "INSERT INTO outbox(id,ciphertext,sent_bytes,sent_reserved) VALUES('job',x'00',0,0); INSERT INTO outbox_deliveries(job,sequence,state) VALUES('job',1,'retryable')")
		}},
		{"unknown-field", "marker is malformed", func(t *testing.T, dir string) {
			q := readJSON(t, markerPath(dir))
			q["released"] = true
			writeJSON(t, markerPath(dir), q)
		}},
		{"wrong-version", "marker is malformed", func(t *testing.T, dir string) {
			q := readJSON(t, markerPath(dir))
			q["version"] = 2
			writeJSON(t, markerPath(dir), q)
		}},
		{"no-mailboxes", "marker is malformed", func(t *testing.T, dir string) {
			q := readJSON(t, markerPath(dir))
			delete(q, "mailboxes")
			writeJSON(t, markerPath(dir), q)
		}},
		{"trailing-data", "marker is malformed", func(t *testing.T, dir string) {
			raw, err := os.ReadFile(markerPath(dir))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(markerPath(dir), append(raw, "{}"...), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"readable-by-others", "marker is malformed", func(t *testing.T, dir string) {
			if err := os.Chmod(markerPath(dir), 0644); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			dir := restore(t)
			tc.damage(t, dir)
			_, err := check(dir)
			var unqualified *sso.NativeRestoreUnqualifiedError
			if !errors.As(err, &unqualified) || len(unqualified.Reasons) != 1 || !strings.Contains(unqualified.Reasons[0], tc.reason) {
				t.Fatalf("want one reason containing %q, got %v", tc.reason, err)
			}
		})
	}

	// A failed later run removes the earlier run's marker and writes none.
	for _, stage := range []string{"validation", "credential-fence"} {
		t.Run("failed-"+stage, func(t *testing.T) {
			dir := restore(t)
			if stage == "validation" {
				st, err := state.OpenNative(filepath.Join(dir, "state/mailboxes", extra.Owner.Mailbox), extra.Source)
				if err != nil {
					t.Fatal(err)
				}
				err = st.UpsertNativeDevice(state.NativeDevice{DeviceID: "phone", Platform: "android", PushToken: "push", SecretHash: "credential"})
				_ = st.Close()
				if err != nil {
					t.Fatal(err)
				}
			} else if err := os.MkdirAll(filepath.Join(dir, "config/users", u.ID, "carddav-auth.json", "busy"), 0700); err != nil {
				t.Fatal(err)
			}
			if native, err := QuarantineNativeRestore(dir); !native || err == nil {
				t.Fatalf("native=%v err=%v", native, err)
			}
			if _, err := os.Lstat(markerPath(dir)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("marker survived a failed stage: %v", err)
			}
			if _, err := check(dir); err == nil || !strings.Contains(err.Error(), "restore again with this version") {
				t.Fatal(err)
			}
		})
	}
}

func execSQL(t *testing.T, path, query string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
}
