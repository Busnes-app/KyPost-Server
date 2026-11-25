package processor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// messageClaimTTL bounds a claim a host never released (it died or paused): the per-message
// work it covers is bounded by perMessageTimeout.
var messageClaimTTL = perMessageTimeout + time.Minute

// beforeClaim runs as a claim is taken: a test pauses a host there, after its first Seen.
var beforeClaim = func() {}

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

// errMessageClaimLost is a message whose claim this host no longer holds: another host took
// it over, so this one records and pushes nothing.
var errMessageClaimLost = errors.New("message claimed by another host")

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

// take claims userID's message for this batch and returns its check; ok false is another
// host's live claim or a store that cannot answer, never "go ahead". check (nil when no
// claims are taken) renews the claim while this batch still holds it, or reports
// errMessageClaimLost. The poller calls it before every record, so a host frozen past the
// claim's TTL, whose claim another host took, records and pushes nothing when it resumes.
func (c *messageClaims) take(userID, messageID string) (check func() error, ok bool) {
	if c.store == nil {
		return nil, true
	}
	sum := sha256.Sum256([]byte(userID + "\x00" + messageID))
	key := "poller-message:" + hex.EncodeToString(sum[:])
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	beforeClaim()
	if won, err := c.store.TakeLease(ctx, key, c.token, messageClaimTTL); err != nil || !won {
		return nil, false
	}
	check = func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if held, err := c.store.RenewLease(ctx, key, c.token, messageClaimTTL); err != nil || !held {
			return errMessageClaimLost
		}
		return nil
	}
	c.keys = append(c.keys, key)
	return check, true
}

// release hands back this batch's claims; a claim another host took over is not this one's.
func (c *messageClaims) release() {
	for _, key := range c.keys {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.store.UndoReplay(ctx, key, c.token)
		cancel()
	}
}

// record is RecordProcessedDecision under the message's claim: the commit point every
// terminal path, and so every push after it, goes through.
func (uc userCtx) record(d state.Decision) error {
	if uc.claim != nil {
		if err := uc.claim(); err != nil {
			return err
		}
	}
	return uc.store.RecordProcessedDecision(d)
}
