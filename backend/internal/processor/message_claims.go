package processor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// messageClaimTTL bounds a claim a host never released (it died or paused): the per-message
// work it covers is bounded by perMessageTimeout.
const messageClaimTTL = perMessageTimeout + time.Minute

// messageClaims fences one batch's messages across hosts in table mode (MH4): the daemon
// role's election is single ownership, but a holder paused past its grace can resume beside
// the new owner, and the processed row is written only at the end, so both would label,
// reply and push. A claim is a replay record on database time, keyed by user and message and
// released when the batch ends. Ordinary installs take no claims.
type messageClaims struct {
	store *state.Store // nil: no claims
	token string
	keys  []string
}

// claimsOn is users.TableMode; a variable so a test can turn claims on without volumes.
var claimsOn = users.TableMode

func (p *Poller) messageClaims() *messageClaims {
	c := &messageClaims{}
	if p.globalStore != nil && claimsOn(p.configDir) {
		c.store = p.globalStore
		c.token = rand.Text()
	}
	return c
}

// take claims userID's message for this batch; false is another host's live claim or a
// store that cannot answer, never "go ahead".
func (c *messageClaims) take(userID, messageID string) bool {
	if c.store == nil {
		return true
	}
	sum := sha256.Sum256([]byte(userID + "\x00" + messageID))
	key := "poller-message:" + hex.EncodeToString(sum[:])
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	won, err := c.store.RecordReplay(ctx, key, c.token, messageClaimTTL)
	if won && err == nil {
		c.keys = append(c.keys, key)
	}
	return won && err == nil
}

// release hands back this batch's claims; a claim another host took over is not this one's.
func (c *messageClaims) release() {
	for _, key := range c.keys {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.store.UndoReplay(ctx, key, c.token)
		cancel()
	}
}
