package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/replica"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// AC17: accept-replica-config's -h changes nothing; it refuses SQLite, a live instance and a
// different totp-secret.key; it rewrites the record naming fields only, never a value.
func TestAcceptReplicaConfig(t *testing.T) {
	stateDir, secretDir := t.TempDir(), t.TempDir()
	t.Setenv("STATE_DIR", stateDir)
	t.Setenv("CONFIG_DIR", t.TempDir())
	t.Setenv("SECRET_DIR", secretDir)
	key := filepath.Join(secretDir, "totp-secret.key")
	if err := os.WriteFile(key, []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	t.Run("SQLite", func(t *testing.T) {
		dbbackend.SetURLForTest(t, "")
		if err := runAcceptReplicaConfig(nil, &out); err == nil || !strings.Contains(err.Error(), "PostgreSQL") {
			t.Fatalf("SQLite: %v", err)
		}
	})
	dbbackend.RequirePostgres(t)
	dbbackend.FreshDatabaseForTest(t)
	if err := runAcceptReplicaConfig([]string{"-h"}, &out); err != nil || !strings.Contains(out.String(), "usage") {
		t.Fatalf("-h: %v %q", err, out.String())
	}
	if ok, _ := dbbackend.Exists(filepath.Join(stateDir, "state.db")); ok {
		t.Fatal("-h created the state database")
	}
	ctx := context.Background()
	if err := dbbackend.Check(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := state.New(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t.Setenv("SERVER_BASE_URL", "https://mail.example.org")
	if err := replica.CheckConfig(ctx, st, key, nil); err != nil {
		t.Fatal(err)
	}
	release, err := st.RegisterInstance(ctx, state.Instance{ID: "api-live", BootID: "b", VolumeID: "v", Role: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runAcceptReplicaConfig(nil, &out); err == nil || !strings.Contains(err.Error(), "stop every replica") {
		t.Fatalf("live instance: %v", err)
	}
	release()
	t.Setenv("SERVER_BASE_URL", "https://new.example.org")
	out.Reset()
	if err := runAcceptReplicaConfig(nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "SERVER_BASE_URL") || strings.Contains(out.String(), "new.example") || regexp.MustCompile(`[0-9a-f]{32}`).MatchString(out.String()) {
		t.Fatalf("output names a value or a digest: %q", out.String())
	}
	if err := replica.CheckConfig(ctx, st, key, nil); err != nil {
		t.Fatalf("after accepting: %v", err)
	}
	if err := os.WriteFile(key, []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runAcceptReplicaConfig(nil, &out); err == nil || !strings.Contains(err.Error(), replica.KeyField) {
		t.Fatalf("different key: %v", err)
	}
}
