package tenancy

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
)

func gatewayDB(t *testing.T) *sql.DB {
	t.Helper()
	st, dir := newStore(t)
	if err := Check(context.Background(), st, dir, config.TenancyMulti, staffA, empty); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func pin(n byte) string { return strings.Repeat(string("0123456789abcdef"[n%16]), 64) }

// RX "Keys": two live pins per ID, revocation and expiry per pin, a retired ID and a used
// key never enrolled again, reserved IDs never at all.
func TestGatewayPins(t *testing.T) {
	ctx := context.Background()
	db := gatewayDB(t)
	now := time.Unix(1_800_000_000, 0)
	lookup := func(spki string, at time.Time) (string, error) {
		id, _, err := GatewayFor(ctx, db, spki, at)
		return id, err
	}
	expires, err := AddGateway(ctx, db, "mx-a", pin(1), now)
	if err != nil || !expires.Equal(now.Add(PinLifetime)) {
		t.Fatal(expires, err)
	}
	if _, err := AddGateway(ctx, db, "mx-a", pin(2), now); err != nil {
		t.Fatal("rotation pin:", err)
	}
	if _, err := AddGateway(ctx, db, "mx-a", pin(3), now); !errors.Is(err, ErrGatewayFull) {
		t.Fatal("third live pin:", err)
	}
	for _, spki := range []string{pin(1), pin(2)} {
		if id, err := lookup(spki, now); err != nil || id != "mx-a" {
			t.Fatal(id, err)
		}
	}
	// Revoking one pin leaves the other; the freed slot takes a new key.
	if err := RevokeGateway(ctx, db, "mx-a", pin(1), now); err != nil {
		t.Fatal(err)
	}
	if err := RevokeGateway(ctx, db, "mx-a", pin(1), now); !errors.Is(err, ErrGatewayNoPin) {
		t.Fatal("second revoke:", err)
	}
	if _, err := lookup(pin(1), now); !errors.Is(err, ErrGatewayUnpinned) {
		t.Fatal("revoked pin:", err)
	}
	if id, err := lookup(pin(2), now); err != nil || id != "mx-a" {
		t.Fatal("other pin:", id, err)
	}
	if _, err := AddGateway(ctx, db, "mx-a", pin(3), now); err != nil {
		t.Fatal("second rotation:", err)
	}
	// A revoked key, or a live one, is never enrolled again under any ID.
	for _, spki := range []string{pin(1), pin(2)} {
		if _, err := AddGateway(ctx, db, "mx-b", spki, now); !errors.Is(err, ErrGatewayKeyUsed) {
			t.Fatal("reused key:", err)
		}
	}
	// Expiry: refused at expires_at, and the expired key stays unusable.
	if _, err := lookup(pin(2), now.Add(PinLifetime)); !errors.Is(err, ErrGatewayUnpinned) {
		t.Fatal("expired pin:", err)
	}
	later := now.Add(PinLifetime + time.Hour)
	if _, err := AddGateway(ctx, db, "mx-a", pin(4), later); err != nil {
		t.Fatal("re-enrol after expiry:", err)
	}
	if _, err := AddGateway(ctx, db, "mx-a", pin(2), later); !errors.Is(err, ErrGatewayKeyUsed) {
		t.Fatal("expired key re-enrolled:", err)
	}
	// Retire: every pin revoked, the ID never takes another.
	if err := RetireGateway(ctx, db, "mx-a", later); err != nil {
		t.Fatal(err)
	}
	if _, err := lookup(pin(4), later); !errors.Is(err, ErrGatewayUnpinned) {
		t.Fatal("retired ID's pin:", err)
	}
	if _, err := AddGateway(ctx, db, "mx-a", pin(5), later); !errors.Is(err, ErrGatewayRetired) {
		t.Fatal("retired ID enrolled:", err)
	}
	if err := RetireGateway(ctx, db, "mx-c", later); !errors.Is(err, ErrGatewayNoPin) {
		t.Fatal("retired an unknown ID:", err)
	}
	for _, id := range append([]string{"", "MX-A", "-mx", "mx_a", strings.Repeat("a", 64)}, ReservedGateways...) {
		if _, err := AddGateway(ctx, db, id, pin(6), now); !errors.Is(err, ErrGatewayID) {
			t.Fatalf("enrolled %q: %v", id, err)
		}
	}
	for _, spki := range []string{"", strings.ToUpper(pin(10)), pin(6)[:63], pin(6) + "0"} {
		if _, err := AddGateway(ctx, db, "mx-d", spki, now); !errors.Is(err, ErrGatewaySPKI) {
			t.Fatalf("enrolled pin %q: %v", spki, err)
		}
	}
	// A reserved ID written behind the CLI's back is still refused.
	if _, err := db.Exec(`INSERT INTO receiving_gateways(id, spki_sha256, created_at, expires_at) VALUES('maddy-local', ?, ?, ?)`, pin(7), now.Unix(), now.Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := lookup(pin(7), now); !errors.Is(err, ErrGatewayUnpinned) {
		t.Fatal("reserved ID admitted:", err)
	}
}

// Pin rows are never deleted, and only ever gain revoked_at and retired_at.
func TestGatewayPinsAreKept(t *testing.T) {
	ctx := context.Background()
	db := gatewayDB(t)
	now := time.Unix(1_800_000_000, 0)
	if _, err := AddGateway(ctx, db, "mx-a", pin(1), now); err != nil {
		t.Fatal(err)
	}
	if err := RevokeGateway(ctx, db, "mx-a", pin(1), now); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DELETE FROM receiving_gateways`,
		`UPDATE receiving_gateways SET revoked_at = NULL`,
		`UPDATE receiving_gateways SET revoked_at = revoked_at + 1`,
		`UPDATE receiving_gateways SET expires_at = expires_at + 1`,
		`UPDATE receiving_gateways SET id = 'mx-b'`,
	} {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("accepted: %s", q)
		}
	}
	if err := RetireGateway(ctx, db, "mx-a", now); err != nil {
		t.Fatal("retire after revoke:", err)
	}
	if _, err := db.Exec(`UPDATE receiving_gateways SET retired_at = NULL`); err == nil {
		t.Error("un-retired an ID")
	}
}

// Concurrent enrolments of one ID never leave more than two live pins.
func TestConcurrentGatewayEnrolment(t *testing.T) {
	ctx := context.Background()
	db := gatewayDB(t)
	now := time.Unix(1_800_000_000, 0)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() { _, _ = AddGateway(ctx, db, "mx-a", pin(byte(i)), now) })
	}
	wg.Wait()
	var live int
	if err := db.QueryRow(`SELECT COUNT(*) FROM receiving_gateways WHERE id = 'mx-a'`).Scan(&live); err != nil || live > 2 || live == 0 {
		t.Fatalf("%d pins (%v), want one or two", live, err)
	}
}
