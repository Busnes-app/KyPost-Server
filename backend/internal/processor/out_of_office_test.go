package processor

import (
	"bytes"
	"context"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/rules"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

const oooBase = "From: Alice <alice@example.com>\r\nTo: bob@x.test\r\nSubject: Lunch?\r\nMessage-ID: <m1@example.com>\r\nReferences: <r0@example.com>\r\n"

func oooHeader(t *testing.T, head string) mail.Header {
	t.Helper()
	m, err := mail.ReadMessage(strings.NewReader(head + "\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	return m.Header
}

func TestOOOEligible(t *testing.T) {
	own := map[string]bool{"bob@x.test": true, "bob.alias@x.test": true}
	refused := map[string]string{
		"auto-submitted":       oooBase + "Auto-Submitted: auto-replied\r\n",
		"auto-generated":       oooBase + "Auto-Submitted: auto-generated\r\n",
		"precedence bulk":      oooBase + "Precedence: bulk\r\n",
		"precedence list":      oooBase + "Precedence: list\r\n",
		"precedence junk":      oooBase + "Precedence: junk\r\n",
		"precedence autoreply": oooBase + "Precedence: auto_reply\r\n",
		"list-id":              oooBase + "List-Id: <l.example.com>\r\n",
		"list-unsubscribe":     oooBase + "List-Unsubscribe: <mailto:u@example.com>\r\n",
		"list-post":            oooBase + "List-Post: <mailto:l@example.com>\r\n",
		"suppress":             oooBase + "X-Auto-Response-Suppress: OOF\r\n",
		"x-autoreply":          oooBase + "X-Autoreply: yes\r\n",
		"null return-path":     "Return-Path: <>\r\n" + oooBase,
		"spam flag":            oooBase + "X-Spam-Flag: YES\r\n",
		"spam status":          oooBase + "X-Spam-Status: Yes, score=9\r\n",
		"mailer-daemon":        strings.Replace(oooBase, "alice@example.com", "MAILER-DAEMON@example.com", 1),
		"noreply":              strings.Replace(oooBase, "alice@example.com", "noreply@example.com", 1),
		"no-reply":             strings.Replace(oooBase, "alice@example.com", "billing-no-reply@example.com", 1),
		"own sender":           strings.Replace(oooBase, "alice@example.com", "Bob.Alias@x.test", 1),
		"bcc only":             strings.Replace(oooBase, "To: bob@x.test", "To: carol@x.test", 1),
		"two from":             strings.Replace(oooBase, "From: Alice <alice@example.com>", "From: a@example.com, b@example.com", 1),
		"no message-id":        strings.Replace(oooBase, "Message-ID: <m1@example.com>\r\n", "", 1),
	}
	for name, head := range refused {
		if _, reason := oooEligible(oooHeader(t, head), own); reason == "" {
			t.Errorf("%s: eligible, want refused", name)
		}
	}
	for name, head := range map[string]string{
		"to":                oooBase,
		"cc":                strings.Replace(oooBase, "To: bob@x.test", "To: carol@x.test\r\nCc: Bob <BOB@x.test>", 1),
		"auto-submitted no": oooBase + "Auto-Submitted: no\r\n",
		"spam flag no":      oooBase + "X-Spam-Flag: NO\r\n",
	} {
		orig, reason := oooEligible(oooHeader(t, head), own)
		if reason != "" || orig.sender != "alice@example.com" || orig.messageID != "<m1@example.com>" {
			t.Errorf("%s: %+v %q, want eligible", name, orig, reason)
		}
	}
}

type oooSent struct {
	from string
	to   []string
	raw  []byte
}

type oooMailClient struct {
	*noopMailClient
	raw   map[int][]byte
	saved *[][]byte
}

func (c oooMailClient) FetchRawMessage(_ context.Context, _ string, uid int) ([]byte, error) {
	return c.raw[uid], nil
}

func (c oooMailClient) SaveSent(_ context.Context, d imapadapter.DraftMessage) error {
	*c.saved = append(*c.saved, d.Raw)
	return nil
}

type oooHarness struct {
	p      *Poller
	userID string
	sent   []oooSent
	saved  [][]byte
	queued []mailbox.OutboundJob
	member bool
	store  *state.Store
}

func newOOOHarness(t *testing.T) *oooHarness {
	t.Helper()
	p, userID := newTestPollerWithUsers(t)
	p.configDir = t.TempDir()
	p.imapKeyPath = filepath.Join(t.TempDir(), "imap-config.key")
	writeTestIMAPConfig(t, p.configDir, p.imapKeyPath, userID, "bob@x.test", "hunter2")
	global, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = global.Close() })
	p.globalStore = global
	p.cfg = config.Default()
	h := &oooHarness{p: p, userID: userID}
	prevSMTP, prevNative, prevMember := oooSendSMTP, oooQueueNative, oooSuiteMember
	oooSendSMTP = func(_ string, _ int, _, _, _, from string, to []string, raw []byte) error {
		h.sent = append(h.sent, oooSent{from, to, raw})
		return nil
	}
	oooQueueNative = func(_ context.Context, _ *Poller, _, _ string, job mailbox.OutboundJob) error {
		h.queued = append(h.queued, job)
		return nil
	}
	oooSuiteMember = func(context.Context, *Poller, users.User, string) bool { return h.member }
	t.Cleanup(func() { oooSendSMTP, oooQueueNative, oooSuiteMember = prevSMTP, prevNative, prevMember })
	stubAlignment(t, map[string]string{"alice@example.com": "alice@example.com"})
	return h
}

func (h *oooHarness) ctx(t *testing.T, ooo config.OutOfOffice, raw ...[]byte) userCtx {
	t.Helper()
	if h.store == nil {
		var err error
		if h.store, err = state.New(t.TempDir()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.store.Close() })
	}
	store := h.store
	m := map[int][]byte{}
	for i, r := range raw {
		m[i+1] = r
	}
	return userCtx{id: h.userID, store: store, mail: oooMailClient{&noopMailClient{}, m, &h.saved}, ooo: ooo, knownSenders: map[string]bool{}}
}

