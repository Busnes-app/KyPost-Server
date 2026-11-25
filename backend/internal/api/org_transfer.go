package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// An org runs one mail import or export at a time across replicas (K "Abuse limits", MH6):
// a lease on database time in the shared store, renewed while the transfer runs and given
// back when it ends, so a replica that dies frees it within orgTransferLease. Single
// installs, and accounts without an org, take none.

var (
	orgTransferLease = 2 * time.Minute
	orgTransferRenew = 30 * time.Second
)

const orgTransferBusy = "another import or export for your organization is running; try again when it finishes"

// takeOrgTransfer holds userID's org transfer lease, or refuses with a status and the
// user's text. release is idempotent.
func (s *Server) takeOrgTransfer(userID string) (release func(), status int, refusal string) {
	none := func() {}
	if s.shared == nil || s.tenancy == nil || userID == "" {
		return none, 0, ""
	}
	u, err := s.users.Get(userID)
	if err != nil {
		return nil, http.StatusServiceUnavailable, "imports and exports are unavailable right now; try again in a moment"
	}
	if u.OrgID == "" {
		return none, 0, ""
	}
	sum := sha256.Sum256([]byte(u.OrgID))
	key, holder := "org-transfer:"+hex.EncodeToString(sum[:]), rand.Text()
	ctx, cancel := sharedContext()
	won, err := s.shared.TakeLease(ctx, key, holder, orgTransferLease)
	cancel()
	switch {
	case err != nil:
		return nil, http.StatusServiceUnavailable, "imports and exports are unavailable right now; try again in a moment"
	case !won:
		return nil, http.StatusConflict, orgTransferBusy
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(orgTransferRenew)
		defer t.Stop()
		for {
			select {
			case <-stop:
				ctx, cancel := sharedContext()
				_ = s.shared.UndoReplay(ctx, key, holder)
				cancel()
				return
			case <-t.C:
				ctx, cancel := sharedContext()
				if ok, err := s.shared.TakeLease(ctx, key, holder, orgTransferLease); err != nil || !ok {
					s.logger.Warn("org transfer lease not renewed", "actor", userID)
				}
				cancel()
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(stop); <-done }) }, 0, ""
}
