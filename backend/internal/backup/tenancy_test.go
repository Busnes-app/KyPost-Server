package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
)

const (
	keptOrg   = "1a2b3c4d-0000-4000-8000-00000000000a"
	purgedOrg = "1a2b3c4d-0000-4000-8000-00000000000b"
	liveOrg   = "1a2b3c4d-0000-4000-8000-00000000000c"
)

// seedTenancy gives s a multi-tenant tenancy.db: an org cache row, both org roots, a sender
// block, a gateway pin, a finished and a live purge, and one address tombstone.
func seedTenancy(t *testing.T, s *Service) (tombstone string) {
	t.Helper()
	ctx := context.Background()
	db, err := tenancy.Open(s.dirs.State, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().Unix()
	for _, q := range []string{
		`INSERT INTO orgs(id, status, revision, enabled, quotas_json, polled_at) VALUES('` + keptOrg + `', 'active', 3, 1, '{}', 1)`,
		`INSERT INTO orgs(id, status, revision, enabled, quotas_json, polled_at) VALUES('` + purgedOrg + `', 'deleted', 4, 0, '{}', 1)`,
		`INSERT INTO user_orgs(user_id, org_id) VALUES('u-kept', '` + keptOrg + `')`,
		`INSERT INTO subject_orgs(issuer, subject, org_id) VALUES('https://identity.example.test', 's-kept', '` + keptOrg + `')`,
		`INSERT INTO sender_blocks(id, kind, value, until, source, level, created_at, actor, reason) VALUES('b1', 'domain', 'spam.example', NULL, 'manual', 0, 1, 'staff', 'test')`,
		`INSERT INTO receiving_gateways(id, spki_sha256, created_at, expires_at) VALUES('mx-1', 'aa', 1, 99999999999)`,
		`INSERT INTO org_purges(org_id, subject, step, started_at, done_at) VALUES('` + purgedOrg + `', '', 5, 1, 2)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	if _, err := tenancy.StartPurge(ctx, db, tenancy.PurgeScope{Org: liveOrg}, tenancy.PurgeSnapshot{Users: []string{"u-live"}}, time.Unix(now, 0)); err != nil {
		t.Fatal(err)
	}
	key, err := tenancy.TombstoneKey(ctx, db, s.dirs.Secret)
	if err != nil {
		t.Fatal(err)
	}
	tombstone = tenancy.Tombstone(key, "gone@example.test")
	if err := tenancy.AddTombstones(ctx, db, []string{tombstone}); err != nil {
		t.Fatal(err)
	}
	return tombstone
}

func countRows(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(query, err)
	}
	return n
}

// checkRestoredTenancy is what a restore must leave in tenancy.db: blocks, pins, org
// roots, the live purge and its snapshot, and tombstones kept; the org cache and finished
// purges gone, so no org is served before a poll and every deleted org is purged again.
func checkRestoredTenancy(t *testing.T, db *sql.DB, tombstone string) {
	t.Helper()
	for query, want := range map[string]int{
		`SELECT count(*) FROM orgs`:                                                0,
		`SELECT count(*) FROM org_purges WHERE done_at IS NOT NULL`:                0,
		`SELECT count(*) FROM org_purges WHERE org_id = '` + liveOrg + `'`:         1,
		`SELECT count(*) FROM org_purge_members WHERE org_id = '` + liveOrg + `'`:  1,
		`SELECT count(*) FROM user_orgs WHERE org_id = '` + keptOrg + `'`:          1,
		`SELECT count(*) FROM subject_orgs WHERE org_id = '` + keptOrg + `'`:       1,
		`SELECT count(*) FROM sender_blocks WHERE value = 'spam.example'`:          1,
		`SELECT count(*) FROM receiving_gateways WHERE id = 'mx-1'`:                1,
		`SELECT count(*) FROM address_tombstones WHERE hmac = '` + tombstone + `'`: 1,
	} {
		if got := countRows(t, db, query); got != want {
			t.Errorf("%s = %d, want %d", query, got, want)
		}
	}
}

// TestTenancySurvivesARestore seals a multi-tenant install, restores it as runRestore does,
// and checks tenancy.db on SQLite and, on the PostgreSQL job, after the import into
// PostgreSQL. The tombstone key comes back with it, so the restored tombstones still match.
func TestTenancySurvivesARestore(t *testing.T) {
	s, _ := nativeService(t)
	tombstone := seedTenancy(t, s)
	key := pinTestKey(t, s)
	result, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := os.ReadFile(result.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "data")
	if _, _, err := capsule.Open(sealed, key, dir); err != nil {
		t.Fatal(err)
	}
	if native, err := QuarantineNativeRestore(dir); !native || err != nil {
		t.Fatal("quarantine", native, err)
	}
	if err := state.ResetReplicaState(filepath.Join(dir, "state")); err != nil {
		t.Fatal(err)
	}
	if err := tenancy.FenceRestored(filepath.Join(dir, "state")); err != nil {
		t.Fatal(err)
	}
	restoredKey, err := cryptutil.LoadKey(filepath.Join(dir, "private", tenancy.TombstoneKeyFile))
	if err != nil {
		t.Fatal("the tombstone key was not sealed:", err)
	}
	if tenancy.Tombstone(restoredKey, "gone@example.test") != tombstone {
		t.Fatal("the restored key does not match the tombstones")
	}
	staged, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "state", tenancy.File)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	checkRestoredTenancy(t, staged, tombstone)
	_ = staged.Close()
	if !dbbackend.Postgres() {
		return
	}
	dbbackend.FreshDatabaseForTest(t)
	t.Setenv("STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("CONFIG_DIR", filepath.Join(dir, "config"))
	roots := []Root{{Dir: filepath.Join(dir, "config"), Prefix: "config"}, {Dir: filepath.Join(dir, "state"), Prefix: "state"}}
	if _, err := ImportDatabases(context.Background(), roots, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	db, err := tenancy.Open(filepath.Join(dir, "state"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	checkRestoredTenancy(t, db, tombstone)
}

// TestTombstonesNeedTheirKeyInTheCapsule: a capsule of address tombstones without their
// key would let a restore reissue every tombstoned address, so it is refused.
func TestTombstonesNeedTheirKeyInTheCapsule(t *testing.T) {
	s, _ := nativeService(t)
	seedTenancy(t, s)
	pinTestKey(t, s)
	if err := os.Remove(filepath.Join(s.dirs.Secret, tenancy.TombstoneKeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "address tombstones") {
		t.Fatal("sealed tombstones without their key:", err)
	}
}
