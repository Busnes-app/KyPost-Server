package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/suitepair"
)

// Out-of-office to KyMessages' status (KyMessages docs/PRESENCE.md): on or
// off and the period, never the reply text.
const (
	kymessageEventOOO = "kypost.presence.ooo"
	kymessagePathOOO  = "/api/suite/presence/out-of-office"
)

// syncOOOMessages sends KyMessages the user's out-of-office state on every
// save: away while the reply is on with both dates, else off. KyMessages keeps
// the user's "Show my status to colleagues" switch and drops it when that is
// off. Best effort: a failure is logged and the next save sends it again.
func (s *Server) syncOOOMessages(ctx context.Context, userID string, o config.OutOfOffice) {
	if !s.kymessage.Available() {
		return
	}
	u, err := s.users.Get(userID)
	if err != nil || !u.Active || u.SSOSub == "" || u.SSOIssuer == "" || u.SSOLinkRevokedAt != 0 {
		return
	}
	body := map[string]any{"sub": u.SSOSub, "issuer": u.SSOIssuer, "revision": time.Now().UnixMilli(), "away": false}
	if start, end := o.Window(); o.Enabled && !start.IsZero() && !end.IsZero() {
		body["away"], body["start"], body["end"] = true, start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339)
	}
	id, err := fsutil.NewUUIDv4()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, answer, err := s.kymessage.Post(ctx, kymessagePathOOO, kymessageEventOOO, "kypost-ooo-"+id, body)
	if err != nil || status != http.StatusOK {
		s.logger.Info("kymessage out-of-office not taken", "user_id", userID, "result", strconv.Itoa(status)+" "+suitepair.Outcome(answer))
	}
}