func oooOn() config.OutOfOffice {
	return config.OutOfOffice{Enabled: true, LocalMessage: "Away, back Monday.", Remote: config.RemoteOff, RemoteMessage: "I am away.", Period: "p1"}
}

func TestOutOfOfficeReplyLocal(t *testing.T) {
	h := newOOOHarness(t)
	h.member = true
	uc := h.ctx(t, oooOn(), []byte("Date: "+time.Now().Format(time.RFC1123Z)+"\r\n"+oooBase+"\r\nprivate body\r\n"), []byte(strings.Replace(oooBase, "<m1@", "<m2@", 1)+"\r\nagain\r\n"),
		[]byte(strings.Replace(oooBase, "<m1@", "<m3@", 1)+"\r\nlater\r\n"))
	h.p.outOfOfficeReply(t.Context(), uc, imapadapter.Message{ID: "1", Subject: "Lunch?\r\nBcc: x@evil.test"}, "bob@x.test")
	if len(h.sent) != 1 || len(h.saved) != 1 {
		t.Fatalf("sent %d, saved %d; want 1 and 1", len(h.sent), len(h.saved))
	}
	s := h.sent[0]
	if s.from != "bob@x.test" || len(s.to) != 1 || s.to[0] != "alice@example.com" {
		t.Fatalf("envelope %s -> %v", s.from, s.to)
	}
	m, err := mail.ReadMessage(bytes.NewReader(s.raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.Header.Get("Auto-Submitted") != "auto-replied" || m.Header.Get("In-Reply-To") != "<m1@example.com>" ||
		!strings.Contains(m.Header.Get("References"), "<r0@example.com>") || !strings.Contains(m.Header.Get("References"), "<m1@example.com>") {
		t.Fatalf("headers %v", m.Header)
	}
	if got := m.Header.Get("Subject"); got != "Automatic reply: Lunch?  Bcc: x@evil.test" || m.Header.Get("Bcc") != "" {
		t.Fatalf("subject %q", got)
	}
	if strings.Contains(string(s.raw), "private body") {
		t.Fatal("original body quoted")
	}
	// Replayed message, and a second message from the same sender: no reply.
	h.p.outOfOfficeReply(t.Context(), uc, imapadapter.Message{ID: "1"}, "bob@x.test")
	h.p.outOfOfficeReply(t.Context(), uc, imapadapter.Message{ID: "2"}, "bob@x.test")
	if len(h.sent) != 1 {
		t.Fatalf("sent %d replies, want 1", len(h.sent))
	}
	// A new period answers the same sender again.
	uc.ooo.Period = "p2"
	h.p.outOfOfficeReply(t.Context(), uc, imapadapter.Message{ID: "3"}, "bob@x.test")
	if len(h.sent) != 2 {
		t.Fatalf("new period: sent %d, want 2", len(h.sent))
	}
}

func TestOutOfOfficeReplyRemote(t *testing.T) {
	msg := []byte(oooBase + "\r\nhi\r\n")
	for name, tc := range map[string]struct {
		allowed bool
		remote  string
		contact bool
		want    string
	}{
		"admin off":            {allowed: false, remote: config.RemoteOn},
		"user off":             {allowed: true, remote: config.RemoteOff},
		"on":                   {allowed: true, remote: config.RemoteOn, want: "I am away."},
		"contacts, stranger":   {allowed: true, remote: config.RemoteContacts},
		"contacts, in contact": {allowed: true, remote: config.RemoteContacts, contact: true, want: "I am away."},
	} {
		t.Run(name, func(t *testing.T) {
			h := newOOOHarness(t)
			h.p.cfg.OutOfOfficeRemote = tc.allowed
			ooo := oooOn()
			ooo.Remote = tc.remote
			uc := h.ctx(t, ooo, msg)
			uc.knownSenders["alice@example.com"] = tc.contact
			h.p.outOfOfficeReply(t.Context(), uc, imapadapter.Message{ID: "1"}, "bob@x.test")
			if tc.want == "" {
				if len(h.sent) != 0 {
					t.Fatal("remote reply sent")
				}
				return
			}
			if len(h.sent) != 1 {
				t.Fatalf("sent %d, want 1", len(h.sent))
			}
			m, _ := mail.ReadMessage(bytes.NewReader(h.sent[0].raw))
			body := new(bytes.Buffer)
			_, _ = body.ReadFrom(m.Body)
			if !strings.Contains(body.String(), "SSBhbSBhd2F5Lg==") { // base64 "I am away."
				t.Fatalf("body %q", body.String())
			}
		})
	}
}

func TestOutOfOfficeReplyNotSent(t *testing.T) {
	msg := []byte(oooBase + "\r\nhi\r\n")
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	for name, tc := range map[string]func(*oooHarness, *userCtx){
		"off":         func(_ *oooHarness, uc *userCtx) { uc.ooo.Enabled = false },
		"not started": func(_ *oooHarness, uc *userCtx) { uc.ooo.Start = future },
		"ended":       func(_ *oooHarness, uc *userCtx) { uc.ooo.End = past },
		"extra mailbox": func(_ *oooHarness, uc *userCtx) {
			uc.mailbox = "extra-1"
		},
		"unauthenticated": func(_ *oooHarness, uc *userCtx) {
			uc.mail = oooMailClient{&noopMailClient{}, map[int][]byte{1: []byte(strings.ReplaceAll(oooBase, "alice@example.com", "mallory@example.com") + "\r\nhi\r\n")}, new([][]byte)}
		},
		"old mail": func(_ *oooHarness, uc *userCtx) {
			uc.mail = oooMailClient{&noopMailClient{}, map[int][]byte{1: []byte("Date: Mon, 2 Jan 2006 15:04:05 -0700\r\n" + oooBase + "\r\nhi\r\n")}, new([][]byte)}
		},
		"rule marks spam": func(_ *oooHarness, uc *userCtx) {
			uc.rules = []rules.Rule{{ID: "r", Enabled: true, Match: rules.MatchGroup{Op: "allof", Conditions: []rules.Condition{{Field: "from", Comparator: "contains", Value: "alice"}}}, Actions: []rules.Action{{Type: "spam"}}}}
		},
		"quota spent": func(h *oooHarness, _ *userCtx) {
			q := h.p.cfg.SendQuota
			if _, err := h.p.globalStore.AdmitSend(context.Background(), "x", h.userID, q.RecipientsPerDay, q.MessagesPerHour, q.RecipientsPerDay, time.Now().Unix()); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newOOOHarness(t)
			h.member = true
			uc := h.ctx(t, oooOn(), msg)
			tc(h, &uc)
			h.p.outOfOfficeReply(t.Context(), uc, imapadapter.Message{ID: "1", Sender: "alice@example.com"}, "bob@x.test")
			if len(h.sent)+len(h.queued) != 0 {
				t.Fatal("reply sent")
			}
		})
	}
}

// A native primary mailbox queues the reply in the outbox, which files Sent.
func TestOutOfOfficeReplyNativeQueues(t *testing.T) {
	h := newOOOHarness(t)
	h.member = true
	uc := h.ctx(t, oooOn(), []byte(oooBase+"\r\nhi\r\n"))
	uc.mailbox = h.userID
	h.p.outOfOfficeReply(t.Context(), uc, imapadapter.Message{ID: "1"}, "bob@x.test")
	if len(h.queued) != 1 || len(h.sent) != 0 {
		t.Fatalf("queued %d, smtp %d", len(h.queued), len(h.sent))
	}
	j := h.queued[0]
	if j.From != "bob@x.test" || len(j.Deliveries) != 1 || j.Deliveries[0].Recipients[0] != "alice@example.com" || len(j.Sent) == 0 {
		t.Fatalf("job %+v", j)
	}
}

// Only an established native domain makes a sender local; a retired or
// unproven domain falls back to the suite directory.
func TestOutOfOfficeLocalNeedsEstablishedDomain(t *testing.T) {
	token := strings.Repeat("ab", 32)
	proof := func(established string) string {
		return `{"example.com":{"token":"` + token + `","expiresAt":1,"established":` + established + `,"verifiedUntil":0}}`
	}
	for name, tc := range map[string]struct {
		domains, retired string
		want             int
	}{
		"established": {proof("true"), `[]`, 1},
		"unproven":    {proof("false"), `[]`, 0},
		"retired":     {`{}`, `["example.com"]`, 0},
	} {
		t.Run(name, func(t *testing.T) {
			h := newOOOHarness(t)
			set := `{"version":1,"issuer":"https://id.x.test","domains":` + tc.domains + `,"retired":` + tc.retired + `}`
			if err := os.WriteFile(filepath.Join(h.p.configDir, sso.NativeDomainsFile), []byte(set), 0o600); err != nil {
				t.Fatal(err)
			}
			uc := h.ctx(t, oooOn(), []byte(oooBase+"\r\nhi\r\n"))
			uc.mailbox = h.userID
			h.p.outOfOfficeReply(t.Context(), uc, imapadapter.Message{ID: "1"}, "bob@x.test")
			if len(h.queued)+len(h.sent) != tc.want {
				t.Fatalf("replies %d, want %d", len(h.queued)+len(h.sent), tc.want)
			}
		})
	}
}

// Mail to a masked address never gets an automatic reply (it would come from
// the account address); a plus tag on an own address still does.
func TestOOOMaskedAndPlusAddresses(t *testing.T) {
	own := map[string]bool{"bob@x.test": true}
	settings := config.UserSettings{MaskedAddresses: []config.MaskedAddress{{Address: "quiet.otter.4821@x.test", State: "active"}}}
	nativeOwn(own, map[string]sso.NativeAddress{
		"quiet.otter.4821@x.test": {Mailbox: "m", State: "active"},
		"sales@x.test":            {Mailbox: "m", State: "active"},
		"paused@x.test":           {Mailbox: "m", State: "reserved"},
		"theirs@x.test":           {Mailbox: "other", State: "active"},
	}, "m", settings)
	if len(own) != 2 || !own["sales@x.test"] {
		t.Fatalf("own %v", own)
	}
	masked := strings.Replace(oooBase, "To: bob@x.test", "To: quiet.otter.4821@x.test", 1)
	if _, reason := oooEligible(oooHeader(t, masked), own); reason != "not_addressed" {
		t.Fatal("masked address answered", reason)
	}
	tagged := strings.Replace(oooBase, "To: bob@x.test", "To: Bob+Lists@x.test", 1)
	if _, reason := oooEligible(oooHeader(t, tagged), own); reason != "" {
		t.Fatal("plus address not answered", reason)
	}
}

// A native delivery whose receipt names a masked address gets no reply even
// when To names a regular owned address; the same mail delivered to the
// regular address is answered.
func TestOutOfOfficeSkipsMaskedDelivery(t *testing.T) {
	h := newOOOHarness(t)
	h.member = true
	if err := config.UpdateUserSettings(config.UserSettingsPath(h.p.configDir, h.userID), func(s *config.UserSettings) error {
		s.MaskedAddresses = []config.MaskedAddress{{Address: "quiet.otter.4821@x.test", State: "active"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for delivered, want := range map[string]int{"quiet.otter.4821@x.test": 0, "bob@x.test": 1} {
		h.queued = nil
		uc := h.ctx(t, oooOn(), []byte(oooBase+"\r\nhi\r\n"))
		uc.mailbox = h.userID
		h.p.outOfOfficeReply(t.Context(), uc, imapadapter.Message{ID: "1", DeliveredTo: []string{delivered}}, "bob@x.test")
		if len(h.queued) != want {
			t.Fatalf("delivered to %s: %d replies, want %d", delivered, len(h.queued), want)
		}
	}
}
