package suitepair

import (
	"errors"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// AC8: an event ID accepted on one replica is refused on another; a failed attempt's undo
// releases only its own record; a store failure refuses.
func TestReplayIsSharedAcrossReplicas(t *testing.T) {
	dir := t.TempDir()
	stores := make([]*state.Store, 3)
	for i := range stores {
		st, err := state.New(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		stores[i] = st
	}
	a, b := newClient(Peer{ID: "kymessage", Name: "KyMessages", Env: "KYPOST_KYMESSAGE"}, nil), newClient(Peer{ID: "kymessage", Name: "KyMessages", Env: "KYPOST_KYMESSAGE"}, nil)
	a.ShareReplay(stores[0])
	b.ShareReplay(stores[1])
	now := time.Now()
	if err := a.replay.Check("ev-1", now); err != nil {
		t.Fatal(err)
	}
	if err := b.replay.Check("ev-1", now); !errors.Is(err, syncauth.ErrReplay) {
		t.Fatalf("B accepted A's event: %v", err)
	}
	b.Forget("ev-1") // B's attempt failed: its undo must not release A's record
	if err := b.replay.Check("ev-1", now); !errors.Is(err, syncauth.ErrReplay) {
		t.Fatalf("B's forget released A's record: %v", err)
	}
	a.Forget("ev-1")
	if err := b.replay.Check("ev-1", now); err != nil {
		t.Fatalf("A's own forget did not release the retry: %v", err)
	}
	if a.replay.window <= 0 {
		t.Fatal("no window")
	}
	c := newClient(Peer{ID: "kymessage", Name: "KyMessages", Env: "KYPOST_KYMESSAGE"}, nil)
	c.ShareReplay(stores[2])
	_ = stores[2].Close()
	if err := c.replay.Check("ev-2", now); !errors.Is(err, syncauth.ErrReplayGuardFull) {
		t.Fatalf("a failing store did not refuse: %v", err)
	}
}
