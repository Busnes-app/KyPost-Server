package processor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"strconv"
	"strings"
	"sync"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/kycalendar"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// Retry bounds for forwards KyCalendar did not take: a few later ticks, then
// give up. ponytail: in memory, so a restart drops pending retries; the mail
// itself stays an ordinary message with a working RSVP card.
const (
	maxForwardAttempts = 3
	maxForwardPending  = 256
	forwardTimeout     = 15 * time.Second
)

// dkimAlignedFrom is the alignment check, a var so tests need no DNS.
var dkimAlignedFrom = func(raw []byte) string { return imapadapter.DKIMAlignedFrom(raw, nil) }

// itipForwarder holds what the poller needs to forward invitations.
type itipForwarder struct {
	client  *kycalendar.Client
	mu      sync.Mutex
	pending []pendingForward
}

type pendingForward struct {
	userID, eventID string
	body            map[string]any
	attempts        int
}

// SetKyCalendar pairs KyCalendar. Startup-only, before Start; nil or an
// unpaired Client leaves forwarding off.
func (p *Poller) SetKyCalendar(c *kycalendar.Client) {
	if c != nil {
		p.itip = &itipForwarder{client: c}
	}
}

// kyCalendarIdentity is the subject and issuer KyCalendar knows the user by,
// or "" when forwarding is off for them: the server is not paired, the user
// has not turned KyCalendar on, or the account has no live SSO link.
func (p *Poller) kyCalendarIdentity(u users.User, settings config.UserSettings) (string, string) {
	if p.itip == nil || !p.itip.client.Available() || !settings.KyCalendarOn(p.itip.client.DefaultOn()) || u.SSOSub == "" || u.SSOIssuer == "" || u.SSOLinkRevokedAt != 0 {
		return "", ""
	}
	return u.SSOSub, u.SSOIssuer
}

// forwardInvitation hands a new message's iTIP part to KyCalendar when a DKIM
// signature aligned to its From domain covers From. Best effort: it never
// fails the message, and logs IDs only.
func (p *Poller) forwardInvitation(ctx context.Context, uc userCtx, msg imapadapter.Message) {
	if uc.kycalSub == "" || !msg.HasAttachments || msg.TooLarge || msg.ParseError || msg.PGPEncrypted {
		return
	}
	uid, err := strconv.Atoi(strings.TrimSpace(msg.ID))
	if err != nil {
		return
	}
	raw, err := uc.mail.FetchRawMessage(ctx, "", uid)
	if err != nil || len(raw) == 0 {
		return
	}
	parsed, err := imapadapter.ParseRawContent(raw)
	if err != nil {
		return
	}
	var it mailmsg.ITIP
	var ics []byte
	for _, a := range parsed.Attachments {
		mediaType, _, _ := mime.ParseMediaType(a.MimeType)
		method := imapadapter.CalendarMethod(a.Content)
		if !strings.EqualFold(mediaType, "text/calendar") || (method != "REQUEST" && method != "REPLY" && method != "CANCEL") {
			continue
		}
		if it, ics, err = mailmsg.ParseITIP(string(a.Content), method); err == nil {
			break
		}
	}
	if ics == nil {
		return
	}
	from := dkimAlignedFrom(raw)
	if from == "" {
		return
	}
	// Provenance for a later REPLY: only an organizer who signed the mail
	// themselves can be answered, so a third party's invitation cannot aim
	// the user's replies at an arbitrary address.
	if it.Method == "REQUEST" && it.Organizer == from {
		if err := kycalendar.OpenInvitations(p.userStateDir(uc.id)).Record(it.UID, it.Organizer); err != nil {
			p.log.Error("kycalendar invitation not recorded", "user_id", uc.id, "message_id", msg.ID)
		}
	}
	sum := sha256.Sum256([]byte(uc.id + "\x00" + uc.mailbox + "\x00" + msg.ID))
	p.postForward(ctx, pendingForward{
		userID:  uc.id,
		eventID: "kypost-inbound-" + hex.EncodeToString(sum[:16]),
		body:    map[string]any{"sub": uc.kycalSub, "issuer": uc.kycalIssuer, "from": from, "dkim_aligned": true, "ics": string(ics)},
	})
}

// postForward sends one forward, queueing it for a later tick when
// KyCalendar is unreachable, rate limiting or failing.
func (p *Poller) postForward(ctx context.Context, f pendingForward) {
	ctx, cancel := context.WithTimeout(ctx, forwardTimeout)
	defer cancel()
	status, answer, err := p.itip.client.Post(ctx, "/api/itip/inbound", kycalendar.EventInbound, f.eventID, f.body)
	if err == nil && status < 500 && status != 429 {
		p.log.Info("kycalendar invitation forwarded", "user_id", f.userID, "event_id", f.eventID,
			"result", strconv.Itoa(status)+" "+kycalendar.Outcome(answer))
		return
	}
	f.attempts++
	p.log.Info("kycalendar invitation not taken", "user_id", f.userID, "event_id", f.eventID, "result", strconv.Itoa(status))
	if f.attempts >= maxForwardAttempts {
		return
	}
	p.itip.mu.Lock()
	defer p.itip.mu.Unlock()
	if len(p.itip.pending) < maxForwardPending {
		p.itip.pending = append(p.itip.pending, f)
	}
}

// retryForwards re-posts the user's queued forwards, once each per tick.
func (p *Poller) retryForwards(ctx context.Context, userID string) {
	p.itip.mu.Lock()
	var mine []pendingForward
	kept := p.itip.pending[:0]
	for _, f := range p.itip.pending {
		if f.userID == userID {
			mine = append(mine, f)
		} else {
			kept = append(kept, f)
		}
	}
	p.itip.pending = kept
	p.itip.mu.Unlock()
	for _, f := range mine {
		p.postForward(ctx, f)
	}
}
