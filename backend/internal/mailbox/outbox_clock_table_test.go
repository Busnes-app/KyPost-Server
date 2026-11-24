package mailbox

import (
	"bytes"
	"context"
	"errors"
	"net/textproto"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

func TestTableOutboxRetryWindowUsesDatabaseClock(t *testing.T) {
	dbbackend.RequirePostgres(t)
	root := t.TempDir()
	db, err := volumes.Open(root, true)
	must(t, err)
	defer db.Close()
	table, err := vol.NewTable(db, "state", vol.TableOptions{MasterKey: bytes.Repeat([]byte{3}, 32), Org: func(context.Context, string) (string, error) { return "", nil }})
	must(t, err)
	unmount, err := vol.Mount(root, table)
	must(t, err)
	defer unmount()
	dir := filepath.Join(root, "mailbox")
	fast := openTest(t, dir, testOwner, testLimits)
	must(t, fast.QueueOutbound(t.Context(), outboxMaster, outboxTestID, outboxTestJob()))
	realNow := time.Now()
	var originalClaim string
	completeRetry := func(t *testing.T, store *Store, claim string, delay int64) {
		t.Helper()
		var before, after, next int64
		must(t, store.db.QueryRow(`SELECT unixepoch()`).Scan(&before))
		must(t, store.CompleteOutbound(t.Context(), outboxMaster, outboxTestID, 0, claim, &textproto.Error{Code: 451, Msg: "temporary test refusal"}))
		must(t, store.db.QueryRow(`SELECT unixepoch(),next_attempt FROM outbox_deliveries WHERE job=? AND sequence=0`, outboxTestID).Scan(&after, &next))
		if next < before+delay || next > after+delay {
			t.Fatalf("retry window used replica clock: next=%d database=[%d,%d] delay=%d", next, before, after, delay)
		}
	}
	synctest.Test(t, func(t *testing.T) {
		slow, err := Open(dir, testOwner, testLimits)
		must(t, err)
		defer slow.Close()
		if time.Until(realNow) < time.Hour {
			t.Fatal("test clocks are not skewed beyond backoff")
		}
		_, claim, err := slow.ClaimOutbound(t.Context(), outboxMaster, outboxTestID, 0, outboxGeneration)
		must(t, err)
		originalClaim = claim
		completeRetry(t, slow, claim, 30)
	})
	if _, _, err := fast.ClaimOutbound(t.Context(), outboxMaster, outboxTestID, 0, outboxGeneration); !errors.Is(err, ErrOutbound) {
		t.Fatal("another replica stole a retry before its database deadline", err)
	}
	_, err = fast.db.Exec(`UPDATE outbox_deliveries SET next_attempt=unixepoch()+600 WHERE job=?`, outboxTestID)
	must(t, err)
	synctest.Test(t, func(t *testing.T) {
		future, err := Open(dir, testOwner, testLimits)
		must(t, err)
		defer future.Close()
		time.Sleep(80 * 365 * 24 * time.Hour)
		if !time.Now().After(realNow.Add(10 * time.Minute)) {
			t.Fatal("future clock did not advance beyond retry")
		}
		ids, err := future.PendingOutbound(t.Context(), 10)
		must(t, err)
		if len(ids) != 0 {
			t.Fatal("fast clock discovered future retry", ids)
		}
		pending, err := HasPendingOutbound(t.Context(), dir)
		must(t, err)
		if pending {
			t.Fatal("fast probe discovered future retry")
		}
		if _, _, err := future.ClaimOutbound(t.Context(), outboxMaster, outboxTestID, 0, outboxGeneration); !errors.Is(err, ErrOutbound) {
			t.Fatal("fast clock claimed future retry", err)
		}
	})
	_, err = fast.db.Exec(`UPDATE outbox_deliveries SET next_attempt=unixepoch()-1 WHERE job=?`, outboxTestID)
	must(t, err)
	synctest.Test(t, func(t *testing.T) {
		slow, err := Open(dir, testOwner, testLimits)
		must(t, err)
		defer slow.Close()
		ids, err := slow.PendingOutbound(t.Context(), 10)
		must(t, err)
		if len(ids) != 1 || ids[0] != outboxTestID {
			t.Fatal("slow clock hid a database-due retry", ids)
		}
		pending, err := HasPendingOutbound(t.Context(), dir)
		must(t, err)
		if !pending {
			t.Fatal("slow probe hid database-due retry")
		}
		_, claim, err := slow.ClaimOutbound(t.Context(), outboxMaster, outboxTestID, 0, outboxGeneration)
		must(t, err)
		if claim == originalClaim {
			t.Fatal("retry reused immutable claim token")
		}
		if err := slow.CompleteOutbound(t.Context(), outboxMaster, outboxTestID, 0, originalClaim, nil); !errors.Is(err, ErrOutbound) {
			t.Fatal("stale completion changed retry", err)
		}
		completeRetry(t, slow, claim, 60)
	})
	_, states, _, err := fast.ReadOutbound(t.Context(), outboxMaster, outboxTestID)
	must(t, err)
	if len(states) != 1 || states[0].State != "retryable" || states[0].Attempts != 2 {
		t.Fatal("refused admission consumed attempts", states)
	}
}

func TestUnmigratedOutboxRetryKeepsReplicaClock(t *testing.T) {
	dbbackend.RequirePostgres(t)
	dir := filepath.Join(t.TempDir(), "mailbox")
	fast := openTest(t, dir, testOwner, testLimits)
	must(t, fast.QueueOutbound(t.Context(), outboxMaster, outboxTestID, outboxTestJob()))
	synctest.Test(t, func(t *testing.T) {
		slow, err := Open(dir, testOwner, testLimits)
		must(t, err)
		defer slow.Close()
		_, claim, err := slow.ClaimOutbound(t.Context(), outboxMaster, outboxTestID, 0, outboxGeneration)
		must(t, err)
		before := time.Now().Unix()
		must(t, slow.CompleteOutbound(t.Context(), outboxMaster, outboxTestID, 0, claim, &textproto.Error{Code: 451, Msg: "temporary test refusal"}))
		var next int64
		must(t, slow.db.QueryRow(`SELECT next_attempt FROM outbox_deliveries WHERE job=?`, outboxTestID).Scan(&next))
		if next < before+30 || next > time.Now().Unix()+30 {
			t.Fatal("ordinary mode changed its time source", next, before)
		}
	})
}
