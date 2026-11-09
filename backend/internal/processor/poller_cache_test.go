package processor

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

func TestPollerStateCacheBoundPreservesHeldStores(t *testing.T) {
	p := &Poller{stateDir: t.TempDir(), stores: map[string]*state.Store{}}
	held := map[string]*state.Store{}
	for i := 0; i < maxRetainedPollerResources+8; i++ {
		id := fmt.Sprintf("user-%d", i)
		st, err := p.userStore(id)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		if err := st.SetCheckpoint(id); err != nil {
			t.Fatal(err)
		}
		held[id] = st
		if len(p.stores) > maxRetainedPollerResources {
			t.Fatal("state cache exceeds bound")
		}
	}
	var wg sync.WaitGroup
	for id, st := range held {
		if _, cached := p.stores[id]; cached {
			continue
		}
		// An evicted caller still owns a usable database; an independent
		// opener must see its acknowledged writes and subsequent updates.
		wg.Go(func() {
			if checkpoint, err := st.Checkpoint(); err != nil || checkpoint != id {
				t.Errorf("held state %q: %q %v", id, checkpoint, err)
				return
			}
			other, err := state.New(filepath.Join(p.stateDir, "users", id))
			if err != nil {
				t.Error(err)
				return
			}
			defer other.Close()
			if err := other.SetCheckpoint("independent-" + id); err != nil {
				t.Error(err)
				return
			}
			if checkpoint, err := st.Checkpoint(); err != nil || checkpoint != "independent-"+id {
				t.Errorf("independent write invisible: %q %v", checkpoint, err)
			}
		})
	}
	wg.Wait()
}
