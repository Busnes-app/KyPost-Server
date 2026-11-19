// Conversation grouping for GET /api/inbox/conversations: the listed window of
// a folder, joined with your replies from Sent, grouped by Message-ID,
// In-Reply-To and References. /api/inbox itself is unchanged; clients opt in.
package api

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
)

// subjectFallbackWindow bounds the subject match used when one side of a reply
// chain carries no threading headers; past it, equal subjects stay apart.
const subjectFallbackWindow = 7 * 24 * time.Hour

var threadHeaderFields = []string{"Message-ID", "In-Reply-To", "References"}

// threadMember is one message considered for grouping. known is false when
// its headers were not fetched (oversized or over the batch budget): unknown
// is not absent, so such a message never takes part in the subject fallback.
type threadMember struct {
	folder  string
	ov      imapadapter.Overview
	own     string   // its Message-ID token
	parents []string // In-Reply-To and References tokens
	known   bool
	// Derived once: grouping compares these, never the raw sender-chosen subject.
	at    time.Time
	dated bool
	base  string // lower-cased baseSubject
	reply bool   // the subject carries a reply prefix
}

func newThreadMember(folder string, ov imapadapter.Overview, lines []string, known bool) threadMember {
	h := imapadapter.HeaderMap(lines)
	m := threadMember{folder: folder, ov: ov, known: known}
	if ids := messageIDToken.FindAllString(strings.Join(h["Message-Id"], " "), 1); len(ids) == 1 {
		m.own = ids[0]
	}
	m.parents = messageIDToken.FindAllString(strings.Join(append(slices.Clone(h["In-Reply-To"]), h["References"]...), " "), -1)
	t, err := time.Parse(time.RFC3339, ov.AtUTC)
	m.at, m.dated = t, err == nil
	subject := capSubject(ov.Subject)
	m.base = strings.ToLower(baseSubject(subject))
	m.reply = replyPrefix.MatchString(subject)
	return m
}

// maxGroupedSubjectBytes bounds the sender-chosen subject grouping normalises,
// so a crafted subject costs a fixed amount however long it is.
const maxGroupedSubjectBytes = 1024

