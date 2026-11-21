package tenancy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

const (
	staffA = "6f1c2b9e-3d4a-4f5b-8c7d-0e1f2a3b4c5d"
	staffB = "0a1b2c3d-4e5f-4a6b-9c8d-7e6f5a4b3c2d"
)

func empty() (bool, error) { return true, nil }

func newStore(t *testing.T) (*state.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := state.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, dir
}

func record(t *testing.T, st *state.Store) (string, string) {
	t.Helper()
	mode, staff, err := st.TenancyRecord()
	if err != nil {
		t.Fatal(err)
	}
	return mode, staff
}

func present(t *testing.T, dir string) bool {
	t.Helper()
	db, err := Open(dir, false)
	if err == nil {
		_ = db.Close()
		return true
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return false
}

// Single mode, unset or explicit, writes nothing: no record and no tenancy store.
func TestSingleWritesNothing(t *testing.T) {
	st, dir := newStore(t)
	for range 2 {
		if err := Check(context.Background(), st, dir, config.TenancySingle, "", func() (bool, error) {
			t.Fatal("single mode asked whether the install is empty")
			return false, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if mode, staff := record(t, st); mode != "" || staff != "" {
		t.Fatalf("single mode recorded %q %q", mode, staff)
	}
	if present(t, dir) {
		t.Fatal("single mode created the tenancy store")
	}
}

// K1: a multi first start on an empty install creates the side tables and the record; the
// same start again passes; a single start, another staff org or a damaged record refuses.
func TestMultiRecord(t *testing.T) {
	ctx := context.Background()
	st, dir := newStore(t)
	if err := Check(ctx, st, dir, config.TenancyMulti, staffA, empty); err != nil {
		t.Fatal(err)
	}
	if mode, staff := record(t, st); mode != config.TenancyMulti || staff != staffA {
		t.Fatalf("record %q %q", mode, staff)
	}
	if !present(t, dir) {
		t.Fatal("no tenancy store after the first multi start")
	}
	if err := Check(ctx, st, dir, config.TenancyMulti, staffA, func() (bool, error) { return false, nil }); err != nil {
		t.Fatalf("a recorded install is not a first start: %v", err)
	}
	if err := Check(ctx, st, dir, config.TenancySingle, "", empty); !errors.Is(err, ErrModeMismatch) {
		t.Fatalf("single start of a multi install: %v", err)
	}
	if err := Check(ctx, st, dir, config.TenancyMulti, staffB, empty); !errors.Is(err, ErrStaffMismatch) || strings.Contains(err.Error(), staffA) || strings.Contains(err.Error(), staffB) {
		t.Fatalf("another staff org: %v", err)
	}
	if err := st.RecordTenancy(config.TenancyMulti, staffB); err == nil {
		t.Fatal("the record was written twice")
	}
	if mode, staff := record(t, st); mode != config.TenancyMulti || staff != staffA {
		t.Fatalf("a refused start changed the record: %q %q", mode, staff)
	}

	if err := os.Remove(filepath.Join(dir, File)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if !present(t, dir) { // on SQLite the file is gone; Postgres keeps its schema
		if err := Check(ctx, st, dir, config.TenancyMulti, staffA, empty); !errors.Is(err, ErrDamaged) {
			t.Fatalf("a record without its store: %v", err)
		}
	}
}

func TestDamagedRecordRefuses(t *testing.T) {
	for name, kv := range map[string][2]string{
		"staff only":      {state.MetaStaffOrgID, staffA},
		"unknown mode":    {state.MetaTenancyMode, "single"},
		"mode only":       {state.MetaTenancyMode, config.TenancyMulti},
		"oversize mode":   {state.MetaTenancyMode, strings.Repeat("m", 1<<16)},
		"uppercase staff": {state.MetaStaffOrgID, strings.ToUpper(staffA)},
	} {
		t.Run(name, func(t *testing.T) {
			st, dir := newStore(t)
			if kv[0] == state.MetaStaffOrgID && name == "uppercase staff" {
				if err := st.SetSetting(state.MetaTenancyMode, config.TenancyMulti); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.SetSetting(kv[0], kv[1]); err != nil {
				t.Fatal(err)
			}
			if err := Check(context.Background(), st, dir, config.TenancyMulti, staffA, empty); !errors.Is(err, ErrDamaged) {
				t.Fatalf("multi: %v", err)
			}
			if err := Check(context.Background(), st, dir, config.TenancySingle, "", empty); !errors.Is(err, ErrModeMismatch) {
				t.Fatalf("single: %v", err)
			}
		})
	}
}

// A multi first start on an install that holds data refuses and writes nothing.
func TestMultiFirstStartNeedsAnEmptyInstall(t *testing.T) {
	st, dir := newStore(t)
	if err := Check(context.Background(), st, dir, config.TenancyMulti, staffA, func() (bool, error) { return false, nil }); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("non-empty install: %v", err)
	}
	if err := Check(context.Background(), st, dir, config.TenancyMulti, staffA, func() (bool, error) { return false, errors.New("unreadable") }); err == nil || errors.Is(err, ErrNotEmpty) {
		t.Fatalf("an emptiness error must refuse: %v", err)
	}
	if mode, staff := record(t, st); mode != "" || staff != "" || present(t, dir) {
		t.Fatalf("a refused first start wrote %q %q", mode, staff)
	}
}

func TestEmpty(t *testing.T) {
	configDir, stateDir := t.TempDir(), t.TempDir()
	if ok, err := Empty(0, configDir, stateDir); err != nil || !ok {
		t.Fatalf("fresh install: %v %v", ok, err)
	}
	if ok, _ := Empty(1, configDir, stateDir); ok {
		t.Fatal("an account is data")
	}
	for _, p := range []string{filepath.Join(configDir, "users", "u1"), filepath.Join(stateDir, "users", "u1"), filepath.Join(stateDir, "mailboxes", "m1")} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if ok, err := Empty(0, configDir, stateDir); err != nil || ok {
			t.Fatalf("%s: %v %v", p, ok, err)
		}
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
	authority := filepath.Join(configDir, "native-authority.db")
	if err := os.WriteFile(authority, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := Empty(0, configDir, stateDir); err != nil || ok {
		t.Fatalf("native authority: %v %v", ok, err)
	}
}

// K3: user_orgs and subject_orgs refuse an update and a second binding; orgs is a cache the
// poller updates. orgs refuses an unknown status.
func TestSideTablesAreWriteOnce(t *testing.T) {
	st, dir := newStore(t)
	if err := Check(context.Background(), st, dir, config.TenancyMulti, staffA, empty); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) error { _, err := db.Exec(q, args...); return err }
	for _, ok := range []struct{ q string }{
		{`INSERT INTO user_orgs(user_id, org_id) VALUES('u1', '` + staffA + `')`},
		{`INSERT INTO subject_orgs(issuer, subject, org_id) VALUES('https://id.example', 's1', '` + staffA + `')`},
		{`INSERT INTO orgs(id, status, revision, enabled, quotas_json, polled_at) VALUES('` + staffB + `', 'active', 1, 1, '{}', 1)`},
		{`UPDATE orgs SET status='suspended', revision=2 WHERE id='` + staffB + `'`},
	} {
		if err := exec(ok.q); err != nil {
			t.Fatalf("%s: %v", ok.q, err)
		}
	}
	for _, refused := range []string{
		`UPDATE user_orgs SET org_id='` + staffB + `' WHERE user_id='u1'`,
		`UPDATE user_orgs SET org_id=org_id WHERE user_id='u1'`,
		`UPDATE subject_orgs SET org_id='` + staffB + `'`,
		`INSERT INTO user_orgs(user_id, org_id) VALUES('u1', '` + staffB + `')`,
		`INSERT INTO subject_orgs(issuer, subject, org_id) VALUES('https://id.example', 's1', '` + staffB + `')`,
		`INSERT INTO user_orgs(user_id, org_id) VALUES('u2', '')`,
		`INSERT INTO orgs(id, status, revision, enabled, quotas_json, polled_at) VALUES('x', 'paused', 1, 1, '{}', 1)`,
		`INSERT INTO orgs(id, status, revision, enabled, quotas_json, polled_at) VALUES('y', 'active', 1, 2, '{}', 1)`,
	} {
		if err := exec(refused); err == nil {
			t.Errorf("accepted: %s", refused)
		}
	}
	var org string
	if err := db.QueryRow(`SELECT org_id FROM user_orgs WHERE user_id='u1'`).Scan(&org); err != nil || org != staffA {
		t.Fatalf("binding changed: %q %v", org, err)
	}
}

// users.User.OrgID is omitempty, so a single-tenant users.json has no orgId key.
func TestOrgIDIsOmittedWhenEmpty(t *testing.T) {
	raw, err := json.Marshal(users.User{ID: "u1"})
	if err != nil || strings.Contains(string(raw), "orgId") {
		t.Fatalf("%s %v", raw, err)
	}
	raw, _ = json.Marshal(users.User{ID: "u1", OrgID: staffA})
	if !strings.Contains(string(raw), `"orgId":"`+staffA+`"`) {
		t.Fatalf("%s", raw)
	}
}

// Replicas starting together on an empty PostgreSQL install record once, under the
// replica-config lock, and all start.
func TestConcurrentMultiFirstStart(t *testing.T) {
	dbbackend.RequirePostgres(t)
	dir := t.TempDir()
	errs := make(chan error, 4)
	for range 4 {
		go func() {
			st, err := state.New(dir)
			if err == nil {
				err = Check(context.Background(), st, dir, config.TenancyMulti, staffA, empty)
				_ = st.Close()
			}
			errs <- err
		}()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	st, err := state.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if mode, staff := record(t, st); mode != config.TenancyMulti || staff != staffA || !present(t, dir) {
		t.Fatalf("record %q %q", mode, staff)
	}
}
