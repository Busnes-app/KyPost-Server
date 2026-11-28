package app

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/s3"
	"github.com/Busnes-app/ky-primitives/suitedir"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/vol/objecttest"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

const (
	bodyIssuer   = "https://id.example.test"
	bodyStaffOrg = "1a2b3c4d-0000-4000-8000-0000000000ff"
	bodyOrgC     = "1a2b3c4d-0000-4000-8000-00000000000c"
)

// bodyPlacement installs useObjectBodies over client for cells, with org A in cells[0], org
// B in cells[len-1], org C in a cell not served and the staff org homed outside them all,
// and returns the placement.
func bodyPlacement(t *testing.T, client *s3.Client, cells []string, stateDir string) func(mailbox.Owner) (*vol.Object, mailbox.BodyScope, error) {
	t.Helper()
	bodyOrgs(t, cells, stateDir)
	_, release, err := useObjectBodies(client, bodyStaffOrg, cells, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return placeBodies(client, placing.db, bodyStaffOrg, cells, stateDir)
}

// bodyOrgs creates stateDir's tenancy.db with the orgs bodyPlacement describes.
func bodyOrgs(t *testing.T, cells []string, stateDir string) {
	t.Helper()
	ctx := context.Background()
	db, err := tenancy.Open(stateDir, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for org, cell := range map[string]string{receivingOrgA: cells[0], receivingOrgB: cells[len(cells)-1], bodyOrgC: "elsewhere-1", bodyStaffOrg: "elsewhere-2"} {
		if err := tenancy.ApplyOrg(ctx, db, suitedir.Org{ID: org, Status: suitedir.OrgActive, Revision: 1, Enabled: true, HomeRegion: cell}, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := tenancy.BindSubject(ctx, db, bodyIssuer, "sub-"+org, org); err != nil {
			t.Fatal(err)
		}
	}
}

// Overlapping runtimes (the api and a receiving runtime): the first release and the close of
// its caller's own tenancy handle leave the hook working for the one still running.
func TestObjectBodiesOutliveAnOverlappingRelease(t *testing.T) {
	client, err := s3.New(s3.Config{Endpoint: "https://s3.invalid", Bucket: "kypost-test", AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	cells := []string{"cell-a"}
	stateDir := t.TempDir()
	bodyOrgs(t, cells, stateDir)
	own, err := tenancy.Open(stateDir, false)
	if err != nil {
		t.Fatal(err)
	}
	_, first, err := useObjectBodies(client, bodyStaffOrg, cells, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := useObjectBodies(client, bodyStaffOrg, cells, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer second()
	first()
	_ = own.Close()
	if _, scope, err := placeBodies(client, placing.db, bodyStaffOrg, cells, stateDir)(bodyOwner(receivingOrgA)); err != nil || scope.Org != receivingOrgA {
		t.Fatalf("placement after the first release: %+v %v", scope, err)
	}
}

func bodyOwner(org string) mailbox.Owner {
	return mailbox.Owner{Issuer: bodyIssuer, Subject: "sub-" + org, Mailbox: "mb-" + org[len(org)-1:]}
}

// Each mailbox is placed in its org's cell, under its org; an org of a cell this deployment
// does not serve, or a subject with no org, cannot open a store (cross-cell); and a store's
// Object opens only its own org's key (cross-org) and only its own cell.
func TestObjectBodiesPlaceEachMailboxInItsOrgsCell(t *testing.T) {
	client, err := s3.New(s3.Config{Endpoint: "https://s3.invalid", Bucket: "kypost-test", AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	cells := []string{"cell-a", "cell-b"}
	place := bodyPlacement(t, client, cells, t.TempDir())
	for org, cell := range map[string]string{receivingOrgA: "cell-a", receivingOrgB: "cell-b", bodyStaffOrg: "cell-a"} {
		o, scope, err := place(bodyOwner(org))
		if err != nil || scope != (mailbox.BodyScope{Cell: cell, Org: org}) || o == nil {
			t.Fatalf("%s: %+v %v, want cell %s", org, scope, err, cell)
		}
	}
	for name, owner := range map[string]mailbox.Owner{
		"org of another cell": bodyOwner(bodyOrgC),
		"no org":              {Issuer: bodyIssuer, Subject: "unbound", Mailbox: "mb-x"},
	} {
		if _, _, err := place(owner); !errors.Is(err, tenancy.ErrOrgCell) {
			t.Fatalf("%s placed: %v", name, err)
		}
	}
	o, _, err := place(bodyOwner(receivingOrgA))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Refused before any request, so s3.invalid is never contacted.
	if _, err := o.Write(ctx, vol.BodyPrefix("cell-a", receivingOrgB, "mb-b")+"0123", []byte("x"), 0); !errors.Is(err, errBodyOrg) {
		t.Fatalf("org A's store sealed under org B's key: %v", err)
	}
	if _, err := o.Write(ctx, vol.BodyPrefix("cell-b", receivingOrgA, "mb-a")+"0123", []byte("x"), 0); !errors.Is(err, vol.ErrCell) {
		t.Fatalf("org A's store wrote into cell B: %v", err)
	}
}

// The real path on PostgreSQL and the S3 fixture: a large body a multi store writes lands in
// cells/<cell>/bodies/<org>/<mailbox>/, sealed under the org key OrgKey made under the owner
// guard, and reads back.
func TestObjectBodiesEndToEnd(t *testing.T) {
	dbbackend.RequirePostgres(t)
	cell := objecttest.Cell()
	client := objecttest.Client(t)
	stateDir := t.TempDir()
	db, err := volumes.Open(stateDir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	accounts, err := volumes.OpenAccounts(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer accounts.Close()
	if _, err := accounts.Exec(`INSERT INTO accounts(id,username,issuer,subject,record) VALUES('u-a','u-a',?,?,?)`, bodyIssuer, "sub-"+receivingOrgA, []byte(`{"id":"u-a","orgId":"`+receivingOrgA+`"}`)); err != nil {
		t.Fatal(err)
	}
	table, err := vol.NewTable(db, "state", vol.TableOptions{MasterKey: make([]byte, 32), Org: func(context.Context, string) (string, error) { return receivingOrgA, nil }})
	if err != nil {
		t.Fatal(err)
	}
	unmount, err := vol.Mount(stateDir, table)
	if err != nil {
		t.Fatal(err)
	}
	defer unmount()
	bodyPlacement(t, client, []string{cell}, stateDir)

	ctx := context.Background()
	marker := "board-minutes-" + cell
	raw := append([]byte("From: a@outside.test\r\nTo: a@example.test\r\nSubject: x\r\n\r\n"), bytes.Repeat([]byte(marker+"\r\n"), mailbox.ObjectThreshold/len(marker)+1)...)
	owner := bodyOwner(receivingOrgA)
	store, err := mailbox.Open(filepath.Join(stateDir, "mailbox-a"), owner, mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 1 << 30, Records: 1000})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id, err := store.Import(ctx, mailbox.Receipt{Gateway: "maddy", Delivery: "r1", Sender: "a@outside.test", Recipients: []mailbox.Recipient{{Address: "a@example.test", Generation: 1}}}, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	prefix := vol.BodyPrefix(cell, receivingOrgA, owner.Mailbox)
	all, err := vol.NewObject(client, []string{cell}, nil)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := all.List(ctx, prefix)
	if err != nil || len(keys) != 1 {
		t.Fatalf("objects under %s: %v %v", prefix, keys, err)
	}
	rc, _, err := client.Get(ctx, keys[0], "")
	if err != nil {
		t.Fatal(err)
	}
	var sealed bytes.Buffer
	_, _ = sealed.ReadFrom(rc)
	_ = rc.Close()
	if strings.Contains(sealed.String(), marker) {
		t.Fatal("the bucket holds plaintext")
	}
	got, err := store.Raw(ctx, "INBOX", id)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("read back: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM doc_keys WHERE org_id=?`, receivingOrgA).Scan(&n); err != nil || n != 1 {
		t.Fatalf("org A's key: %d %v", n, err)
	}
}
