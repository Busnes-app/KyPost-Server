package vol_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

func TestPurgedOwnerCannotRecreateDocumentOrOrgKey(t *testing.T) {
	dbbackend.RequirePostgres(t)
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
	if _, err = accounts.Exec(`INSERT INTO accounts(id,username,record) VALUES('gone','gone',?)`, []byte(`{"id":"gone","username":"gone","orgId":"org-gone"}`)); err != nil {
		t.Fatal(err)
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	var pause atomic.Bool
	pause.Store(true)
	table, err := vol.NewTable(db, "state", vol.TableOptions{MasterKey: make([]byte, 32), Org: func(context.Context, string) (string, error) {
		if pause.CompareAndSwap(true, false) {
			close(entered)
			<-resume
		}
		// This deliberately retained result must not authorize a post-purge write.
		return "org-gone", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := table.Write(ctx, "users/gone/contacts.json", []byte("old writer"), 0); result <- err }()
	<-entered
	if _, err = accounts.Exec(`DELETE FROM accounts WHERE id='gone'`); err != nil {
		close(resume)
		t.Fatal(err)
	}
	close(resume)
	if err = <-result; err == nil {
		t.Fatal("paused writer survived owner purge")
	}
	if _, err = table.Write(ctx, "users/gone/contacts.json", []byte("retried writer"), 0); err == nil {
		t.Fatal("retry recreated a purged owner's document")
	}
	for _, name := range []string{"documents", "doc_keys"} {
		var n int
		if err = db.QueryRow(`SELECT count(*) FROM ` + name).Scan(&n); err != nil || n != 0 {
			t.Fatal(name, n, err)
		}
	}
	var noAccount int
	if err = accounts.QueryRow(`SELECT count(*) FROM accounts`).Scan(&noAccount); err != nil || noAccount != 0 {
		t.Fatal(noAccount, err)
	}
}
