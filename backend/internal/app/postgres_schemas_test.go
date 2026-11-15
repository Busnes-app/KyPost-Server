package app

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/cfreceiving"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// AC4: each store database is its own schema with a catalog row; names are distinct,
// short whatever the user ID, and the same on reopen.
func TestPostgresSchemaPerDatabase(t *testing.T) {
	dbbackend.RequirePostgres(t)
	root := t.TempDir()
	stateDir, configDir := filepath.Join(root, "state"), filepath.Join(root, "config")
	t.Setenv("STATE_DIR", stateDir)
	t.Setenv("CONFIG_DIR", configDir)
	dbbackend.FreshDatabaseForTest(t)
	id := strings.Repeat("u", 64)
	owner := mailbox.Owner{Issuer: "https://identity.example.test", Subject: "s", Mailbox: id}
	limits := mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100}
	open := func() {
		t.Helper()
		for _, dir := range []string{filepath.Join(stateDir, "receiving"), configDir} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		for _, dir := range []string{stateDir, filepath.Join(stateDir, "users", id)} {
			st, err := state.New(dir)
			if err != nil {
				t.Fatal(err)
			}
			_ = st.Close()
		}
		m, err := mailbox.Open(filepath.Join(stateDir, "users", id, "mailbox"), owner, limits)
		if err != nil {
			t.Fatal(err)
		}
		_ = m.Close()
		in, err := ingress.Open(filepath.Join(stateDir, "receiving"), ingress.ReceivingLimits)
		if err != nil {
			t.Fatal(err)
		}
		_ = in.Close()
		cf, err := cfreceiving.Open(filepath.Join(stateDir, "receiving"))
		if err != nil {
			t.Fatal(err)
		}
		_ = cf.Close()
		if _, err := sso.NewLifecycleStore(configDir).ApplyDirectory("https://identity.example.test", syncauth.Event{ID: "e1", Type: "user.updated", At: time.Now()}, "s", 1, "digest", true, func() (bool, error) { return false, nil }); err != nil {
			t.Fatal(err)
		}
	}
	open()
	first, err := dbbackend.Stores(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"state/state.db":                            state.Kind,
		"state/users/" + id + "/state.db":           state.Kind,
		"state/users/" + id + "/mailbox/mailbox.db": mailbox.Kind,
		"state/receiving/ingress.db":                ingress.Kind,
		"state/receiving/cloudflare.db":             cfreceiving.Kind,
		"config/native-authority.db":                sso.AuthorityKind,
	}
	schemas := map[string]bool{}
	for _, st := range first {
		if want[st.Path] != st.Kind {
			t.Fatalf("unexpected catalog row %+v", st)
		}
		if len(st.Schema) > 63 || schemas[st.Schema] {
			t.Fatalf("schema %q is too long or shared", st.Schema)
		}
		schemas[st.Schema] = true
	}
	if len(first) != len(want) {
		t.Fatalf("catalog %+v, want %v", first, want)
	}
	open()
	again, err := dbbackend.Stores(context.Background())
	if err != nil || !reflect.DeepEqual(first, again) {
		t.Fatalf("reopen changed the catalog: %+v %v", again, err)
	}
}
