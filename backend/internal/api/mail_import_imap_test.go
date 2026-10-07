//go:build linux

package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

type fakeMsg struct {
	raw, flags, date string
	size             int // announced RFC822.SIZE when not 0
}

type fakeFolder struct {
	name, attrs string
	msgs        []fakeMsg
	exists      int // announced EXISTS when not 0
}

// fakeIMAP is just enough of an IMAP server, over implicit TLS or STARTTLS,
// to import from; it records every command it receives.
type fakeIMAP struct {
	t          *testing.T
	ln         net.Listener
	tls        *tls.Config
	startTLS   bool
	user, pass string
	folders    []fakeFolder
	release    chan struct{} // closed to let a "Slow" fetch answer
	mu         sync.Mutex
	commands   []string
}

func newFakeIMAP(t *testing.T, startTLS bool, pass string, folders []fakeFolder) *fakeIMAP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIMAP{t: t, ln: ln, tls: &tls.Config{Certificates: []tls.Certificate{fakeCert(t, "imap.example.com")}}, startTLS: startTLS,
		user: "alice@example.com", pass: pass, folders: folders, release: make(chan struct{})}
	t.Cleanup(func() { close(f.release); _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func fakeCert(t *testing.T, name string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func (f *fakeIMAP) roots() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(f.tls.Certificates[0].Leaf)
	return pool
}

func (f *fakeIMAP) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.commands...)
}

var fakeArg = regexp.MustCompile(`^"((?:[^"\\]|\\.)*)"|^\{(\d+)\}$`)

func (f *fakeIMAP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	var w io.Writer = c
	if !f.startTLS {
		tc := tls.Server(c, f.tls)
		if tc.Handshake() != nil {
			return
		}
		r, w = bufio.NewReader(tc), tc
	}
	say := func(lines ...string) {
		for _, l := range lines {
			_, _ = io.WriteString(w, l+"\r\n")
		}
	}
	say("* OK fake ready")
	selected := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		tag, rest, _ := strings.Cut(line, " ")
		verb, args, _ := strings.Cut(rest, " ")
		verb = strings.ToUpper(verb)
		if verb == "UID" {
			sub, more, _ := strings.Cut(args, " ")
			verb, args = "UID "+strings.ToUpper(sub), more
		}
		f.mu.Lock()
		f.commands = append(f.commands, verb+" "+args)
		f.mu.Unlock()
		switch verb {
		case "STARTTLS":
			say(tag + " OK begin TLS")
			tc := tls.Server(c, f.tls)
			if tc.Handshake() != nil {
				return
			}
			r, w = bufio.NewReader(tc), tc
		case "LOGIN":
			var creds []string
			for len(creds) < 2 {
				args = strings.TrimLeft(args, " ")
				m := fakeArg.FindStringSubmatch(args)
				if m == nil {
					say(tag + " BAD syntax")
					return
				}
				if m[2] == "" {
					creds = append(creds, strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(m[1]))
					args = args[len(m[0]):]
					continue
				}
				n, _ := strconv.Atoi(m[2])
				say("+ go")
				buf := make([]byte, n)
				if _, err = io.ReadFull(r, buf); err != nil {
					return
				}
				creds = append(creds, string(buf))
				if args, err = r.ReadString('\n'); err != nil {
					return
				}
				args = strings.TrimRight(args, "\r\n")
			}
			if creds[0] == "slow" { // a sign-in that never answers
				<-f.release
				return
			}
			if creds[0] == f.user && creds[1] == f.pass {
				say(tag + " OK logged in")
			} else {
				say(tag + " NO [AUTHENTICATIONFAILED] Invalid credentials")
			}
		case "LIST":
			for _, fo := range f.folders {
				say(fmt.Sprintf(`* LIST (%s) "/" "%s"`, fo.attrs, strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(fo.name)))
			}
			say(tag + " OK LIST done")
		case "EXAMINE":
			selected = -1
			for i, fo := range f.folders {
				if `"`+fo.name+`"` == args {
					selected = i
				}
			}
			if selected < 0 {
				say(tag + " NO no such folder")
				continue
			}
			exists := len(f.folders[selected].msgs)
			if f.folders[selected].exists != 0 {
				exists = f.folders[selected].exists
			}
			say(fmt.Sprintf("* %d EXISTS", exists), "* OK [UIDVALIDITY 7] ok", tag+" OK [READ-ONLY] EXAMINE done")
		case "FETCH":
			var lo, hi int
			fmt.Sscanf(args, "%d:%d", &lo, &hi)
			say("* 1 FETCH (FLAGS (\\Seen))") // unsolicited, without a UID
			for seq := lo; seq <= hi && selected >= 0 && seq <= len(f.folders[selected].msgs); seq++ {
				m := f.folders[selected].msgs[seq-1]
				size := m.size
				if size == 0 {
					size = len(m.raw)
				}
				say(fmt.Sprintf(`* %d FETCH (UID %d RFC822.SIZE %d FLAGS (%s) INTERNALDATE "%s")`, seq, seq*10+3, size, m.flags, m.date))
			}
			say(tag + " OK FETCH done")
		case "UID FETCH":
			uid, _ := strconv.Atoi(strings.Fields(args)[0])
			if selected >= 0 && f.folders[selected].name == "Slow" {
				<-f.release
				return
			}
			if selected >= 0 && f.folders[selected].name == "Flood" {
				// Unsolicited bodies of other messages, without end.
				for {
					if _, err := io.WriteString(w, "* 9 FETCH (UID 999 BODY[] {10000}\r\n"+strings.Repeat("x", 10000)+")\r\n"); err != nil {
						return
					}
				}
			}
			if seq := (uid - 3) / 10; selected >= 0 && seq >= 1 && seq <= len(f.folders[selected].msgs) {
				m := f.folders[selected].msgs[seq-1]
				say(fmt.Sprintf("* %d FETCH (UID %d BODY[] {%d}", seq, uid, len(m.raw)))
				_, _ = io.WriteString(w, m.raw+")\r\n")
			}
			say(tag + " OK FETCH done")
		case "LOGOUT":
			say("* BYE", tag+" OK bye")
			return
		default:
			say(tag + " BAD not here")
		}
	}
}

