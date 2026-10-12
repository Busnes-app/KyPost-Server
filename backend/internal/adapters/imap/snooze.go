package imap

import (
	"context"
	"errors"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

// SnoozedFolder holds snoozed mail until it wakes.
const SnoozedFolder = "Snoozed"

// ErrNoMessageID is returned for a message without exactly one valid Message-ID.
var ErrNoMessageID = errors.New("message has no single valid Message-ID")

// MessageIDOf returns the Message-ID of uid in folder.
func MessageIDOf(ctx context.Context, c Client, folder string, uid int) (string, error) {
	got, err := c.FetchHeaderFields(ctx, folder, []int{uid}, "Message-ID")
	if err != nil {
		return "", err
	}
	lines, ok := got[uid]
	if !ok {
		return "", ErrNoMessageID
	}
	id := singleMessageID(lines)
	if id == "" {
		return "", ErrNoMessageID
	}
	return id, nil
}

// MessageIDRefs maps Message-ID to UID for the newest n messages in folder.
// A Message-ID shared by several messages maps to the newest.
func MessageIDRefs(ctx context.Context, c Client, folder string, n int) (map[string]int, error) {
	overviews, err := c.ListOverviews(ctx, folder, n)
	if err != nil {
		return nil, err
	}
	uids := make([]int, 0, len(overviews))
	for _, ov := range overviews {
		uids = append(uids, ov.UID)
	}
	refs := map[string]int{}
	if len(uids) == 0 {
		return refs, nil
	}
	got, err := c.FetchHeaderFields(ctx, folder, uids, "Message-ID")
	if err != nil {
		return nil, err
	}
	for uid, lines := range got {
		if id := singleMessageID(lines); id != "" && uid > refs[id] {
			refs[id] = uid
		}
	}
	return refs, nil
}

// OverviewByID returns the overview of uid in folder, or false when it is gone.
func OverviewByID(ctx context.Context, c Client, folder string, uid int) (Overview, bool, error) {
	page, _, err := c.ListOverviewsBefore(ctx, folder, uid+1, 1)
	if err != nil || len(page) == 0 || page[0].UID != uid {
		return Overview{}, false, err
	}
	return page[0], true, nil
}

func singleMessageID(lines []string) string {
	values := HeaderMap(lines)["Message-Id"]
	if len(values) != 1 {
		return ""
	}
	ids := mailmsg.MessageIDs(values[0])
	if len(ids) != 1 {
		return ""
	}
	return ids[0]
}
