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

// messageClaimTTL bounds a claim a host never released (it died or froze): the per-message
// work it covers is bounded by perMessageTimeout.
var messageClaimTTL = perMessageTimeout + time.Minute

// beforeClaim runs as a claim is taken: a test pauses a host there, after its first Seen.
var beforeClaim = func() {}

// claimsOn is users.TableMode; a variable so a test can turn claims on without volumes.
var claimsOn = users.TableMode

// messageClaims fences one batch's messages across hosts in table mode (MH4). The daemon
// role's election is single ownership, but a holder frozen past its grace can resume beside
// the new owner, and the processed row is written only at the end, so both would label,
// reply and push. A claim is a lease (state.TakeLease) on database time in the polled
// mailbox's own store, keyed by user and message. Every decision write renews it in its own
// transaction (state.RecordProcessedDecisionUnder), so a host whose claim was taken over
// records nothing, and pushes nothing, since every push follows a record. Claims are given
// back when the batch ends. Ordinary installs take none.
type messageClaims struct {
	on    bool
	token string
	held  []claimed
}

type claimed struct {
	store *state.Store
	key   string
}

func (p *Poller) messageClaims() *messageClaims {
	if p.globalStore == nil || !claimsOn(p.configDir) {
		return &messageClaims{}
	}
	return &messageClaims{on: true, token: rand.Text()}
}

// take claims userID's message in store for this batch. A nil lease with ok is "no claims";
// ok false is another host's live claim or a store that cannot answer, never "go ahead".
func (c *messageClaims) take(store *state.Store, userID, messageID string) (lease *state.Lease, ok bool) {
	if !c.on {
		return nil, true
	}
	sum := sha256.Sum256([]byte(userID + "\x00" + messageID))
	lease = &state.Lease{Key: "poller-message:" + hex.EncodeToString(sum[:]), Holder: c.token, Window: messageClaimTTL}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	beforeClaim()
	if won, err := store.TakeLease(ctx, lease.Key, lease.Holder, lease.Window); err != nil || !won {
		return nil, false
	}
	c.held = append(c.held, claimed{store, lease.Key})
	return lease, true
}

// release hands back this batch's claims; a claim another host took over is not this one's.
func (c *messageClaims) release() {
	for _, h := range c.held {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = h.store.UndoReplay(ctx, h.key, c.token)
		cancel()
	}
}

// record is RecordProcessedDecision under the message's claim: the commit point every
// terminal path, and so every push after it, goes through.
func (uc userCtx) record(d state.Decision) error {
	return uc.store.RecordProcessedDecisionUnder(uc.claim, d)
}
