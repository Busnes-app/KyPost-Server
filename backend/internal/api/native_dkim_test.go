//go:build linux

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

type dkimEntry struct {
	Domain, Selector, State, RecordName, RecordType, RecordValue string
	CreatedAt, RotationRecommendedAt, RetiredAt, KeepPublishedUntil  *time.Time
	AgeDays                                                          *int
	RotationRecommended                                              *bool
}

type dkimFixture struct {
	t            *testing.T
	s            *Server
	token, csrf  string
	secret       string
	resolver     *recordDNS
	path, keyPath string
	now          time.Time
	bodies       [][]byte
}

// newDKIMFixture saves a direct profile for example.test through the API.
func newDKIMFixture(t *testing.T) *dkimFixture {
	t.Helper()
	s := newDirectoryTestServer(t)
	ctx := context.Background()
	const password = "dkim-rotation-admin-password"
	admin, err := s.users.Create(ctx, "rotation-admin", password, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	f := &dkimFixture{t: t, s: s, resolver: &recordDNS{txt: map[string][]string{}}, now: time.Now()}
	f.token, f.csrf = mintSessionForTest(s, admin.ID)
	f.secret = userAuthSecret(s, admin.ID, password)
	f.path, f.keyPath = filepath.Join(s.configDir, "native-relay.json"), filepath.Join(config.SecretDir(), "native-relay.key")
	s.dnsResolver = f.resolver
	s.dkimNow = func() time.Time { return f.now }
	if _, err := s.nativeDomains.ConfigureDomain(ctx, "example.test", s.ssoStore.Load().IssuerURL); err != nil {
		t.Fatal(err)
	}
	s.nativeDomains.SetLookupForTest(func(_ context.Context, name string) ([]string, error) {
		set, err := s.nativeDomains.ReadSet()
		for _, d := range set.Domains {
			if name == d.RecordName()+"." {
				return []string{d.RecordValue()}, err
			}
		}
		return nil, &net.DNSError{IsNotFound: true}
	})
	if w := f.call("PUT", "/api/admin/mail-relay", map[string]any{"mode": "direct", "host": "mail.example.test"}); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	return f
}

// call sends body with the admin's step-up credential, CSRF and cookie.
func (f *dkimFixture) call(method, path string, body map[string]any) *httptest.ResponseRecorder {
	f.t.Helper()
	if body != nil {
		if _, ok := body["authSecret"]; !ok {
			body["authSecret"] = f.secret
		}
	}
	raw, _ := json.Marshal(body)
	if body == nil {
		raw = nil
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.AddCookie(&http.Cookie{Name: "kypost_session", Value: f.token})
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", f.csrf)
	w := httptest.NewRecorder()
	f.s.routes().ServeHTTP(w, req)
	f.bodies = append(f.bodies, w.Body.Bytes())
	return w
}

func (f *dkimFixture) rotate(body map[string]any) *httptest.ResponseRecorder {
	return f.call("POST", "/api/admin/mail-relay/dkim/rotate", body)
}

// confirm resets the shared DNS limit first, so each step gets its own check.
func (f *dkimFixture) confirm() *httptest.ResponseRecorder {
	f.s.deliverabilityCooldown = newCooldown(10 * time.Second)
	return f.call("POST", "/api/admin/mail-relay/dkim/confirm", map[string]any{"domain": "example.test"})
}

func (f *dkimFixture) entries() []dkimEntry {
	f.t.Helper()
	w := f.call("GET", "/api/admin/mail-relay", nil)
	var status struct{ DKIM []dkimEntry }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil {
		f.t.Fatal("status", w.Code, w.Body)
	}
	return status.DKIM
}

func (f *dkimFixture) states() []string {
	var states []string
	for _, e := range f.entries() {
		states = append(states, e.State+":"+e.Selector)
	}
	return states
}

func (f *dkimFixture) profile() mailmsg.DomainRelay {
	f.t.Helper()
	c, _, err := mailmsg.ReadDomainRelay(f.path, f.keyPath)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *dkimFixture) fileBytes() []byte {
	raw, _ := os.ReadFile(f.path)
	return raw
}

func (f *dkimFixture) publish(e dkimEntry) {
	f.resolver.txt[e.RecordName+"."] = []string{e.RecordValue}
}

// remember records every private key the profile has held so far.
func (f *dkimFixture) remember(seen map[string]mailmsg.DKIMKey) {
	for _, k := range f.profile().DKIM {
		if len(k.Private) > 0 {
			seen[k.Selector] = k
		}
	}
}

func (f *dkimFixture) requireNoKeyMaterial(out []byte, seen map[string]mailmsg.DKIMKey) {
	f.t.Helper()
	keys := make([]mailmsg.DKIMKey, 0, len(seen))
	for _, k := range seen {
		keys = append(keys, k)
	}
	requireNoDKIMPrivate(f.t, out, mailmsg.DomainRelay{DKIM: keys})
}

func TestDKIMRotationAPIEndToEnd(t *testing.T) {
	f := newDKIMFixture(t)
	logs := captureServerLogs(t, f.s)
	seen := map[string]mailmsg.DKIMKey{}
	f.remember(seen)
	first := f.entries()
	if len(first) != 1 || first[0].State != "active" || first[0].CreatedAt == nil || first[0].AgeDays == nil || *first[0].AgeDays != 0 ||
		first[0].RotationRecommended == nil || *first[0].RotationRecommended || !first[0].RotationRecommendedAt.Equal(first[0].CreatedAt.Add(180*24*time.Hour)) {
		t.Fatal("active entry", first)
	}
	active := f.profile().DKIM[0]
	// 180 days later the key is only recommended for rotation.
	f.now = first[0].CreatedAt.Add(180 * 24 * time.Hour)
	if e := f.entries()[0]; !*e.RotationRecommended || *e.AgeDays != 180 {
		t.Fatal("rotation not recommended at 180 days", e)
	}
	f.now = time.Now()

	w := f.rotate(map[string]any{"domain": "example.test"})
	var prepared struct {
		Record dkimEntry
		DKIM   []dkimEntry
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &prepared) != nil || prepared.Record.State != "next" || prepared.Record.RecordValue == "" ||
		prepared.Record.Selector == first[0].Selector || !regexp.MustCompile(`^kypost-[0-9a-f]{8}$`).MatchString(prepared.Record.Selector) {
		t.Fatal("prepare", w.Code, w.Body)
	}
	f.remember(seen)
	if a := f.profile().DKIM[0]; a.State != "active" || !bytes.Equal(a.Private, active.Private) {
		t.Fatal("prepare changed the active key")
	}
	before := f.fileBytes()
	if w := f.rotate(map[string]any{"domain": "example.test"}); w.Code != 200 || !strings.Contains(w.Body.String(), prepared.Record.Selector) || !bytes.Equal(before, f.fileBytes()) {
		t.Fatal("second prepare", w.Code)
	}
	if got := f.states(); !slices.Equal(got, []string{"active:" + first[0].Selector, "next:" + prepared.Record.Selector}) {
		t.Fatal("states after prepare", got)
	}
	// Not live: 409 with the check, and the shared DNS limit applies.
	if w := f.confirm(); w.Code != 409 || !strings.Contains(w.Body.String(), "not live yet (fail)") {
		t.Fatal("unpublished next confirmed", w.Code, w.Body)
	}
	limited := f.call("POST", "/api/admin/mail-relay/dkim/confirm", map[string]any{"domain": "example.test"})
	if retry, _ := strconv.Atoi(limited.Header().Get("Retry-After")); limited.Code != 429 || retry < 1 {
		t.Fatal("confirm not rate limited", limited.Code)
	}
	f.publish(prepared.Record)
	// A live record alone switches nothing: the tick never does.
	tick := DKIMRotationFor(f.s.configDir, f.resolver)
	if _, events, err := tick.Tick(context.Background(), time.Now()); err != nil || len(events) != 0 {
		t.Fatal("tick switched", events, err)
	}
	gen := f.profile().Generation
	if w := f.confirm(); w.Code != 200 {
		t.Fatal("live next not switched", w.Code, w.Body)
	}
	entries := f.entries()
	if len(entries) != 2 || entries[0].State != "active" || entries[0].Selector != prepared.Record.Selector || entries[1].State != "retiring" ||
		entries[1].Selector != first[0].Selector || !entries[1].KeepPublishedUntil.Equal(entries[1].RetiredAt.Add(7*24*time.Hour)) {
		t.Fatal("states after switch", entries)
	}
	if f.profile().Generation != gen {
		t.Fatal("switch changed the generation")
	}
	if w := f.rotate(map[string]any{"domain": "example.test"}); w.Code != 409 || !strings.Contains(w.Body.String(), "finish the previous rotation first") {
		t.Fatal("rotation while retiring", w.Code, w.Body)
	}
	if w := f.confirm(); w.Code != 409 {
		t.Fatal("confirming again", w.Code)
	}
	// Destruction after 7 days, by the tick.
	if _, events, err := tick.Tick(context.Background(), entries[1].KeepPublishedUntil.Add(time.Second)); err != nil || len(events) != 1 {
		t.Fatal("destruction", events, err)
	}
	entries = f.entries()
	if len(entries) != 2 || entries[1].State != "removable" || entries[1].RecordValue != "" || entries[1].RecordName != first[0].RecordName {
		t.Fatal("removable entry", entries)
	}
	for _, phase := range []string{`"phase":"prepare"`, `"phase":"switch"`, `"trigger":"manual"`, `"message":"dkim rotation"`} {
		if !bytes.Contains(logs.Bytes(), []byte(phase)) {
			t.Fatal("log lacks", phase, logs.String())
		}
	}
	f.requireNoKeyMaterial(logs.Bytes(), seen)
	for _, b := range f.bodies {
		f.requireNoKeyMaterial(b, seen)
	}
}

func TestDKIMRotationAPIEmergencyAndRefusals(t *testing.T) {
	f := newDKIMFixture(t)
	seen := map[string]mailmsg.DKIMKey{}
	f.remember(seen)
	old := f.entries()[0]
	before := f.fileBytes()
	if w := f.rotate(map[string]any{"domain": "example.test", "emergency": true}); w.Code != 400 || !bytes.Equal(before, f.fileBytes()) {
		t.Fatal("emergency without acceptGap", w.Code)
	}
	if w := f.rotate(map[string]any{"domain": "other.test"}); w.Code != 404 {
		t.Fatal("unknown domain", w.Code)
	}
	if w := f.rotate(map[string]any{"domain": "example.test"}); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	f.remember(seen)
	w := f.rotate(map[string]any{"domain": "example.test", "emergency": true, "acceptGap": true})
	var answer struct {
		Warning string
		Record  dkimEntry
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &answer) != nil {
		t.Fatal("emergency", w.Code, w.Body)
	}
	want := "Mail from example.test will fail DKIM until you publish " + answer.Record.RecordName + ". Receivers may reject it or mark it as spam. Remove the old record " + old.RecordName + " now."
	if answer.Warning != want || answer.Record.State != "active" {
		t.Fatal("emergency warning", answer.Warning)
	}
	f.remember(seen)
	entries := f.entries()
	if len(entries) != 2 || entries[0].Selector != answer.Record.Selector || entries[1].State != "removable" || entries[1].Selector != old.Selector {
		t.Fatal("emergency states", entries)
	}
	for _, k := range f.profile().DKIM {
		if k.State == mailmsg.DKIMRemovable && len(k.Private) != 0 {
			t.Fatal("compromised key kept")
		}
	}
	for _, b := range f.bodies {
		f.requireNoKeyMaterial(b, seen)
	}
	// Restore hold and relay mode refuse every key action.
	hold := filepath.Join(f.s.stateDir, sso.NativeRestoreHoldFile)
	if err := os.WriteFile(hold, []byte("held"), 0600); err != nil {
		t.Fatal(err)
	}
	if w := f.rotate(map[string]any{"domain": "example.test"}); w.Code != 409 {
		t.Fatal("rotation under a restore hold", w.Code)
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	if w := f.call("PUT", "/api/admin/mail-relay", map[string]any{"host": "smtp.example.test", "smtpUsername": "u", "smtpPassword": "p"}); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w := f.rotate(map[string]any{"domain": "example.test"}); w.Code != 409 {
		t.Fatal("relay profile rotated", w.Code)
	}
	if w := f.confirm(); w.Code != 409 {
		t.Fatal("relay profile confirmed", w.Code)
	}
}

// Every key action takes the relay PUT's step-up, and nothing returns key material.
func TestDKIMKeyLockRequiresStepUp(t *testing.T) {
	type action struct {
		name, path string
		body       map[string]any
	}
	actions := []action{
		{"rotate", "/api/admin/mail-relay/dkim/rotate", map[string]any{"domain": "example.test"}},
		{"emergency", "/api/admin/mail-relay/dkim/rotate", map[string]any{"domain": "example.test", "emergency": true, "acceptGap": true}},
		{"confirm", "/api/admin/mail-relay/dkim/confirm", map[string]any{"domain": "example.test"}},
	}
	relayPut := action{"relay put", "/api/admin/mail-relay", map[string]any{"mode": "direct", "host": "mail.example.test"}}
	clone := func(m map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	// send runs one action under a refusal mode and returns its status.
	send := func(f *dkimFixture, a action, mode string) int {
		body := clone(a.body)
		method := "POST"
		if a.path == "/api/admin/mail-relay" {
			method = "PUT"
		}
		switch mode {
		case "no secret":
			body["authSecret"] = ""
		case "wrong password":
			body["authSecret"] = userAuthSecret(f.s, f.mustUser().ID, "not-the-password")
		case "sso without grant":
			f.s.sessMu.Lock()
			session := f.s.sessions[f.token]
			session.SSOKySignOn, session.SSOAppAdmin = true, true
			session.SSO = sso.SessionIdentity{Issuer: f.s.ssoStore.Load().IssuerURL, Subject: "rotation-admin-sub"}
			f.s.sessions[f.token] = session
			f.s.sessMu.Unlock()
		case "no digest":
			f.s.sessMu.Lock()
			session := f.s.sessions[f.token]
			session.SSOKySignOn, session.SSOAppAdmin = true, true
			f.s.sessions[f.token] = session
			f.s.sessMu.Unlock()
			raw, _ := json.Marshal(body)
			req := httptest.NewRequest(method, a.path, bytes.NewReader(raw))
			req.AddCookie(&http.Cookie{Name: "kypost_session", Value: f.token})
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-CSRF-Token", f.csrf)
			w := httptest.NewRecorder()
			handler := map[string]http.HandlerFunc{"/api/admin/mail-relay": f.s.handleNativeMailRelay, "/api/admin/mail-relay/dkim/rotate": f.s.handleDKIMRotate, "/api/admin/mail-relay/dkim/confirm": f.s.handleDKIMConfirm}[a.path]
			f.s.withAdmin(handler)(w, req)
			f.bodies = append(f.bodies, w.Body.Bytes())
			return w.Code
		}
		f.s.deliverabilityCooldown = newCooldown(10 * time.Second)
		return f.call(method, a.path, body).Code
	}
	for _, mode := range []string{"no secret", "wrong password", "sso without grant", "no digest"} {
		for _, a := range actions {
			t.Run(mode+"/"+a.name, func(t *testing.T) {
				f := newDKIMFixture(t)
				if a.name == "confirm" {
					f.rotate(map[string]any{"domain": "example.test"})
					f.publish(f.entries()[1])
				}
				want := send(newDKIMFixture(t), relayPut, mode)
				before := f.fileBytes()
				got := send(f, a, mode)
				if got < 400 || got != want || !bytes.Equal(before, f.fileBytes()) {
					t.Fatalf("%s %s: got %d, relay PUT %d, or the file changed", mode, a.name, got, want)
				}
				seen := map[string]mailmsg.DKIMKey{}
				f.remember(seen)
				for _, b := range f.bodies {
					f.requireNoKeyMaterial(b, seen)
				}
			})
		}
	}
	t.Run("valid step-up", func(t *testing.T) {
		for _, a := range actions {
			f := newDKIMFixture(t)
			if a.name == "confirm" {
				f.rotate(map[string]any{"domain": "example.test"})
				f.publish(f.entries()[1])
			}
			if got := send(f, a, "valid"); got != 200 {
				t.Fatal(a.name, got)
			}
		}
	})
	t.Run("routes", func(t *testing.T) {
		raw, err := os.ReadFile("server.go")
		if err != nil {
			t.Fatal(err)
		}
		var routes []string
		for _, m := range regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+ /api/admin/mail-relay[^"]*)"`).FindAllSubmatch(raw, -1) {
			routes = append(routes, string(m[1]))
		}
		slices.Sort(routes)
		want := []string{"GET /api/admin/mail-relay", "GET /api/admin/mail-relay/deliverability", "POST /api/admin/mail-relay/dkim/confirm", "POST /api/admin/mail-relay/dkim/rotate", "POST /api/admin/mail-relay/test", "PUT /api/admin/mail-relay"}
		if !slices.Equal(routes, want) {
			t.Fatal("mail-relay routes", routes)
		}
	})
}

func (f *dkimFixture) mustUser() users.User {
	u, err := f.s.users.GetByUsername("rotation-admin")
	if err != nil {
		f.t.Fatal(err)
	}
	return u
}
