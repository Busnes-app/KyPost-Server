package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/vol/objecttest"
)

// TestOrgPurgeDeletesObjectBodies: the purge deletes every message body of the org's
// mailboxes and its org prefix in every served cell, leaves another org's, and its final
// check refuses an object left behind (MH11).
func TestOrgPurgeDeletesObjectBodies(t *testing.T) {
	px := newPurgeFixture(t)
	cells := []string{objecttest.Cell(), objecttest.Cell()}
	px.srv.objects, _ = objecttest.New(t, cells...)
	px.srv.cells = cells
	ctx := context.Background()
	put := func(cell, org string, mailbox ...string) string {
		t.Helper()
		id, err := vol.NewObjectID()
		if err != nil {
			t.Fatal(err)
		}
		key := vol.BodyPrefix(cell, org, mailbox...) + id
		if len(mailbox) == 0 {
			key = vol.BodyPrefix(cell, org, "gone-mailbox") + id
		}
		if _, err := px.srv.objects.Write(ctx, key, []byte("sealed body"), 0); err != nil {
			t.Fatal(err)
		}
		return key
	}
	for _, cell := range cells {
		put(cell, orgA, px.a.ID)
		put(cell, orgA, px.a.extra)
		put(cell, orgA) // a mailbox no snapshot names: the org prefix covers it
	}
	keepB := put(cells[0], orgB, px.b.ID)

	setOrg(t, px.db, orgA, "deleted", false, `{}`)
	purgeStepHook = func(step int) error {
		if step == -3 { // a body written after step 3's delete
			put(cells[1], orgA, px.a.ID)
		}
		return nil
	}
	defer func() { purgeStepHook = nil }()
	if err := px.srv.runPurges(ctx, time.Now()); !errors.Is(err, errPurgeLeft) || !strings.Contains(err.Error(), "objects: ") {
		t.Fatal("final check passed with an object left:", err)
	}
	purgeStepHook = nil
	if err := px.srv.runPurges(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	px.assertPurged(t)
	for _, cell := range cells {
		if keys, err := px.srv.objects.List(ctx, vol.BodyPrefix(cell, orgA)); err != nil || len(keys) != 0 {
			t.Fatalf("cell %s keeps org A's objects: %v %v", cell, keys, err)
		}
	}
	if keys, err := px.srv.objects.List(ctx, vol.BodyPrefix(cells[0], orgB, px.b.ID)); err != nil || len(keys) != 1 || keys[0] != keepB {
		t.Fatal("org B's object was touched:", keys, err)
	}
}
