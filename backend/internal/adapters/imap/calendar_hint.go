package imap

import (
	"sort"
	"strconv"
	"strings"

	goimap "github.com/BrianLeishman/go-imap"
)

// calendarInviteUIDs reports which fetched messages carry a text/calendar
// part that go-imap's parser drops (see calendarParts), so the list can set
// HasAttachments for an invite-only message. One BODYSTRUCTURE fetch, only
// for messages with no attachment already: a server-built structure listing,
// no body bytes. Best effort: on any error nothing is marked, which is the
// pre-existing answer.
func calendarInviteUIDs(d *goimap.Dialer, emails map[int]*goimap.Email) map[int]bool {
	var uids []string
	for uid, e := range emails {
		if e != nil && len(e.Attachments) == 0 {
			uids = append(uids, strconv.Itoa(uid))
		}
	}
	out := map[int]bool{}
	if len(uids) == 0 {
		return out
	}
	sort.Strings(uids)
	raw, err := d.Exec("UID FETCH "+strings.Join(uids, ",")+" BODYSTRUCTURE", true, goimap.RetryCount, nil)
	if err != nil {
		return out
	}
	records, err := d.ParseFetchResponse(raw)
	if err != nil {
		return out
	}
	for _, tks := range records {
		tks = unwrapTokens(tks)
		uid, structure := 0, (*goimap.Token)(nil)
		for i := 0; i+1 < len(tks); i++ {
			switch {
			case strings.EqualFold(tks[i].Str, "UID") && tks[i+1].Type == goimap.TNumber:
				uid = tks[i+1].Num
			case strings.EqualFold(tks[i].Str, "BODYSTRUCTURE") && tks[i+1].Type == goimap.TContainer:
				structure = tks[i+1]
			}
		}
		if _, asked := emails[uid]; asked && structure != nil && hasCalendarPart(structure, 0) {
			out[uid] = true
		}
	}
	return out
}

// hasCalendarPart walks a BODYSTRUCTURE (RFC 3501 §7.4.2). A multipart lists
// its child parts as leading containers; a single part starts with its type
// and subtype strings. Only child-part positions are descended into, so a
// parameter list such as ("text" "calendar") can never match.
func hasCalendarPart(t *goimap.Token, depth int) bool {
	if depth > 16 || len(t.Tokens) == 0 {
		return false
	}
	if t.Tokens[0].Type == goimap.TContainer {
		for _, child := range t.Tokens {
			if child.Type != goimap.TContainer {
				break
			}
			if hasCalendarPart(child, depth+1) {
				return true
			}
		}
		return false
	}
	return len(t.Tokens) > 1 && strings.EqualFold(t.Tokens[0].Str, "text") && strings.EqualFold(t.Tokens[1].Str, "calendar")
}
