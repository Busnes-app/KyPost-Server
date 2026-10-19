package processor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/kycalendar"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

type inviteMailClient struct {
	*noopMailClient
	raw map[int][]byte
}

func (c inviteMailClient) FetchRawMessage(_ context.Context, mailbox string, uid int) ([]byte, error) {
	if mailbox != "" {
		return nil, nil
	}
	return c.raw[uid], nil
}

const forwardICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:e1@example.com\r\nORGANIZER:mailto:alice@example.com\r\nATTENDEE:mailto:bob@x.test\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func inviteMail(from, ics string) []byte {
	return []byte("From: " + from + "\r\nTo: bob@x.test\r\nSubject: Planning\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nInvite\r\n" +
		"--b\r\nContent-Type: text/calendar; method=REQUEST; charset=UTF-8\r\n\r\n" + ics + "--b--\r\n")
}

type fakeKyCalendar struct {
	mu     sync.Mutex
	bodies []map[string]any
	ids    []string
	status int
}

func newForwardPoller(t *testing.T) (*Poller, *fakeKyCalendar) {
	t.Helper()
	logger, err := logging.New()
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeKyCalendar{status: http.StatusOK}
	secret := []byte(strings.Repeat("k", kycalendar.MinSecretBytes))
	var receiver *kycalendar.Client
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ev, err := receiver.Verify(r, kycalendar.EventInbound, body)
		if err != nil || r.URL.Path != "/api/itip/inbound" {
			t.Errorf("KyCalendar got %s: %v", r.URL.Path, err)
		}
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.bodies = append(fake.bodies, v)
		fake.ids = append(fake.ids, ev.ID)
		receiver.Forget(ev.ID) // so a retry is seen, not deduplicated
		w.WriteHeader(fake.status)
		_, _ = w.Write([]byte(`{"outcome":"applied"}`))
	}))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	receiver = kycalendar.New(base, secret, srv.Client().Transport)
	p := &Poller{log: logger, stateDir: t.TempDir()}
	p.SetKyCalendar(kycalendar.New(base, secret, srv.Client().Transport))
	return p, fake
}

func stubAlignment(t *testing.T, aligned map[string]string) {
	previous := dkimAlignedFrom
	dkimAlignedFrom = func(raw []byte) string {
		for marker, from := range aligned {
			if strings.Contains(string(raw), marker) {
				return from
			}
		}
		return ""
	}
	t.Cleanup(func() { dkimAlignedFrom = previous })
}

// A REQUEST is forwarded only when its From is DKIM-aligned and the user
// turned KyCalendar on; only an organizer who signed it themselves is recorded
// as provenance for a later REPLY.
func TestForwardInvitation(t *testing.T) {
	stubAlignment(t, map[string]string{"From: alice@example.com": "alice@example.com", "From: relay@other.test": "relay@other.test"})
	user := users.User{ID: "u1", SSOSub: "sub-1", SSOIssuer: "https://id.example.com"}
	for name, tc := range map[string]struct {
		enabled   bool
		user      users.User
		raw       []byte
		forwarded bool
		recorded  bool
	}{
		"aligned":            {enabled: true, user: user, raw: inviteMail("alice@example.com", forwardICS), forwarded: true, recorded: true},
		"setting off":        {enabled: false, user: user, raw: inviteMail("alice@example.com", forwardICS)},
		"no sso link":        {enabled: true, user: users.User{ID: "u1"}, raw: inviteMail("alice@example.com", forwardICS)},
		"revoked link":       {enabled: true, user: users.User{ID: "u1", SSOSub: "s", SSOIssuer: "i", SSOLinkRevokedAt: 1}, raw: inviteMail("alice@example.com", forwardICS)},
		"not aligned":        {enabled: true, user: user, raw: inviteMail("mallory@example.com", forwardICS)},
		"aligned, other org": {enabled: true, user: user, raw: inviteMail("relay@other.test", forwardICS), forwarded: true},
		"not an invitation":  {enabled: true, user: user, raw: inviteMail("alice@example.com", strings.Replace(forwardICS, "METHOD:REQUEST", "METHOD:PUBLISH", 1))},
	} {
		t.Run(name, func(t *testing.T) {
			p, fake := newForwardPoller(t)
			uc := userCtx{id: "u1", mail: inviteMailClient{&noopMailClient{}, map[int][]byte{7: tc.raw}}}
			uc.kycalSub, uc.kycalIssuer = p.kyCalendarIdentity(tc.user, tc.enabled)
			p.forwardInvitation(t.Context(), uc, imapadapter.Message{ID: "7", HasAttachments: true})
			if got := len(fake.bodies) == 1; got != tc.forwarded {
				t.Fatalf("forwarded = %v (%d), want %v", got, len(fake.bodies), tc.forwarded)
			}
			if tc.forwarded {
				b := fake.bodies[0]
				if b["sub"] != "sub-1" || b["issuer"] != "https://id.example.com" || b["dkim_aligned"] != true ||
					b["ics"] != forwardICS || !strings.HasPrefix(fake.ids[0], "kypost-inbound-") {
					t.Fatalf("forwarded %v as %s", b, fake.ids[0])
				}
			}
			recorded, err := kycalendar.OpenInvitations(p.userStateDir("u1")).Has("e1@example.com", "alice@example.com")
			if err != nil || recorded != tc.recorded {
				t.Fatalf("recorded = %v %v, want %v", recorded, err, tc.recorded)
			}
		})
	}
}

// KyCalendar failing is retried on later ticks under the same event ID, a
// bounded number of times, and never touches the message.
func TestForwardInvitationRetriesBounded(t *testing.T) {
	stubAlignment(t, map[string]string{"From: alice@example.com": "alice@example.com"})
	p, fake := newForwardPoller(t)
	fake.status = http.StatusServiceUnavailable
	uc := userCtx{id: "u1", kycalSub: "sub-1", kycalIssuer: "iss", mail: inviteMailClient{&noopMailClient{}, map[int][]byte{7: inviteMail("alice@example.com", forwardICS)}}}
	p.forwardInvitation(t.Context(), uc, imapadapter.Message{ID: "7", HasAttachments: true})
	for range 5 {
		p.retryForwards(t.Context(), "u1")
	}
	if len(fake.ids) != maxForwardAttempts {
		t.Fatalf("attempts = %d, want %d", len(fake.ids), maxForwardAttempts)
	}
	for _, id := range fake.ids {
		if id != fake.ids[0] {
			t.Fatalf("event ID changed across retries: %v", fake.ids)
		}
	}
	if len(p.itip.pending) != 0 {
		t.Fatalf("still pending after giving up: %d", len(p.itip.pending))
	}
}
