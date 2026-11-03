package processor

import (
	"context"
	"strconv"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// wokenLookback is how many of the newest INBOX messages are searched for a
// woken message's new IMAP UID.
const wokenLookback = 50

// pinSearchFor is how long an unpinned woken message is searched for.
const pinSearchFor = 24 * time.Hour

// wakeSnoozed returns due snoozed mail to INBOX as unread. A row is dropped
// only once its message has woken or is confirmed gone from Snoozed; any
// failure keeps it for the next tick. IMAP renumbers the message, so this
// tick's fetchUnread treats it as new mail: rules, labels and push run as on
// arrival. Native keeps the id, so wake sends the push unless arrival
// processing still will.
func (p *Poller) wakeSnoozed(ctx context.Context, uc userCtx) {
	now := time.Now()
	p.pinWoken(ctx, uc)
	due, err := uc.store.DueSnoozes(now.Unix())
	if err != nil || len(due) == 0 {
		if err != nil {
			p.log.Error("snooze wake failed", "user_id", uc.id, "error", err.Error())
		}
		return
	}
	refs, complete, err := imapadapter.MessageIDRefs(ctx, uc.mail, imapadapter.SnoozedFolder, state.MaxSnoozes)
	if err != nil {
		p.log.Warn("snoozed mail lookup failed", "user_id", uc.id, "error", err.Error())
		return
	}
	native := imapadapter.MailSourceIdentity(uc.mail) != "imap"
	woke := false
	for _, z := range due {
		uid, ok := refs[z.MessageID]
		if !ok {
			if complete {
				// Moved out of Snoozed by the user, so it is already awake.
				_ = uc.store.DropSnooze(z.MessageID)
			}
			continue
		}
		id := strconv.Itoa(uid)
		err := uc.mail.ApplyInboxAction(ctx, id, "unread", imapadapter.SnoozedFolder, "")
		if err == nil {
			err = uc.mail.ApplyInboxAction(ctx, id, "move", imapadapter.SnoozedFolder, "INBOX")
		}
		if err != nil {
			p.log.Warn("snooze wake move failed", "user_id", uc.id, "error", err.Error())
			continue
		}
		inboxID := ""
		if native {
			inboxID = id
		}
		if err := uc.store.MarkWoken(z.MessageID, inboxID, now.Unix()); err != nil {
			p.log.Error("snooze wake record failed", "user_id", uc.id, "error", err.Error())
		}
		woke = true
		if native && !p.arrivalPending(uc, uid) {
			p.notifyWoken(ctx, uc, uid)
		}
	}
	if woke && !native {
		p.pinWoken(ctx, uc)
	}
}

// pinWoken finds the INBOX UID of woken IMAP mail not yet pinned: just woken,
// a lookup that failed on an earlier tick, or pins cleared by a UID reset.
func (p *Poller) pinWoken(ctx context.Context, uc userCtx) {
	rows, err := uc.store.Unpinned()
	if err != nil || len(rows) == 0 {
		return
	}
	inbox, complete, err := imapadapter.MessageIDRefs(ctx, uc.mail, "INBOX", wokenLookback)
	if err != nil {
		return
	}
	for _, z := range rows {
		if uid, ok := inbox[z.MessageID]; ok {
			_ = uc.store.MarkWoken(z.MessageID, strconv.Itoa(uid), z.WokenAt)
		} else if complete && time.Since(time.Unix(z.WokenAt, 0)) > pinSearchFor {
			// Left INBOX or buried under newer mail: a pin no longer helps.
			_ = uc.store.DropSnooze(z.MessageID)
		}
	}
}

// arrivalPending reports that a native message has not been processed yet
// and sits above the checkpoint, so the poll that follows will notify for it.
func (p *Poller) arrivalPending(uc userCtx, uid int) bool {
	if seen, err := uc.store.Seen(strconv.Itoa(uid)); err != nil || seen {
		return false
	}
	checkpoint, err := uc.store.Checkpoint()
	if err != nil {
		return false
	}
	cp, err := strconv.Atoi(checkpoint)
	return err == nil && uid > cp
}

func (p *Poller) notifyWoken(ctx context.Context, uc userCtx, uid int) {
	ov, ok, err := imapadapter.OverviewByID(ctx, uc.mail, "INBOX", uid)
	if err != nil || !ok {
		return
	}
	msg := imapadapter.Message{ID: ov.MessageID, Subject: ov.Subject, Sender: ov.Sender, SentTo: ov.SentTo, AtUTC: ov.AtUTC, Keywords: ov.Keywords}
	// Encrypted mail keeps its metadata out of cleartext push. Fail closed:
	// content we could not inspect is treated as encrypted.
	contents, err := uc.mail.GetMessageBodies(ctx, "INBOX", []int{uid})
	c, ok := contents[uid]
	msg.PGPEncrypted = err != nil || !ok || c.TooLarge || c.ParseError || c.PGPEncrypted || c.PGPEncryptedPayload != ""
	p.maybeSendPushNotification(uc, msg, "", ov.Keywords)
	p.maybeSendNativePushNotification(uc, msg, "", ov.Keywords)
}
