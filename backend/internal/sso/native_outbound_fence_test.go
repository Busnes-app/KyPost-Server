//go:build linux

package sso

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

// MH4: in table mode a native send takes the domains fence, then the SSO settings fence
// (WithCurrentSettings, table-backed sso.json), then the shared directory fence, on one
// session in fence order: it enqueues and its submission claims the delivery.
func TestFencedNativeOutboundEnqueuesAndClaims(t *testing.T) {
	dbbackend.RequirePostgres(t)
	fences := t.TempDir()
	t.Setenv("STATE_DIR", fences)
	st, err := state.New(fences)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sender, u, job := outboundFixture(t)
	db, err := volumes.Open(fences, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	table, err := vol.NewTable(db, "config", vol.TableOptions{MasterKey: bytes.Repeat([]byte{7}, 32), Org: func(context.Context, string) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(sender.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries { // the platform documents; databases and accounts stay put
		name := e.Name()
		if e.IsDir() || name == "users.json" || strings.Contains(name, ".db") || strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".gate") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(sender.ConfigDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = table.Write(t.Context(), name, body, 0); err != nil {
			t.Fatal(err)
		}
	}
	release, err := vol.MountFenced(sender.ConfigDir, table, state.VolFence(fences))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	vol.FenceRootForTest(t, sender.StateRoot, state.VolFence(fences))
	if !vol.OpenDocument(filepath.Join(sender.ConfigDir, "sso.json")).IsTable() {
		t.Fatal("settings are not table-backed")
	}
	ctx := context.Background()
	id, err := fsutil.NewUUIDv4()
	if err != nil {
		t.Fatal(err)
	}
	if err = sender.Queue(ctx, u.ID, id, job); err != nil {
		t.Fatal("fenced native send did not enqueue", err)
	}
	_, _ = sender.Submit(ctx, u.ID, id, 0) // the loopback relay refuses; the claim is what counts
	statuses, _, err := sender.Status(ctx, u.ID, id)
	if err != nil || len(statuses) != 1 || statuses[0].Attempts != 1 {
		t.Fatal("fenced submission did not claim the delivery", statuses, err)
	}
}
