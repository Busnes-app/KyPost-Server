package vol_test

import (
	"context"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

// OrgKey (MH11 bodies) creates an org's data key only for an account of that org: another
// org's account, or a purged one (accounts go first), creates nothing.
func TestOrgKeyOwnerGuard(t *testing.T) {
	dbbackend.RequirePostgres(t)
	ctx := context.Background()
	dir := t.TempDir()
	db, err := volumes.Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	accounts, err := volumes.OpenAccounts(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer accounts.Close()
	for _, a := range [][]any{{"u-a", "https://id.test", "sub-a", `{"id":"u-a","orgId":"org-a"}`}, {"u-b", "https://id.test", "sub-b", `{"id":"u-b","orgId":"org-b"}`}} {
		if _, err := accounts.Exec(`INSERT INTO accounts(id,username,issuer,subject,record) VALUES(?,?,?,?,?)`, a[0], a[0], a[1], a[2], []byte(a[3].(string))); err != nil {
			t.Fatal(err)
		}
	}
	table, err := vol.NewTable(db, "state", vol.TableOptions{MasterKey: make([]byte, 32), Org: func(context.Context, string) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vol.OrgKey(ctx, dir, "https://id.test", "sub-a", "org-a"); err == nil {
		t.Fatal("a key without a mounted table")
	}
	release, err := vol.Mount(dir, table)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	keys := func() (n int) {
		if err := db.QueryRow(`SELECT count(*) FROM doc_keys`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := vol.OrgKey(ctx, dir, "https://id.test", "sub-b", "org-a"); err == nil || keys() != 0 {
		t.Fatalf("org B's account created org A's key: %v, %d keys", err, keys())
	}
	first, err := vol.OrgKey(ctx, dir, "https://id.test", "sub-a", "org-a")
	if err != nil || len(first) != 32 || keys() != 1 {
		t.Fatalf("org A's key: %v, %d keys", err, keys())
	}
	if again, err := vol.OrgKey(ctx, dir, "https://id.test", "sub-a", "org-a"); err != nil || string(again) != string(first) {
		t.Fatalf("org A's key changed: %v", err)
	}
	// Purge: the account goes first, then the keys; nothing may create one after.
	if _, err := accounts.Exec(`DELETE FROM accounts WHERE id='u-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM doc_keys`); err != nil {
		t.Fatal(err)
	}
	if _, err := vol.OrgKey(ctx, dir, "https://id.test", "sub-a", "org-a"); err == nil || keys() != 0 {
		t.Fatalf("a purged account recreated its org's key: %v, %d keys", err, keys())
	}
}
