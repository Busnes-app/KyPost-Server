package api

import (
	"context"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
)

// UseObjects sets the deployment's message-body store (MH11, multi only): the purge empties
// its prefixes and StartBodySweeper runs over it.
func (s *Server) UseObjects(o *vol.Object) { s.objects = o }

// StartBodySweeper runs each mailbox's late delete (mailbox.SweepBodies) hourly, on the
// elected api-sweepers worker. Without object storage it returns at once.
func (s *Server) StartBodySweeper(ctx context.Context) {
	if s.objects == nil {
		return
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if deleted := s.sweepBodies(ctx); deleted > 0 {
				s.logger.Info("deleted message bodies past their retention", "removed", strconv.Itoa(deleted))
			}
		}
	}
}

// sweepBodies is one pass over every native mailbox. One mailbox's failure is logged and
// skipped, so it cannot stop the others' cleanup.
func (s *Server) sweepBodies(ctx context.Context) (deleted int) {
	all, err := s.ssoLifecycle.NativeMailboxesWithoutAddresses()
	if err != nil {
		s.logger.Error("message body sweep could not list mailboxes", "error", err.Error())
		return 0
	}
	for _, m := range all {
		if ctx.Err() != nil {
			return deleted
		}
		if !m.Prepared {
			continue
		}
		a, ok, err := s.ssoLifecycle.NativeMailboxAssignment(m.ID)
		if err == nil && (!ok || a.Source == "") {
			continue
		}
		var box *mailbox.Store
		if err == nil {
			box, err = mailbox.OpenExisting(filepath.Join(a.Dir(s.stateDir), "mailbox"), a.Owner, a.Limits, a.Source)
		}
		if err == nil {
			var n int
			n, err = box.SweepBodies(ctx)
			deleted += n
			_ = box.Close()
		}
		if err != nil {
			s.logger.Error("message body sweep failed for a mailbox", "mailbox", m.ID, "error", err.Error())
		}
	}
	return deleted
}
