package vol_test

import (
	"bytes"
	"context"
	"errors"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"strings"
	"sync"
	"testing"
)

func TestDocumentsAreSealedAndBoundToTheirOwner(t *testing.T) {
	options := tableOptions()
	options.Org = func(_ context.Context, owner string) (string, error) {
		if owner == "u-b" {
			return "org-b", nil
		}
		return "org-a", nil
	}
	a, b, db := twoTablesWithOptions(t, "state", options)
	const path = "users/u-a/contacts.json"
	plain := []byte(`{"name":"MH2 Private Name","email":"private@example.test"}`)
	if _, err := a.Write(ctx, path, plain, 0); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := db.QueryRow(`SELECT body FROM documents WHERE path=?`, path).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("Private Name")) || bytes.Contains(raw, []byte("private@example.test")) {
		t.Fatal("plaintext in stored body")
	}
	if got, _, err := b.Read(ctx, path); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("other replica cannot open: %v", err)
	}
	for _, changed := range []struct{ path, owner string }{
		{"users/u-a/groups.json", "u-a"},
		{"users/u-other/contacts.json", "u-other"},
		{"users/u-b/contacts.json", "u-b"},
	} {
		if _, err := db.Exec(`INSERT INTO documents(root,path,owner,body,updated_at) VALUES('state',?,?,?,0)`, changed.path, changed.owner, raw); err != nil {
			t.Fatal(err)
		}
		if body, _, err := b.Read(ctx, changed.path); err == nil || body != nil {
			t.Fatal("copied body opened", changed.path, err)
		}
	}
	if _, err := db.Exec(`UPDATE documents SET root='config' WHERE path=?`, path); err != nil {
		t.Fatal(err)
	}
	other, err := vol.NewTable(db, "config", vol.TableOptions{MasterKey: options.MasterKey, Org: options.Org})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := other.Read(ctx, path); err == nil {
		t.Fatal("body moved between roots opened", err)
	}
	// Removing owner metadata must not turn a D document into cleartext.
	if _, err := db.Exec(`UPDATE documents SET owner=NULL WHERE path=?`, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := other.Read(ctx, path); err == nil {
		t.Fatal("owner downgrade accepted", err)
	}
}

func TestConcurrentOrgKeyCreationAndCorruption(t *testing.T) {
	a, b, db := twoTables(t, "state")
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, table := range []*vol.Table{a, b} {
		wg.Go(func() {
			<-start
			p := []string{"users/u-a/contacts.json", "users/u-a/groups.json"}[i]
			if _, err := table.Write(ctx, p, []byte("secret"), 0); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM doc_keys`).Scan(&n); err != nil || n != 1 {
		t.Fatal("keys", n, err)
	}
	for _, p := range []string{"users/u-a/contacts.json", "users/u-a/groups.json"} {
		if body, _, err := a.Read(ctx, p); err != nil || string(body) != "secret" {
			t.Fatal("replicas chose different keys", err)
		}
	}
	if _, err := db.Exec(`UPDATE doc_keys SET wrapped=?`, []byte("corrupt")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Read(ctx, "users/u-a/contacts.json"); err == nil {
		t.Fatal("corrupt key read", err)
	}
	if _, err := b.Write(ctx, "users/u-a/rules.json", []byte("secret"), 0); err == nil {
		t.Fatal("corrupt key replaced", err)
	}
	var raw []byte
	if err := db.QueryRow(`SELECT wrapped FROM doc_keys`).Scan(&raw); err != nil || string(raw) != "corrupt" {
		t.Fatal("corrupt key was rewritten", err)
	}
}

func TestPlatformDocumentsRemainReadableAndKeyIsRequired(t *testing.T) {
	a, _, db := twoTables(t, "config")
	plain := []byte("timezone: UTC\n")
	if _, err := a.Write(ctx, "config.yaml", plain, 0); err != nil {
		t.Fatal(err)
	}
	var body []byte
	if err := db.QueryRow(`SELECT body FROM documents`).Scan(&body); err != nil || !bytes.Equal(body, plain) {
		t.Fatal("platform document changed", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM doc_keys`).Scan(&n); err != nil || n != 0 {
		t.Fatal("platform write created an org key", err)
	}
	for _, key := range [][]byte{nil, make([]byte, 31), make([]byte, 33)} {
		_, err := vol.NewTable(db, "config", vol.TableOptions{MasterKey: key, Org: tableOptions().Org})
		if err == nil || !strings.Contains(err.Error(), "documents.key") {
			t.Fatal("missing or invalid provisioned key accepted", err)
		}
	}
}

func TestMailboxAndPickupOwnersAreRequired(t *testing.T) {
	options := tableOptions()
	options.Owner = func(_ context.Context, path string, body []byte) (string, error) {
		if strings.HasPrefix(path, "mailboxes/") {
			return "owner-a", nil
		}
		if string(body) == `{"senderUserId":"owner-a"}` {
			return "owner-a", nil
		}
		return "", errors.New("unknown pickup owner")
	}
	a, _, db := twoTablesWithOptions(t, "state", options)
	for _, p := range []string{"mailboxes/mbx-a/native-mailbox.json", "pickup/id-a.json"} {
		plain := []byte(`{"senderUserId":"owner-a"}`)
		if _, err := a.Write(ctx, p, plain, 0); err != nil {
			t.Fatal(err)
		}
		var own string
		if err := db.QueryRow(`SELECT owner FROM documents WHERE path=?`, p).Scan(&own); err != nil || own != "owner-a" {
			t.Fatal("wrong owner", own, err)
		}
		if got, _, err := a.Read(ctx, p); err != nil || !bytes.Equal(got, plain) {
			t.Fatal("cannot read owner-bound record", err)
		}
	}
}
