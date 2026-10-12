package processor

import (
	"context"
	"strconv"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// snoozeGiveUp drops a due snooze whose message cannot be found or moved for
// this long, so a deleted Snoozed folder does not log on every tick forever.
const snoozeGiveUp = 7 * 24 * time.Hour

// wokenLookback is how many of the newest INBOX messages are searched for a
// woken message's new IMAP UID.
const wokenLookback = 50

// wakeSnoozed returns due snoozed mail to INBOX as unread. IMAP renumbers the
// message, so this tick's fetchUnread treats it as new mail: rules, labels and
// push run as on arrival. Native keeps the id below the checkpoint, so the
// push is sent here.
func (p *Poller) wakeSnoozed(ctx context.Context, uc userCtx) {
	now := time.Now()
	due, err := uc.store.DueSnoozes(now.Unix())
	if err != nil || len(due) == 0 {
		if err != nil {
			p.log.Error("snooze wake failed", "user_id", uc.id, "error", err.Error())
		}
		return
	}
	refs, err := imapadapter.MessageIDRefs(ctx, uc.mail, imapadapter.SnoozedFolder, state.MaxSnoozes)
	if err != nil {
		p.log.Warn("snoozed mail lookup failed", "user_id", uc.id, "error", err.Error())
		dropOverdue(uc.store, due, now)
		return
	}
	native := imapadapter.MailSourceIdentity(uc.mail) != "imap"
	for _, z := range due {
		uid, ok := refs[z.MessageID]
		if !ok {
			// Moved out of Snoozed by the user, so it is already awake.
			_ = uc.store.DropSnooze(z.MessageID)
			continue
		}
		id := strconv.Itoa(uid)
		err := uc.mail.ApplyInboxAction(ctx, id, "unread", imapadapter.SnoozedFolder, "")
		if err == nil {
			err = uc.mail.ApplyInboxAction(ctx, id, "move", imapadapter.SnoozedFolder, "INBOX")
		}
		if err != nil {
			p.log.Warn("snooze wake move failed", "user_id", uc.id, "error", err.Error())
			dropOverdue(uc.store, []state.Snooze{z}, now)
			continue
		}
		inboxID := ""
		if native {
			inboxID = id
		} else if inbox, err := imapadapter.MessageIDRefs(ctx, uc.mail, "INBOX", wokenLookback); err == nil {
			if n, ok := inbox[z.MessageID]; ok {
				inboxID = strconv.Itoa(n)
			}
		}
		if err := uc.store.MarkWoken(z.MessageID, inboxID, now.Unix()); err != nil {
			p.log.Error("snooze wake record failed", "user_id", uc.id, "error", err.Error())
		}
		if native && inboxID != "" {
			p.notifyWoken(ctx, uc, uid)
		}
	}
}

func (p *Poller) notifyWoken(ctx context.Context, uc userCtx, uid int) {
	ov, ok, err := imapadapter.OverviewByID(ctx, uc.mail, "INBOX", uid)
	if err != nil || !ok {
		return
	}
	msg := imapadapter.Message{ID: ov.MessageID, Subject: ov.Subject, Sender: ov.Sender, SentTo: ov.SentTo, AtUTC: ov.AtUTC, Keywords: ov.Keywords}
	p.maybeSendPushNotification(uc, msg, "", ov.Keywords)
	p.maybeSendNativePushNotification(uc, msg, "", ov.Keywords)
}

func dropOverdue(store *state.Store, due []state.Snooze, now time.Time) {
	for _, z := range due {
		if now.Sub(time.Unix(z.WakeAt, 0)) > snoozeGiveUp {
			_ = store.DropSnooze(z.MessageID)
		}
	}
}
