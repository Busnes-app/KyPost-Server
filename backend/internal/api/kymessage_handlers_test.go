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

	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/suitepair"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

var chatSecret = []byte(strings.Repeat("m", 32))

// fakeConsole is the KyMessages console's side of contract H: one route, the
// kypost event type only, signature with a replay guard, caller and target
// by subject.
type fakeConsole struct {
	*httptest.Server
	mu      sync.Mutex
	issuer  string
	users   map[string]bool // subjects with an active KyMessages account
	seen    map[string]bool
	bodies  []string
	answer  string // overrides the success URL when set
	refusal [2]any // status, code: overrides everything when set
}

func newFakeConsole(t *testing.T, issuer string) *fakeConsole {
	c := &fakeConsole{issuer: issuer, users: map[string]bool{}, seen: map[string]bool{}}
	c.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		refuse := func(status int, code string) {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"error":"refused","code":%q}`, code)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/suite/chat" {
			refuse(http.StatusNotFound, "not_found")
			return
		}
		body, _ := io.ReadAll(r.Body)
		h := syncauth.FromRequest(r)
		if h.EventType != "kypost.chat.open" {
			refuse(http.StatusUnauthorized, "bad_signature")
			return
		}
		if _, err := syncauth.Verify(chatSecret, h, body, syncauth.Options{}); err != nil {
			refuse(http.StatusUnauthorized, "bad_signature")
			return
		}
		if c.seen[h.EventID] {
			_, _ = w.Write([]byte(`{"outcome":"duplicate"}`))
			return
		}
		c.seen[h.EventID] = true
		c.bodies = append(c.bodies, string(body))
		if c.refusal[0] != nil {
			refuse(c.refusal[0].(int), c.refusal[1].(string))
			return
		}
		var req struct {
			Sub       string `json:"sub"`
			Issuer    string `json:"issuer"`
			TargetSub string `json:"target_sub"`
		}
		switch {
		case json.Unmarshal(body, &req) != nil || req.Sub == "" || req.TargetSub == "":
			refuse(http.StatusBadRequest, "bad_body")
		case req.Issuer != c.issuer || !c.users[req.Sub]:
			refuse(http.StatusNotFound, "no_user")
		case !c.users[req.TargetSub] || req.TargetSub == req.Sub:
			refuse(http.StatusNotFound, "no_target")
		default:
			u := "https://chat.example.com/#/user/@" + strings.TrimPrefix(req.TargetSub, "s-") + ":example.com"
			if c.answer != "" {
				u = c.answer
			}
			fmt.Fprintf(w, `{"url":%q}`, u)
		}
	}))
	t.Cleanup(c.Close)
	return c
}

type chatFixture struct {
	srv     *Server
	console *fakeConsole
	ann     users.User
	issuer  string
	logs    *syncLog
}

// newChatFixture: KyPost signs in through a KyIdentity that lists Element for
// sub-ann only and knows every @example.com address (ann's own as sub-ann);
// the console is paired and has ann and bob.
func newChatFixture(t *testing.T) *chatFixture {
	t.Helper()
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, secret, ok := r.BasicAuth(); !ok || id != "kypost" || secret != "suite-secret" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/suite/people":
			emails := r.URL.Query()["email"]
			if r.URL.Query().Get("sub") != "sub-ann" || len(emails) == 0 || len(emails) > 100 || emails[0] == "down@example.com" {
				http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
				return
			}
			var people []string
			for _, e := range emails {
				sub := "s-" + strings.TrimSuffix(e, "@example.com")
				if e == "ann@example.com" {
					sub = "sub-ann"
				}
				if strings.HasSuffix(e, "@example.com") {
					people = append(people, fmt.Sprintf(`{"sub":%q,"name":"N","email":%q}`, sub, strings.ToUpper(e)))
				}
			}
			fmt.Fprintf(w, `{"people":[%s]}`, strings.Join(people, ","))
		case "/api/suite/apps":
			apps := `{"id":"kypost","name":"KyPost","url":"https://mail.example.com/"}`
			if r.URL.Query().Get("sub") == "sub-ann" {
				apps += `,{"id":"kymessage-chat","name":"KyMessages","url":"https://chat.example.com/"}`
			}
			fmt.Fprintf(w, `{"apps":[%s]}`, apps)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(idp.Close)
	s := newTestServer(t)
	if err := s.ssoStore.Save(sso.SSOSettings{Enabled: true, IssuerURL: idp.URL + "/", ClientID: "kypost", ClientSecret: "suite-secret"}); err != nil {
		t.Fatal(err)
	}
	logs := &syncLog{}
	logger, err := logging.NewWithOutput(logs)
	if err != nil {
		t.Fatal(err)
	}
	s.logger = logger
	ann := ssoUser(t, s, "ann", idp.URL, "sub-ann")
	console := newFakeConsole(t, idp.URL)
	console.users["sub-ann"], console.users["s-bob"] = true, true
	f := &chatFixture{srv: s, console: console, ann: ann, issuer: idp.URL, logs: logs}
	f.pair(chatSecret)
	return f
}

func (f *chatFixture) pair(secret []byte) {
	base, _ := url.Parse(f.console.URL)
	f.srv.SetKyMessage(suitepair.New(KyMessagePeer, base, secret, f.console.Client().Transport))
}

func (f *chatFixture) call(t *testing.T, u users.User, path, body string) (int, map[string]any, http.Header) {
	t.Helper()
	rec := requestBodyAs(f.srv, u.ID, "POST", path, body)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Header().Get("Cache-Control") != "no-store" && rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("%s: not no-store", path)
	}
	return rec.Code, out, rec.Header()
}

func TestKyMessagePeople(t *testing.T) {
	f := newChatFixture(t)
	code, out, _ := f.call(t, f.ann, "/api/kymessage/people", `{"addresses":["Bob@Example.com","out@other.org","ann@example.com","bob@example.com"]}`)
	if code != 200 || fmt.Sprint(out["inside"]) != "[bob@example.com]" {
		t.Fatalf("people %d %v", code, out)
	}
	many := strings.TrimSuffix(strings.Repeat(`"a@example.com",`, 101), ",")
	for name, body := range map[string]string{
		"none":         `{"addresses":[]}`,
		"too many":     `{"addresses":[` + many + `]}`,
		"display name": `{"addresses":["Bob <bob@example.com>"]}`,
		"not json":     `x`,
	} {
		if code, out, _ := f.call(t, f.ann, "/api/kymessage/people", body); code != 400 || out["code"] != "bad_request" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if code, out, _ := f.call(t, f.ann, "/api/kymessage/people", `{"addresses":["down@example.com"]}`); code != 503 || out["code"] != "unavailable" {
		t.Fatalf("KyIdentity down: %d %v", code, out)
	}
}

// Chat is off, and nothing reaches KyIdentity's people or the console, for
// anyone without Element in their suite list, no SSO link, or while unpaired.
func TestKyMessageOff(t *testing.T) {
	f := newChatFixture(t)
	bob := ssoUser(t, f.srv, "bob", f.issuer, "sub-bob")
	local := ssoUser(t, f.srv, "loc", "", "")
	for _, u := range []users.User{bob, local} {
		for _, path := range []string{"/api/kymessage/people", "/api/kymessage/chat"} {
			if code, out, _ := f.call(t, u, path, `{"addresses":["bob@example.com"],"address":"bob@example.com"}`); code != 409 || out["code"] != "kymessage_off" {
				t.Fatalf("%s %s: %d %v", u.Username, path, code, out)
			}
		}
	}
	f.srv.SetKyMessage(nil)
	if code, out, _ := f.call(t, f.ann, "/api/kymessage/chat", `{"address":"bob@example.com"}`); code != 409 || out["code"] != "kymessage_off" {
		t.Fatalf("unpaired: %d %v", code, out)
	}
	if len(f.console.bodies) != 0 {
		t.Fatalf("console asked while off: %v", f.console.bodies)
	}
	w := httptest.NewRecorder()
	f.srv.routes().ServeHTTP(w, httptest.NewRequest("POST", "/api/kymessage/chat", strings.NewReader(`{}`)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no session: %d", w.Code)
	}
}

func TestKyMessageChat(t *testing.T) {
	f := newChatFixture(t)
	code, out, _ := f.call(t, f.ann, "/api/kymessage/chat", `{"address":"Bob@example.com"}`)
	if code != 200 || out["url"] != "https://chat.example.com/#/user/@bob:example.com" {
		t.Fatalf("chat %d %v", code, out)
	}
	if want := `{"issuer":"` + f.issuer + `","sub":"sub-ann","target_sub":"s-bob"}`; len(f.console.bodies) != 1 || f.console.bodies[0] != want {
		t.Fatalf("console saw %q", f.console.bodies)
	}
	if logs := f.logs.String(); strings.Contains(logs, "bob") || strings.Contains(logs, "sub-ann") || strings.Contains(logs, "chat.example.com") {
		t.Fatalf("logs carry an address, subject or URL: %s", logs)
	}

	// Refused here, before the console: outside the organisation, the caller, a bad address.
	for name, body := range map[string]string{"outside": `{"address":"out@other.org"}`, "self": `{"address":"ann@example.com"}`} {
		if code, out, _ := f.call(t, f.ann, "/api/kymessage/chat", body); code != 404 || out["code"] != "no_target" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	for _, body := range []string{`{}`, `{"address":"Bob <bob@example.com>"}`, `{"address":"a@b\r\nX: y"}`, `x`} {
		if code, out, _ := f.call(t, f.ann, "/api/kymessage/chat", body); code != 400 || out["code"] != "bad_request" {
			t.Errorf("%s: %d %v", body, code, out)
		}
	}
	if len(f.console.bodies) != 1 {
		t.Fatalf("console asked for a refused target: %v", f.console.bodies)
	}

	// The console's answers.
	if code, out, _ := f.call(t, f.ann, "/api/kymessage/chat", `{"address":"carol@example.com"}`); code != 404 || out["code"] != "no_target" || out["error"] != "This person isn't on KyMessages yet." {
		t.Fatalf("no_target: %d %v", code, out)
	}
	delete(f.console.users, "sub-ann")
	if code, out, _ := f.call(t, f.ann, "/api/kymessage/chat", `{"address":"bob@example.com"}`); code != 404 || out["code"] != "no_user" {
		t.Fatalf("no_user: %d %v", code, out)
	}
	f.console.users["sub-ann"] = true
	f.console.refusal = [2]any{http.StatusTooManyRequests, "rate_limited"}
	if code, out, h := f.call(t, f.ann, "/api/kymessage/chat", `{"address":"bob@example.com"}`); code != 429 || out["code"] != "rate_limited" || h.Get("Retry-After") == "" {
		t.Fatalf("rate_limited: %d %v", code, out)
	}
	for _, refusal := range [][2]any{{404, "kypost_off"}, {400, "bad_body"}, {500, "internal"}, {404, "rate_limited"}} {
		f.console.refusal = refusal
		if code, out, _ := f.call(t, f.ann, "/api/kymessage/chat", `{"address":"bob@example.com"}`); code != 502 || out["code"] != "kymessage_unavailable" {
			t.Errorf("%v: %d %v", refusal, code, out)
		}
	}
	f.console.refusal = [2]any{}

	// An answer off Element's origin, or not its user view, never reaches the browser.
	for _, bad := range []string{
		"https://evil.example.com/#/user/@bob:example.com",
		"http://chat.example.com/#/user/@bob:example.com",
		"https://u@chat.example.com/#/user/@bob:example.com",
		"https://chat.example.com/x#/user/@bob:example.com",
		"https://chat.example.com/?a=1#/user/@bob:example.com",
		"https://chat.example.com/#/room/!abc:example.com",
		"https://chat.example.com/#/user/@",
		"https://chat.example.com:444/#/user/@bob:example.com",
		"javascript:alert(1)//chat.example.com/#/user/@bob",
	} {
		f.console.answer = bad
		if code, out, _ := f.call(t, f.ann, "/api/kymessage/chat", `{"address":"bob@example.com"}`); code != 502 || out["url"] != nil {
			t.Errorf("%s: %d %v", bad, code, out)
		}
	}
	f.console.answer = ""

	// Our signature refused (a rotated secret): unavailable, not passed through.
	f.pair([]byte(strings.Repeat("x", 32)))
	if code, out, _ := f.call(t, f.ann, "/api/kymessage/chat", `{"address":"bob@example.com"}`); code != 502 || out["code"] != "kymessage_unavailable" {
		t.Fatalf("wrong secret: %d %v", code, out)
	}
}

func TestKyMessageRateLimit(t *testing.T) {
	f := newChatFixture(t)
	f.srv.kymessageLimiter = newKeyedLimiter(1, 0)
	f.call(t, f.ann, "/api/kymessage/people", `{"addresses":["bob@example.com"]}`)
	if code, out, h := f.call(t, f.ann, "/api/kymessage/chat", `{"address":"bob@example.com"}`); code != 429 || out["code"] != "rate_limited" || h.Get("Retry-After") == "" {
		t.Fatalf("second request: %d %v", code, out)
	}
	if len(f.console.bodies) != 0 {
		t.Fatal("a limited request reached the console")
	}
}

// Dedicated administrator identities have no mailbox, so no chat from it either.
func TestKyMessageRefusesAdministratorIdentity(t *testing.T) {
	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "admin-create", 1, adminRuntimeUser(true)))
	u := assertMailboxless(t, s, "native-runtime-one", users.RoleAdmin)
	base, _ := url.Parse("https://console.example.com")
	s.SetKyMessage(suitepair.New(KyMessagePeer, base, chatSecret, http.DefaultTransport))
	for _, path := range []string{"/api/kymessage/people", "/api/kymessage/chat"} {
		assertAdministratorRefusal(t, path, requestBodyAs(s, u.ID, "POST", path, `{}`))
	}
}

func TestAdminKyMessageStatus(t *testing.T) {
	f := newChatFixture(t)
	admin, err := f.srv.users.Create(t.Context(), "root", "root-testpassword", users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if rec := requestAs(f.srv, admin.ID, "GET", "/api/admin/kymessage"); rec.Code != 200 ||
		rec.Body.String() != `{"source":"env","connected":true,"url":"`+f.console.URL+`"}`+"\n" {
		t.Fatalf("admin status %d %s", rec.Code, rec.Body)
	}
	if rec := requestAs(f.srv, f.ann.ID, "GET", "/api/admin/kymessage"); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin got %d", rec.Code)
	}
}
