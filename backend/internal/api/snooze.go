package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// maxSnoozeAhead bounds how far ahead a message can be snoozed.
const maxSnoozeAhead = 366 * 24 * time.Hour

// parseWakeAt reads a snooze wake time: RFC3339, in the future, within a year.
func parseWakeAt(raw string, now time.Time) (int64, error) {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return 0, errors.New("wakeAt must be an RFC3339 time")
	}
	if !t.After(now) || t.Sub(now) > maxSnoozeAhead {
		return 0, errors.New("wakeAt must be in the future and within a year")
	}
	return t.Unix(), nil
}

// mailStateFor is the selected mailbox's state store.
func (s *Server) mailStateFor(r *http.Request) (*state.Store, error) {
	ac, ok := authFromContext(r)
	if !ok {
		return nil, errors.New("no auth context on request")
	}
	return s.mailboxStore(r.Context(), ac.UserID, ac.Mailbox)
}

// snoozeMessage records the snooze before moving, so a crash between the two
// leaves a row that the wake drops, never a message stranded in Snoozed.
func snoozeMessage(ctx context.Context, c imapadapter.Client, store *state.Store, internalID, mailbox string, wakeAt int64) error {
	uid, err := strconv.Atoi(internalID)
	if err != nil {
		return errors.New("invalid message id")
	}
	messageID, err := imapadapter.MessageIDOf(ctx, c, mailbox, uid)
	if err != nil {
		return err
	}
	if err := store.Snooze(messageID, wakeAt); err != nil {
		return err
	}
	if mailbox == imapadapter.SnoozedFolder {
		return nil
	}
	if err := c.ApplyInboxAction(ctx, internalID, "move", mailbox, imapadapter.SnoozedFolder); err != nil {
		_ = store.DropSnooze(messageID)
		return err
	}
	return nil
}

// handleSnoozes lists the selected mailbox's snoozed messages by their
// reference in the Snoozed folder, for the "until …" column.
func (s *Server) handleSnoozes(w http.ResponseWriter, r *http.Request) {
	mailClient, err := s.mailFor(r)
	if err != nil {
		if writeMailSourceConflict(w, err) {
			return
		}
		http.Error(w, "imap client is not configured", http.StatusServiceUnavailable)
		return
	}
	store, err := s.mailStateFor(r)
	if err != nil {
		http.Error(w, "failed to open user state", http.StatusInternalServerError)
		return
	}
	rows, err := store.ListSnoozes()
	if err != nil {
		http.Error(w, "failed to read snoozes", http.StatusInternalServerError)
		return
	}
	type snoozed struct {
		MessageID string `json:"messageId"`
		WakeAt    string `json:"wakeAt"`
	}
	out := []snoozed{}
	if len(rows) > 0 {
		refs, err := imapadapter.MessageIDRefs(r.Context(), mailClient, imapadapter.SnoozedFolder, state.MaxSnoozes)
		if err != nil {
			http.Error(w, "failed to fetch snoozed mail", http.StatusBadGateway)
			return
		}
		for _, z := range rows {
			if uid, ok := refs[z.MessageID]; ok {
				out = append(out, snoozed{
					MessageID: imapadapter.MessageReference(mailClient, strconv.Itoa(uid)),
					WakeAt:    time.Unix(z.WakeAt, 0).UTC().Format(time.RFC3339),
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"snoozes": out})
}

// wokenPins maps INBOX ids of the selected mailbox's woken messages to their
// rows. A pin is decoration: failing to read it never fails the inbox.
func (s *Server) wokenPins(ctx context.Context, userID, mailbox string) (map[int]state.Snooze, *state.Store) {
	if mailbox != "" && !strings.EqualFold(mailbox, "INBOX") {
		return nil, nil
	}
	ac, _ := authContextFromContext(ctx)
	store, err := s.mailboxStore(ctx, userID, ac.Mailbox)
	if err != nil {
		return nil, nil
	}
	rows, err := store.Woken()
	if err != nil {
		s.logger.Error("failed to read woken mail", "user_id", userID, "error", err.Error())
		return nil, nil
	}
	pins := make(map[int]state.Snooze, len(rows))
	for _, z := range rows {
		if id, err := strconv.Atoi(z.InboxID); err == nil {
			pins[id] = z
		}
	}
	return pins, store
}

func wokenAt(z state.Snooze) string { return time.Unix(z.WokenAt, 0).UTC().Format(time.RFC3339) }

func pinnedAt(pins map[int]state.Snooze, uid int) string {
	if z, ok := pins[uid]; ok {
		return wokenAt(z)
	}
	return ""
}