// The guard resolves once and refuses every internal destination, including
// a name with one internal answer among public ones.
func TestIMAPImportAddressGuard(t *testing.T) {
	defer func(old func(context.Context, string) ([]net.IPAddr, error)) { imapResolve = old }(imapResolve)
	imapResolve = func(_ context.Context, host string) ([]net.IPAddr, error) {
		switch host {
		case "public.example.com":
			return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
		case "private.example.com":
			return []net.IPAddr{{IP: net.ParseIP("10.0.0.5")}}, nil
		case "mixed.example.com":
			return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}, {IP: net.ParseIP("192.168.1.1")}}, nil
		case "mapped.example.com":
			return []net.IPAddr{{IP: net.ParseIP("::ffff:127.0.0.1")}}, nil
		}
		return nil, fmt.Errorf("no such host")
	}
	ctx := context.Background()
	for _, host := range []string{"127.0.0.1", "::1", "10.1.2.3", "169.254.169.254", "::ffff:127.0.0.1", "100.64.0.1", "0.0.0.0", "224.0.0.1", "fd00::1",
		"private.example.com", "mixed.example.com", "mapped.example.com"} {
		if ip, err := imapAddress(ctx, host); err != errIMAPPrivate {
			t.Errorf("%s: got %v %v, want the private refusal", host, ip, err)
		}
	}
	if _, err := imapAddress(ctx, "missing.example.com"); err != errIMAPResolve {
		t.Error("unresolvable", err)
	}
	if ip, err := imapAddress(ctx, "public.example.com"); err != nil || ip.String() != "93.184.216.34" {
		t.Error("public", ip, err)
	}
	for in, want := range map[string]string{"IMAP.Example.com.": "imap.example.com", " 93.184.216.34 ": "93.184.216.34", "::1": "::1"} {
		if got, ok := imapHost(in); !ok || got != want {
			t.Errorf("imapHost(%q) = %q %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "[::1]", "imap.example.com:993", "a..b", "-a.com", "ex ample.com", "fe80::1%eth0", "user@host"} {
		if got, ok := imapHost(bad); ok {
			t.Errorf("imapHost(%q) accepted as %q", bad, got)
		}
	}
}

func TestIMAPImport(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SECRET_DIR", t.TempDir())
	srv := newNativeRuntimeServer(t)
	var logs bytes.Buffer
	logger, err := logging.NewWithOutput(&logs)
	if err != nil {
		t.Fatal(err)
	}
	srv.logger = logger
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "imap-import-one", 1, runtimeDirectoryUser(true)))
	one, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	const password = "long-password-for-import"
	if _, err = srv.users.SetPassword(ctx, one.ID, password, false, nil); err != nil {
		t.Fatal(err)
	}
	admin, err := srv.users.Create(ctx, "imap-import-admin", password, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	member, err := srv.users.Create(ctx, "imap-import-legacy", password, users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	type caller struct{ token, csrf string }
	session := func(id string) caller { tok, csrf := mintSessionForTest(srv, id); return caller{tok, csrf} }
	me, adm, legacy := session(one.ID), session(admin.ID), session(member.ID)
	srv.sessMu.Lock()
	srv.sessions["imap-second"] = Session{UserID: one.ID, IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), CSRFToken: "second-csrf"}
	srv.sessions["imap-sso"] = Session{UserID: one.ID, IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), CSRFToken: "sso-csrf", SSOKySignOn: true}
	srv.sessMu.Unlock()
	meAgain, meSSO := caller{"imap-second", "second-csrf"}, caller{"imap-sso", "sso-csrf"}
	call := func(path string, c caller, body string, header map[string]string) *httptest.ResponseRecorder {
		method := "POST"
		if body == "" {
			method = "GET"
		}
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: c.token})
		req.Header.Set("X-CSRF-Token", c.csrf)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range header {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		return w
	}
	const account = `"host":"imap.example.com","port":993,"security":"tls","username":"alice@example.com",`
	start := func(c caller, fields string) *httptest.ResponseRecorder {
		return call("/api/import/imap", c, `{`+fields+`"password":"`+password+`"}`, nil)
	}
	grant := func(fields string) string {
		t.Helper()
		w := start(me, fields)
		var got struct {
			Token, Target string
			Expires       int `json:"expiresInSeconds"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Token) != 64 || got.Expires != 600 {
			t.Fatalf("grant %s: %d %s", fields, w.Code, w.Body)
		}
		return got.Token
	}
	type listed struct {
		Folders []struct {
			Name       string
			Path       []string
			Attributes []string
		}
		Target string
		Error  string
	}
	list := func(c caller, token, providerPassword string) (*httptest.ResponseRecorder, listed) {
		w := call("/api/import/imap/"+token+"/folders", c, `{"password":"`+providerPassword+`"}`, nil)
		var got listed
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		return w, got
	}
	status := func() importJob {
		t.Helper()
		var st importJob
		if w := call("/api/import", me, "", nil); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &st) != nil {
			t.Fatal("status", w.Code, w.Body)
		}
		return st
	}
	wait := func() importJob {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if st := status(); st.State != "running" {
				return st
			}
		}
		t.Fatal("import did not finish")
		return importJob{}
	}

	seen := "From: a@example.com\r\nSubject: seen\r\nDate: Mon, 1 Jan 2001 00:00:00 +0000\r\n\r\nhello\r\n"
	fake := newFakeIMAP(t, false, "provider-app-password-1", []fakeFolder{
		{name: "INBOX", attrs: `\HasNoChildren`, msgs: []fakeMsg{
			{raw: seen, flags: `\Seen`, date: "06-May-2019 07:08:09 -0700"},
			{raw: "Subject: flagged\r\n\r\nx\r\n", flags: `\Flagged \Answered`, date: " 1-Jan-2020 00:00:00 +0000"},
			{raw: "Subject: too big\r\n\r\nx\r\n", size: int(importMessageBytes) + 1, date: " 1-Jan-2020 00:00:00 +0000"},
			{raw: "no header line\r\n", date: " 1-Jan-2020 00:00:00 +0000"},
		}},
		{name: "[Gmail]", attrs: `\Noselect \HasChildren`},
		{name: "[Gmail]/All Mail", attrs: `\All \HasNoChildren`, msgs: []fakeMsg{{raw: seen, flags: `\Seen`, date: "06-May-2019 07:08:09 -0700"}}},
		{name: "Caf&AOk-.Notes", attrs: ``, msgs: []fakeMsg{{raw: seen, date: "06-May-2019 07:08:09 -0700"}}},
		{name: `Bad\Name`, attrs: ``},
		{name: "Slow", attrs: ``, msgs: []fakeMsg{{raw: seen, date: "06-May-2019 07:08:09 -0700"}}},
		{name: "Lies", msgs: []fakeMsg{{raw: "Subject: lie\r\n\r\n" + strings.Repeat("x", 2<<10), size: 100, date: " 1-Jan-2020 00:00:00 +0000"}}},
		{name: "Flood", msgs: []fakeMsg{{raw: "Subject: flood\r\n\r\nx\r\n", size: 10000, date: " 1-Jan-2020 00:00:00 +0000"}}},
		{name: "Huge", exists: 1000000},
		{name: "Five", exists: 5},
		{name: "Max31", exists: 2147483647},
		{name: "Max32", exists: 4294967295},
		{name: "Wider", exists: 4294967296},
		{name: "Widest", exists: 9223372036854775807},
		{name: strings.Repeat("L", 256)},
	})
	defer func(r func(context.Context, string) ([]net.IPAddr, error), d func(context.Context, string, string) (net.Conn, error), roots *x509.CertPool) {
		imapResolve, imapDial, imapRoots = r, d, roots
	}(imapResolve, imapDial, imapRoots)
	resolved := "93.184.216.34"
	imapResolve = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP(resolved)}}, nil
	}
	var dialedMu sync.Mutex
	var dialed []string
	dialedList := func() []string {
		dialedMu.Lock()
		defer dialedMu.Unlock()
		return append([]string{}, dialed...)
	}
	imapDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialedMu.Lock()
		dialed = append(dialed, addr)
		dialedMu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, fake.ln.Addr().String())
	}
	imapRoots = fake.roots()

	// Refusals before any grant exists.
	for _, c := range []struct {
		name string
		w    *httptest.ResponseRecorder
		want int
	}{
		{"no step-up", call("/api/import/imap", me, `{`+account+`"x":1}`, nil), 401},
		{"no csrf", call("/api/import/imap", caller{me.token, ""}, `{`+account+`"password":"`+password+`"}`, nil), 403},
		{"other port", start(me, `"host":"imap.example.com","port":25,"security":"tls","username":"u",`), 400},
		{"plaintext", start(me, `"host":"imap.example.com","port":143,"security":"none","username":"u",`), 400},
		{"tls on 143", start(me, `"host":"imap.example.com","port":143,"security":"tls","username":"u",`), 400},
		{"bad host", start(me, `"host":"imap.example.com:993","port":993,"security":"tls","username":"u",`), 400},
		{"no username", start(me, `"host":"imap.example.com","port":993,"security":"tls","username":" ",`), 400},
		{"imap account", start(legacy, account), 409},
		{"administrator", start(adm, account), 409},
	} {
		if c.w.Code != c.want {
			t.Errorf("%s: got %d want %d: %s", c.name, c.w.Code, c.want, c.w.Body)
		}
	}
	if len(srv.importGrants) != 0 || len(dialedList()) != 0 {
		t.Fatal("a refused request minted a grant or dialled out")
	}

	// A KySignOn session confirms the request without the provider password,
	// which only the folders request carries and which needs no step-up.
	w := call("/api/import/imap", meSSO, `{`+account+`"mailbox":""}`, nil)
	var challenge struct{ Error, Challenge string }
	if w.Code != 403 || json.Unmarshal(w.Body.Bytes(), &challenge) != nil || challenge.Error != "sso_step_up_required" {
		t.Fatal("KySignOn session skipped step-up", w.Code, w.Body)
	}
	srv.stepUpMu.Lock()
	sc := srv.stepUps[challenge.Challenge]
	sc.verified = true
	srv.stepUps[challenge.Challenge] = sc
	srv.stepUpMu.Unlock()
	if w = call("/api/import/imap", meSSO, `{`+account+`"mailbox":""}`, map[string]string{stepUpHeader: challenge.Challenge}); w.Code != 200 {
		t.Fatal("confirmed KySignOn grant", w.Code, w.Body)
	}
	var ssoGrant struct{ Token string }
	_ = json.Unmarshal(w.Body.Bytes(), &ssoGrant)
	if w, got := list(meSSO, ssoGrant.Token, fake.pass); w.Code != 200 || len(got.Folders) == 0 {
		t.Fatal("KySignOn listing", w.Code, w.Body)
	}

	// Failed sign-ins are the user's budget across grants: neither a fresh
	// step-up nor a correct sign-in resets it, and once spent nothing is
	// dialled.
	var token string
	for i := range imapLoginMaxFailures {
		if i%3 == 0 {
			token = grant(account)
		}
		if i == imapLoginMaxFailures-1 {
			if w, _ := list(me, grant(account), fake.pass); w.Code != 200 {
				t.Fatal("correct sign-in within the budget", w.Code, w.Body)
			}
			token = grant(account)
		}
		if w, got := list(me, token, "wrong-password"); w.Code != 400 || !strings.Contains(got.Error, "app password") {
			t.Fatal("wrong provider password", i, w.Code, w.Body)
		}
	}
	dials := len(dialedList())
	token = grant(account)
	if w, got := list(me, token, fake.pass); w.Code != 429 || w.Header().Get("Retry-After") == "" || !strings.Contains(got.Error, "too many") || len(dialedList()) != dials {
		t.Fatal("sign-in budget reset by a new grant", w.Code, w.Body, len(dialedList())-dials)
	}
	srv.imapLoginLockout = newFailureLockout(imapLoginMaxFailures, imapLoginLockoutFor)

	// Listings dialling out at once are bounded server-wide; a refusal spends
	// no sign-in attempt.
	for range maxIMAPListings {
		srv.imapListSlots <- struct{}{}
	}
	if w, _ := list(me, token, fake.pass); w.Code != 503 || len(dialedList()) != dials {
		t.Fatal("listing past the server-wide slots", w.Code, w.Body)
	}
	for range maxIMAPListings {
		<-srv.imapListSlots
	}

	// A new grant cancels the user's listing in flight, which hands back its
	// slot in time for the new grant's listing even with every other slot taken.
	for range maxIMAPListings - 1 {
		srv.imapListSlots <- struct{}{}
	}
	slowGrant := grant(`"host":"imap.example.com","port":993,"security":"tls","username":"slow",`)
	answered := make(chan int, 1)
	go func() { w, _ := list(me, slowGrant, "x"); answered <- w.Code }()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(fmt.Sprint(fake.recorded()), `LOGIN "slow"`); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("slow sign-in never arrived")
		}
	}
	token = grant(account)
	if w, _ := list(me, token, fake.pass); w.Code != 200 {
		t.Fatal("the cancelled listing kept its slot", w.Code, w.Body)
	}
	for range maxIMAPListings - 1 {
		<-srv.imapListSlots
	}
	select {
	case code := <-answered:
		if code != 404 {
			t.Fatal("cancelled listing answered", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a new grant left the old listing running")
	}

	// A server that never answers holds its slot only until the listing
	// timeout.
	defer func(old time.Duration) { imapListTimeout = old }(imapListTimeout)
	imapListTimeout = time.Second
	began := time.Now()
	if w, _ := list(me, grant(`"host":"imap.example.com","port":993,"security":"tls","username":"slow",`), "x"); w.Code != 504 || time.Since(began) > 10*time.Second || len(srv.imapListSlots) != 0 {
		t.Fatal("a silent server outlived the listing timeout", w.Code, w.Body, time.Since(began), len(srv.imapListSlots))
	}
	imapListTimeout = 45 * time.Second
	token = grant(account)
	srv.imapLoginLockout = newFailureLockout(imapLoginMaxFailures, imapLoginLockoutFor)

	if w = call("/api/import/"+token, me, "From a b\r\nSubject: x\r\n\r\nx\r\n", map[string]string{"Content-Type": "application/octet-stream"}); w.Code != 404 {
		t.Fatal("an IMAP grant was spent as a file upload link", w.Code, w.Body)
	}
	for _, c := range []caller{meAgain, meSSO} {
		if w, _ := list(c, token, fake.pass); w.Code != 404 {
			t.Fatal("grant crossed sessions", c.token, w.Code)
		}
	}
	w, got := list(me, token, fake.pass)
	if w.Code != 200 || got.Target != "Imported/imap-example-com" {
		t.Fatal("listing", w.Code, w.Body)
	}
	heldPassword := func(token string) []byte {
		srv.importMu.Lock()
		defer srv.importMu.Unlock()
		return srv.importGrants[token].imap.password
	}
	wiped := func(b []byte) bool { return len(b) > 0 && bytes.Count(b, []byte{0}) == len(b) }
	jobPassword := heldPassword(token)
	if string(jobPassword) != fake.pass {
		t.Fatal("the grant does not hold the password for the job")
	}
	var names []string
	for _, f := range got.Folders {
		names = append(names, f.Name+"="+strings.Join(f.Path, "|")+"="+strings.Join(f.Attributes, ","))
	}
	if want := []string{`INBOX=INBOX=\hasnochildren`, `[Gmail]/All Mail=[Gmail]|All Mail=\all,\hasnochildren`, "Caf&AOk-.Notes=Café.Notes=", "Slow=Slow=", "Lies=Lies=", "Flood=Flood=", "Huge=Huge=", "Five=Five=", "Max31=Max31=", "Max32=Max32=", "Wider=Wider=", "Widest=Widest="}; fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("folders %q, want %q", names, want)
	}
	for _, addr := range dialedList() {
		if addr != "93.184.216.34:993" {
			t.Fatal("dialled something other than the approved address", addr)
		}
	}

	run := func(token, body string) *httptest.ResponseRecorder {
		return call("/api/import/imap/"+token+"/start", me, body, nil)
	}
	for body, want := range map[string]int{
		`{"folders":[]}`:                            400,
		`{"folders":["Nope"]}`:                      400,
		`{"folders":["[Gmail]"]}`:                   400,
		`{"folders":["INBOX","INBOX"]}`:             400,
		`{"folders":["INBOX"],"target":"a.b"}`:      400,
		`{"folders":["INBOX"],"target":"Imp/../x"}`: 400,
	} {
		if w = run(token, body); w.Code != want {
			t.Errorf("start %s: %d %s", body, w.Code, w.Body)
		}
	}
	if w = call("/api/import/imap/"+token+"/start", meAgain, `{"folders":["INBOX"]}`, nil); w.Code != 404 {
		t.Fatal("start from another session", w.Code)
	}
	if w = run(token, `{"folders":["INBOX","Caf&AOk-.Notes"]}`); w.Code != 202 {
		t.Fatal("start", w.Code, w.Body)
	}
	if len(srv.importGrants) != 0 {
		t.Fatal("the grant and its password outlived the start")
	}
	if w = run(token, `{"folders":["INBOX"]}`); w.Code != 404 {
		t.Fatal("grant reused", w.Code)
	}
	st := wait()
	if !wiped(jobPassword) {
		t.Fatal("the job kept the password after signing in")
	}
	if st.State != "finished" || st.Host != "imap.example.com" || st.Folder != "Imported/imap-example-com" || st.Imported != 3 || st.Skipped != 2 || st.Duplicates != 0 || st.FoldersDone != 2 || st.FoldersTotal != 2 {
		t.Fatalf("import status %+v", st)
	}

	a, err := srv.ssoLifecycle.AdmitNativeMailbox(ctx, srv.stateDir, "https://idp.example", one.ID, one.ID, srv.users)
	if err != nil {
		t.Fatal(err)
	}
	store, err := mailbox.OpenExisting(filepath.Join(a.Dir(srv.stateDir), "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	inbox, err := store.List(ctx, "Imported/imap-example-com/INBOX", 0, 10)
	if err != nil || len(inbox) != 2 {
		t.Fatal("stored", inbox, err)
	}
	// INTERNALDATE, not the Date header; \Seen and \Flagged carried over.
	if m := inbox[1]; m.Subject != "seen" || !m.Seen || m.Starred || m.AtUTC != "2019-05-06T14:08:09Z" {
		t.Fatalf("seen message %+v", m)
	}
	if m := inbox[0]; m.Subject != "flagged" || m.Seen || !m.Starred || m.AtUTC != "2020-01-01T00:00:00Z" {
		t.Fatalf("flagged message %+v", m)
	}
	if raw, err := store.Raw(ctx, "Imported/imap-example-com/INBOX", inbox[1].ID); err != nil || string(raw) != seen {
		t.Fatalf("bytes %q %v", raw, err)
	}
	if notes, err := store.List(ctx, "Imported/imap-example-com/Café_Notes", 0, 10); err != nil || len(notes) != 1 {
		t.Fatal("mapped folder", notes, err)
	}

	// Read-only: nothing that writes, and bodies only through BODY.PEEK; the
	// oversized message was refused by its SIZE and never fetched.
	readOnly := regexp.MustCompile(`^(LOGIN|LIST|EXAMINE|FETCH|LOGOUT) |^UID FETCH \d+ \(BODY\.PEEK\[\]\)$`)
	for _, c := range fake.recorded() {
		if !readOnly.MatchString(c) || strings.HasPrefix(c, "UID FETCH 33 ") {
			t.Fatal("command outside the read-only set, or the oversized message fetched:", c)
		}
	}

	// A re-run skips everything already imported.
	token = grant(account)
	if w, _ = list(me, token, fake.pass); w.Code != 200 {
		t.Fatal("relist", w.Code, w.Body)
	}
	if w = run(token, `{"folders":["INBOX","Caf&AOk-.Notes"]}`); w.Code != 202 {
		t.Fatal("re-run", w.Code, w.Body)
	}
	if st = wait(); st.State != "finished" || st.Imported != 0 || st.Duplicates != 3 || st.Skipped != 2 {
		t.Fatalf("re-run %+v", st)
	}

	// Neither credential reaches the log or the status; the host does.
	statusBody := call("/api/import", me, "", nil).Body.String()
	for _, secret := range []string{fake.user, fake.pass, "wrong-password"} {
		if strings.Contains(logs.String(), secret) || strings.Contains(statusBody, secret) {
			t.Fatalf("%q leaked:\n%s\n%s", secret, logs.String(), statusBody)
		}
	}
	if !strings.Contains(logs.String(), `"host":"imap.example.com"`) || !strings.Contains(logs.String(), `"source":"imap"`) {
		t.Fatal("audit lacks the host", logs.String())
	}

	// STARTTLS on 143, with a password that needs a literal.
	tlsFake := newFakeIMAP(t, true, "pässwörd \"quoted\"", []fakeFolder{{name: "INBOX"}})
	imapDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "93.184.216.34:143" {
			t.Error("dialled", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, tlsFake.ln.Addr().String())
	}
	imapRoots = tlsFake.roots()
	token = grant(`"host":"imap.example.com","port":143,"security":"starttls","username":"alice@example.com",`)
	if w, got = list(me, token, `pässwörd \"quoted\"`); w.Code != 200 || len(got.Folders) != 1 {
		t.Fatal("STARTTLS listing", w.Code, w.Body)
	}
	if cmds := tlsFake.recorded(); len(cmds) == 0 || !strings.HasPrefix(cmds[0], "STARTTLS") {
		t.Fatal("first command was not STARTTLS", cmds)
	}
	replaced := heldPassword(token)

	// An untrusted certificate is refused before any credential is sent.
	imapRoots = x509.NewCertPool()
	before := len(tlsFake.recorded())
	if w, got = list(me, token, "anything"); w.Code != 502 || !strings.Contains(got.Error, "certificate") {
		t.Fatal("untrusted certificate", w.Code, w.Body)
	}
	for _, c := range tlsFake.recorded()[before:] {
		if strings.HasPrefix(c, "LOGIN") {
			t.Fatal("signed in over an unverified connection")
		}
	}
	imapRoots = fake.roots()
	dialedMu.Lock()
	dialed = nil
	dialedMu.Unlock()
	imapDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialedMu.Lock()
		dialed = append(dialed, addr)
		dialedMu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, fake.ln.Addr().String())
	}

	// A name resolving to a private address is refused without dialling.
	resolved = "10.0.0.7"
	token = grant(account)
	if !wiped(replaced) {
		t.Fatal("a replaced grant kept its password")
	}
	if w, got = list(me, token, fake.pass); w.Code != 400 || !strings.Contains(got.Error, "private") || len(dialedList()) != 0 {
		t.Fatal("private address", w.Code, w.Body, dialedList())
	}
	resolved = "93.184.216.34"

	// Incoming encryption on refuses the grant, and the start after it.
	path := srv.userSettingsPath(one.ID)
	setEncryption := func(on bool) {
		t.Helper()
		if err := config.UpdateUserSettings(path, func(s *config.UserSettings) error { s.EncryptIncoming = on; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	setEncryption(true)
	if w = start(me, account); w.Code != 409 || !strings.Contains(w.Body.String(), "incoming encryption") {
		t.Fatal("grant while encryption is on", w.Code, w.Body)
	}
	setEncryption(false)
	token = grant(account)
	if w, _ = list(me, token, fake.pass); w.Code != 200 {
		t.Fatal("list", w.Code)
	}
	setEncryption(true)
	if w = run(token, `{"folders":["INBOX"]}`); w.Code != 409 {
		t.Fatal("start while encryption is on", w.Code, w.Body)
	}
	setEncryption(false)

	// Concurrency: one per user, maxImports overall; a busy refusal keeps the grant.
	for _, busy := range [][]string{{one.ID}, {"someone", "someone-else"}} {
		srv.importMu.Lock()
		saved := srv.imports
		srv.imports = map[string]*importJob{}
		for _, id := range busy {
			srv.imports[id] = &importJob{State: "running"}
		}
		srv.importMu.Unlock()
		if w = run(token, `{"folders":["INBOX"]}`); w.Code != 409 || len(srv.importGrants) != 1 {
			t.Fatal("busy start", busy, w.Code, w.Body)
		}
		if w = start(me, account); w.Code != 409 {
			t.Fatal("busy grant", busy, w.Code, w.Body)
		}
		srv.importMu.Lock()
		srv.imports = saved
		srv.importMu.Unlock()
	}

	// Cancel stops a job waiting on the server.
	if w = run(token, `{"folders":["Slow"]}`); w.Code != 202 {
		t.Fatal("slow start", w.Code, w.Body)
	}
	for deadline := time.Now().Add(10 * time.Second); status().Current != "Imported/imap-example-com/Slow"; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("slow import never started", status())
		}
	}
	if w = call("/api/import/cancel", me, "{}", nil); w.Code != 200 {
		t.Fatal("cancel", w.Code, w.Body)
	}
	if st = wait(); st.State != "cancelled" {
		t.Fatalf("cancelled %+v", st)
	}

	// A message larger than announced, a session past its byte budget and a
	// huge EXISTS each stop the job.
	defer func(old int64) { imapImportBytes = old }(imapImportBytes)
	imapImportBytes = 64 << 10
	// After a small folder, an EXISTS over 31 bits is refused rather than
	// wrapped, and one within 31 bits past the cap is refused before any walk.
	for folder, want := range map[string]string{"Lies": "larger than it announced", "Flood": "sent more than twice", "Huge": "more than 20000 messages",
		`Five","Max31`: "more than 20000 messages", `Five","Max32`: "would not open Imported/imap-example-com/Max32", `Five","Wider`: "would not open Imported/imap-example-com/Wider", `Five","Widest`: "would not open Imported/imap-example-com/Widest"} {
		token = grant(account)
		if w, _ = list(me, token, fake.pass); w.Code != 200 {
			t.Fatal("list", w.Code)
		}
		before := len(fake.recorded())
		if w = run(token, `{"folders":["`+folder+`"]}`); w.Code != 202 {
			t.Fatal(folder, "start", w.Code, w.Body)
		}
		if st = wait(); st.State != "failed" || !strings.Contains(st.Error, want) {
			t.Fatalf("%s %+v", folder, st)
		}
		cmds := fake.recorded()[before:]
		last := 0
		for i, c := range cmds {
			if strings.HasPrefix(c, "EXAMINE") {
				last = i
			}
		}
		if folder != "Lies" && folder != "Flood" && strings.Contains(fmt.Sprint(cmds[last:]), "FETCH") {
			t.Fatal("walked a huge folder")
		}
	}

	// Neither a grant nor a listing while the user's job runs.
	srv.importMu.Lock()
	srv.imports[one.ID] = &importJob{State: "running"}
	srv.importMu.Unlock()
	if w = start(me, account); w.Code != 409 {
		t.Fatal("grant during a job", w.Code, w.Body)
	}
	if w, _ = list(me, token, fake.pass); w.Code != 409 {
		t.Fatal("listing during a job", w.Code, w.Body)
	}
	srv.importMu.Lock()
	srv.imports[one.ID] = &importJob{State: "finished"}
	srv.importMu.Unlock()

	// No job starts once shutdown began.
	token = grant(account)
	if w, _ = list(me, token, fake.pass); w.Code != 200 {
		t.Fatal("list", w.Code)
	}
	srv.cancelImports()
	if w = run(token, `{"folders":["INBOX"]}`); w.Code != 503 {
		t.Fatal("start after shutdown", w.Code, w.Body)
	}
	if w, _ = list(me, token, fake.pass); w.Code != 503 {
		t.Fatal("listing after shutdown", w.Code, w.Body)
	}
	if w = start(me, account); w.Code != 503 {
		t.Fatal("grant after shutdown", w.Code, w.Body)
	}
}