// capSubject cuts s to maxGroupedSubjectBytes on a rune boundary.
func capSubject(s string) string {
	if len(s) <= maxGroupedSubjectBytes {
		return s
	}
	cut := maxGroupedSubjectBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// groupThreads partitions members into conversations, each oldest first.
// Members sharing any Message-ID token join (threadIDs' rule, so siblings whose
// parent is outside the window still meet). Then a reply (Re:/Fwd:/AW:/SV:)
// joins the newest earlier message with the same base subject within
// subjectFallbackWindow, but only when none of the reply's parents is here and
// either the reply names no parent or that message has no Message-ID: equal
// subjects alone ("Invoice") never merge. Subjects are normalised once per
// member and ctx is checked per member, since the input is sender-controlled.
func groupThreads(ctx context.Context, members []threadMember) ([][]threadMember, error) {
	parent := make([]int, len(members))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	union := func(a, b int) { parent[find(a)] = find(b) }

	firstWith, owned := map[string]int{}, map[string]bool{}
	for _, m := range members {
		if m.own != "" {
			owned[m.own] = true
		}
	}
	for i, m := range members {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, id := range append([]string{m.own}, m.parents...) {
			if id == "" {
				continue
			}
			if j, ok := firstWith[id]; ok {
				union(i, j)
			} else {
				firstWith[id] = i
			}
		}
	}

	// Candidates per base subject, oldest first, so a reply walks back from its
	// own time and stops at the window instead of comparing every pair.
	bySubject := map[string][]int{}
	for i, m := range members {
		if m.dated && m.known && m.base != "" {
			bySubject[m.base] = append(bySubject[m.base], i)
		}
	}
	for _, ids := range bySubject {
		slices.SortStableFunc(ids, func(a, b int) int { return members[a].at.Compare(members[b].at) })
	}
	for i, reply := range members {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !reply.reply || !reply.dated || !reply.known || reply.base == "" ||
			slices.ContainsFunc(reply.parents, func(id string) bool { return owned[id] }) {
			continue
		}
		ids := bySubject[reply.base]
		end, _ := slices.BinarySearchFunc(ids, reply.at, func(j int, t time.Time) int {
			if members[j].at.After(t) {
				return 1
			}
			return -1
		})
		for k := end - 1; k >= 0 && reply.at.Sub(members[ids[k]].at) <= subjectFallbackWindow; k-- {
			if j := ids[k]; j != i && (len(reply.parents) == 0 || members[j].own == "") {
				union(i, j)
				break
			}
		}
	}

	byRoot := map[int][]threadMember{}
	var roots []int
	for i, m := range members {
		r := find(i)
		if _, ok := byRoot[r]; !ok {
			roots = append(roots, r)
		}
		byRoot[r] = append(byRoot[r], m)
	}
	out := make([][]threadMember, 0, len(roots))
	for _, r := range roots {
		group := byRoot[r]
		slices.SortStableFunc(group, func(a, b threadMember) int {
			return cmp.Or(a.at.Compare(b.at), cmp.Compare(a.ov.UID, b.ov.UID))
		})
		out = append(out, group)
	}
	return out, nil
}

// listThreadMembers lists the newest limit messages of folder with their
// threading headers: header lines only, never bodies.
func listThreadMembers(ctx context.Context, client imapadapter.Client, folder string, limit int) ([]threadMember, error) {
	overviews, err := client.ListOverviews(ctx, folder, limit)
	if err != nil {
		return nil, err
	}
	uids := make([]int, 0, len(overviews))
	for _, ov := range overviews {
		uids = append(uids, ov.UID)
	}
	lines, err := client.FetchHeaderFields(ctx, folder, uids, threadHeaderFields...)
	if err != nil {
		return nil, err
	}
	out := make([]threadMember, 0, len(overviews))
	for _, ov := range overviews {
		got, known := lines[ov.UID]
		out = append(out, newThreadMember(folder, ov, got, known))
	}
	return out, nil
}

type conversationOut struct {
	// ID is the oldest listed member's messageId.
	ID string `json:"id"`
	// MessageIDs are the members in the requested folder, oldest first.
	MessageIDs []string `json:"messageIds"`
	// Sent are your own replies, listed where they are and never moved.
	Sent []sentMember `json:"sent,omitempty"`
}

type sentMember struct {
	inboxEmail
	Mailbox string `json:"mailbox"`
}

// handleInboxConversations answers GET /api/inbox/conversations?mailbox=&limit=:
// the conversations among the newest `limit` messages of mailbox (default 500,
// the inbox window) and the newest `limit` of each Sent folder. Only groups of
// two or more with a member in mailbox are listed; every other message is its
// own conversation. ponytail: grouped per window and recomputed per request,
// so a member older than the window is not joined; a native mailbox reads each
// stored message to find its headers.
func (s *Server) handleInboxConversations(w http.ResponseWriter, r *http.Request) {
	limit := maxInboxLimit
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= maxInboxLimit {
		limit = v
	}
	mailbox := strings.TrimSpace(r.URL.Query().Get("mailbox"))
	client, err := s.mailFor(r)
	if err != nil {
		if writeMailSourceConflict(w, err) {
			return
		}
		if errors.Is(err, errIMAPNotConfigured) {
			writeJSON(w, http.StatusOK, map[string]any{"conversations": []conversationOut{}})
			return
		}
		http.Error(w, "failed to open mail source; check account state and configuration before retrying", http.StatusServiceUnavailable)
		return
	}
	ctx := r.Context()
	members, err := listThreadMembers(ctx, client, mailbox, limit)
	if errors.Is(err, imapadapter.ErrUnsafeMailbox) {
		writeMailboxError(w, err)
		return
	}
	if err != nil {
		http.Error(w, "failed to fetch inbox", http.StatusBadGateway)
		return
	}
	folder := mailbox
	if folder == "" {
		folder = "INBOX"
	}
	for _, sent := range sentFoldersBesides(ctx, client, folder) {
		// A Sent folder that does not exist or will not list adds no replies.
		if more, err := listThreadMembers(ctx, client, sent, limit); err == nil {
			members = append(members, more...)
		}
	}
	groups, err := groupThreads(ctx, members)
	if err != nil {
		http.Error(w, "request cancelled", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": conversationsOut(client, mailbox, groups)})
}

func conversationsOut(client imapadapter.Client, mailbox string, groups [][]threadMember) []conversationOut {
	out := []conversationOut{}
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		c := conversationOut{MessageIDs: []string{}}
		for _, m := range group {
			ref := imapadapter.MessageReference(client, m.ov.MessageID)
			if m.folder == mailbox {
				c.MessageIDs = append(c.MessageIDs, ref)
				continue
			}
			c.Sent = append(c.Sent, sentMember{Mailbox: m.folder, inboxEmail: inboxEmail{
				MessageID: ref, Sender: m.ov.Sender, SentTo: m.ov.SentTo, CC: m.ov.CC,
				Subject: m.ov.Subject, Status: m.ov.Status, AtUTC: m.ov.AtUTC, Keywords: m.ov.Keywords,
			}})
		}
		if len(c.MessageIDs) == 0 {
			continue
		}
		c.ID = c.MessageIDs[0]
		out = append(out, c)
	}
	return out
}
