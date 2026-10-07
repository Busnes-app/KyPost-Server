//go:build linux

package backup

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// Exercise the collector and sealed restore while committed rows exist only in WAL.
func TestNativeDatabasesSurviveSealedRestore(t *testing.T) {
	s, u := nativeService(t)
	key := pinTestKey(t, s)
	paths := []string{"users/" + u.ID + "/mailbox/mailbox.db", "receiving/ingress.db"}
	want := []byte("Content-Type: application/pgp-encrypted\r\n\r\nopaque bytes\x00\xff")
	for _, rel := range paths {
		path := filepath.Join(s.dirs.State, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if filepath.Base(path) == "ingress.db" {
			store, err := ingress.Open(filepath.Dir(path), ingress.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
		}
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		db.SetMaxOpenConns(1)
		if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;
			CREATE TABLE probe (id INTEGER PRIMARY KEY, raw BLOB, receipt TEXT);
			PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO probe VALUES (17, ?, 'committed-receipt')`, want); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path + "-wal")
		if err != nil || info.Size() == 0 {
			t.Fatalf("test requires an uncheckpointed WAL: %v", err)
		}
		// Prove copying the main file alone loses this committed row.
		main, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		copyPath := filepath.Join(t.TempDir(), "main.db")
		if err := os.WriteFile(copyPath, main, 0600); err != nil {
			t.Fatal(err)
		}
		copyDB, err := sql.Open("sqlite", copyPath)
		if err != nil {
			t.Fatal(err)
		}
		var count int
		err = copyDB.QueryRow(`SELECT count(*) FROM probe`).Scan(&count)
		copyDB.Close()
		if err != nil || count != 0 {
			t.Fatalf("test row was checkpointed: count=%d err=%v", count, err)
		}
	}
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(res.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "restored")
	manifest, _, err := capsule.Open(raw, key, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range manifest.Files {
		if strings.HasSuffix(file.Path, "-wal") || strings.HasSuffix(file.Path, "-shm") {
			t.Fatalf("snapshot must be standalone: %s", file.Path)
		}
	}
	for _, rel := range paths {
		path := filepath.Join(dir, "state", rel)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		var got []byte
		var receipt string
		err = db.QueryRow(`SELECT raw, receipt FROM probe WHERE id=17`).Scan(&got, &receipt)
		db.Close()
		if err != nil || string(got) != string(want) || receipt != "committed-receipt" {
			t.Fatalf("restored %s lost committed bytes/receipt: %v", rel, err)
		}
	}
	for _, c := range drillChecks(dir, manifest) {
		if !c.Passed {
			t.Fatalf("restored check failed: %s", c.Name)
		}
	}
	for _, rel := range paths {
		name := "state/" + rel
		if err := os.WriteFile(filepath.Join(dir, name), []byte("corrupt"), 0600); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, c := range drillChecks(dir, manifest) {
			if c.Name == "sqlite:"+name {
				found = true
				if c.Passed {
					t.Fatalf("corrupt %s passed drill", rel)
				}
			}
		}
		if !found {
			t.Fatalf("drill did not inspect %s", rel)
		}
	}
}

func TestNativeStoreBackupPreservesIdentityAndReplay(t *testing.T) {
	ctx := context.Background()
	s, u := nativeService(t)
	key := pinTestKey(t, s)
	a, found, err := sso.NewLifecycleStore(s.dirs.Config).NativeAssignment(u.NativeMailboxIssuer, u.SSOSub)
	if err != nil || !found {
		t.Fatal("missing fixture reservation", err)
	}
	owner, ml := a.Owner, a.Limits
	il := ingress.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100}
	m, err := mailbox.Open(filepath.Join(s.dirs.State, "users", u.ID, "mailbox"), owner, ml)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	g, err := ingress.Open(filepath.Join(s.dirs.State, "receiving"), il)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	// Routes carry the ledger address generation; restore validation refuses
	// one the ledger never reached.
	addresses, err := sso.NewLifecycleStore(s.dirs.Config).NativeAddresses()
	if err != nil {
		t.Fatal(err)
	}
	route := ingress.Route{Address: a.Address, Issuer: owner.Issuer, Subject: owner.Subject, Mailbox: owner.Mailbox, Generation: addresses[a.Address].Generation, Active: true, ValidUntil: time.Now().Add(time.Hour)}
	if err := g.SetRoute(ctx, route); err != nil {
		t.Fatal(err)
	}
	if err := g.Bind(ctx, "gateway", "delivery", "sender@example.com", route.Address); err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: sender@example.com\r\nTo: alice@example.com\r\nSubject: exact MIME\r\nContent-Type: application/octet-stream\r\n\r\nopaque\x00\xff")
	if err := g.Accept(ctx, "gateway", "delivery", "sender@example.com", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	// An acknowledged delivery survives backup only as its replay tombstone.
	if err := g.Bind(ctx, "gateway", "archived", "sender@example.com", route.Address); err != nil {
		t.Fatal(err)
	}
	if err := g.Accept(ctx, "gateway", "archived", "sender@example.com", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	done, err := g.Claim(ctx, "gateway", "archived", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Acknowledge(ctx, "gateway", "archived", done.Lease, done.Digest); err != nil {
		t.Fatal(err)
	}
	receipt := mailbox.Receipt{Gateway: "gateway", Delivery: "delivery", Sender: "sender@example.com", Recipients: []mailbox.Recipient{{Address: route.Address, Generation: route.Generation}}}
	id, err := m.Import(ctx, receipt, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Update(ctx, "INBOX", id, true, true, []string{"Primary"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := os.ReadFile(res.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "restored")
	if _, _, err := capsule.Open(sealed, key, dir); err != nil {
		t.Fatal(err)
	}
	restored, err := mailbox.Open(filepath.Join(dir, "state/users", u.ID, "mailbox"), owner, ml)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	originalClient, err := mailbox.NewClient(m, route.Address)
	if err != nil {
		t.Fatal(err)
	}
	restoredClient, err := mailbox.NewClient(restored, route.Address)
	if err != nil {
		t.Fatal(err)
	}
	if originalClient.MailSourceIdentity() != restoredClient.MailSourceIdentity() {
		t.Fatal("restored mailbox namespace changed")
	}
	gotID, err := restored.Import(ctx, receipt, bytes.NewReader(raw))
	if err != nil || gotID != id {
		t.Fatalf("restore lost replay receipt: id=%d err=%v", gotID, err)
	}
	got, err := restored.Raw(ctx, "INBOX", id)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("restore changed raw MIME: %v", err)
	}
	rows, err := restored.List(ctx, "INBOX", 0, 100)
	if err != nil || len(rows) != 1 || !rows[0].Seen || !rows[0].Starred || len(rows[0].Labels) != 1 || rows[0].Labels[0] != "Primary" {
		t.Fatalf("restore lost metadata: %+v %v", rows, err)
	}
	receiver, err := ingress.Open(filepath.Join(dir, "state/receiving"), il)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	delivery, err := receiver.Claim(ctx, "gateway", "delivery", time.Minute)
	if err != nil || !bytes.Equal(delivery.Raw, raw) || len(delivery.Bindings) != 1 || delivery.Bindings[0].Issuer != owner.Issuer || delivery.Bindings[0].Subject != owner.Subject || delivery.Bindings[0].Mailbox != owner.Mailbox || delivery.Bindings[0].Generation != route.Generation {
		t.Fatalf("restore lost receiving bytes/route binding: %v", err)
	}
	if archived, err := receiver.Get(ctx, "gateway", "archived"); err != nil || archived.State != "archived" || archived.Digest != done.Digest {
		t.Fatalf("restore lost archived receipt: %+v %v", archived, err)
	}
	if err := receiver.Bind(ctx, "gateway", "archived", "sender@example.com", route.Address); err != nil {
		t.Fatalf("restored tombstone did not recognise replay: %v", err)
	}
	if err := receiver.Accept(ctx, "gateway", "archived", "sender@example.com", bytes.NewReader(raw)); err != nil {
		t.Fatalf("restored tombstone did not recognise replay: %v", err)
	}
}

// The standalone main file can fit the cap while committed WAL rows do not.
// Synthetic probe rows measure database capacity, not accepted mail throughput.
func TestNativeBackupRefusesOversizedWALSnapshot(t *testing.T) {
	for _, name := range []string{"mailbox.db", "ingress.db"} {
		t.Run(name, func(t *testing.T) {
			s, u := nativeService(t)
			pinTestKey(t, s)
			rel := filepath.Join("users", u.ID, "mailbox", name)
			if name == "ingress.db" {
				rel = filepath.Join("receiving", name)
			}
			path := filepath.Join(s.dirs.State, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			if _, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;
				CREATE TABLE capacity_probe (raw BLOB, receipt TEXT); PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`INSERT INTO capacity_probe VALUES (zeroblob(?), 'retain-receipt')`, recoveryclient.MaxCapsuleFileBytes+1); err != nil {
				t.Fatal(err)
			}
			main, err := os.Stat(path)
			if err != nil || main.Size() >= recoveryclient.MaxCapsuleFileBytes {
				t.Fatal("fixture main file must fit the cap", err)
			}
			wal, err := os.Stat(path + "-wal")
			if err != nil || wal.Size() <= recoveryclient.MaxCapsuleFileBytes {
				t.Fatal("fixture requires oversized committed WAL", err)
			}
			result, err := s.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), filepath.ToSlash(rel)) || !strings.Contains(err.Error(), "64 MiB") || result.LocalPath != "" {
				t.Fatalf("oversized snapshot must fail without a capsule: %+v %v", result, err)
			}
			for _, dir := range []string{s.cfg.Dir, filepath.Join(s.dirs.State, scratchDirName)} {
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 0 {
					t.Fatal("failed backup left capsule or scratch", entries, err)
				}
			}
			var size int64
			var receipt string
			if err = db.QueryRow(`SELECT length(raw), receipt FROM capacity_probe`).Scan(&size, &receipt); err != nil || size != recoveryclient.MaxCapsuleFileBytes+1 || receipt != "retain-receipt" {
				t.Fatal("failed backup changed original committed data", size, receipt, err)
			}
		})
	}
}
