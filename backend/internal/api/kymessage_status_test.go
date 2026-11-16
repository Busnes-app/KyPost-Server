package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/Busnes-app/ky-primitives/syncauth"

	"github.com/Busnes-app/kypost-server/backend/internal/suitepair"
)

// statusConsole is KyMessages' lookup route: kypost.presence.lookup only,
// signature checked, the signed body recorded.
type statusConsole struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
	status int
}

func pairStatusConsole(t *testing.T, f *chatFixture) *statusConsole {
	c := &statusConsole{status: 200}
	c.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h := syncauth.FromRequest(r)
		if r.URL.Path != kymessagePathStatus || h.EventType != kymessageEventStatus {
			t.Errorf("%s %s", r.URL.Path, h.EventType)
		}
		if _, err := syncauth.Verify(chatSecret, h, body, syncauth.Options{}); err != nil {
			http.Error(w, `{"code":"bad_signature"}`, http.StatusUnauthorized)
			return
		}
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		c.mu.Lock()
		c.bodies = append(c.bodies, req)
		status := c.status
		c.mu.Unlock()
		var people []string
		for _, s := range req["subs"].([]any) {
			switch s {
			case "s-bob":
				people = append(people, `{"sub":"s-bob","state":"in_a_meeting","until":"2026-11-16T15:00:00Z"}`)
			case "s-cy":
				people = append(people, `{"sub":"s-cy","state":"busy","until":"2026-11-16T15:00:00Z"}`)
			default:
				people = append(people, fmt.Sprintf(`{"sub":%q,"state":"on_fire"}`, s))
			}
		}
		people = append(people, `{"sub":"s-stranger","state":"available"}`)
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"people":[%s]}`, strings.Join(people, ","))
	}))
	t.Cleanup(c.Close)
	base, _ := url.Parse(c.URL)
	f.srv.SetKyMessage(suitepair.New(KyMessagePeer, base, chatSecret, c.Client().Transport))
	return c
}

func TestKyMessageStatus(t *testing.T) {
	f := newChatFixture(t)
	c := pairStatusConsole(t, f)
	code, out, _ := f.call(t, f.ann, "/api/kymessage/status", `{"addresses":["Bob@Example.com","cy@example.com","dee@example.com","out@other.org","not an address"]}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	got, _ := json.Marshal(out["people"])
	want := `[{"address":"bob@example.com","state":"in_a_meeting","until":"2026-11-16T15:00:00Z"},{"address":"cy@example.com","state":"busy"},{"address":"dee@example.com","state":"unknown"}]`
	if string(got) != want {
		t.Fatalf("people %s", got)
	}
	// The signed body names the caller and organisation subjects only: never an address.
	if len(c.bodies) != 1 || fmt.Sprint(c.bodies[0]["subs"]) != "[s-bob s-cy s-dee]" || c.bodies[0]["sub"] != "sub-ann" || c.bodies[0]["issuer"] != f.issuer || len(c.bodies[0]) != 3 {
		t.Fatalf("sent %v", c.bodies)
	}
	// Answers are kept a minute.
	f.call(t, f.ann, "/api/kymessage/status", `{"addresses":["bob@example.com"]}`)
	if len(c.bodies) != 1 {
		t.Fatalf("not cached: %d", len(c.bodies))
	}
	f.srv.kymessageStatus.mu.Lock()
	for k, e := range f.srv.kymessageStatus.m {
		e.at = e.at.Add(-statusCacheFor)
		f.srv.kymessageStatus.m[k] = e
	}
	f.srv.kymessageStatus.mu.Unlock()
	f.call(t, f.ann, "/api/kymessage/status", `{"addresses":["bob@example.com"]}`)
	if len(c.bodies) != 2 {
		t.Fatalf("kept past a minute: %d", len(c.bodies))
	}

	many := strings.TrimSuffix(strings.Repeat(`"a@example.com",`, 51), ",")
	if code, out, _ := f.call(t, f.ann, "/api/kymessage/status", `{"addresses":[`+many+`]}`); code != 400 || out["code"] != "too_many" {
		t.Fatalf("over the cap: %d %v", code, out)
	}
	c.status = 503
	if code, out, _ := f.call(t, f.ann, "/api/kymessage/status", `{"addresses":["eve@example.com"]}`); code != 502 || out["code"] != "kymessage_unavailable" {
		t.Fatalf("console down: %d %v", code, out)
	}
	// Off: no Element in the suite list, or unpaired.
	bob := ssoUser(t, f.srv, "bob", f.issuer, "sub-bob")
	if code, out, _ := f.call(t, bob, "/api/kymessage/status", `{"addresses":["cy@example.com"]}`); code != 409 || out["code"] != "kymessage_off" {
		t.Fatalf("off: %d %v", code, out)
	}
	f.srv.SetKyMessage(nil)
	if code, _, _ := f.call(t, f.ann, "/api/kymessage/status", `{"addresses":["cy@example.com"]}`); code != 409 {
		t.Fatalf("unpaired: %d", code)
	}
}
