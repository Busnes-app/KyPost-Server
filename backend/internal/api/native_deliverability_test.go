//go:build linux

package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// recordDNS answers from a fixed TXT map; it counts every lookup.
type recordDNS struct {
	mu    sync.Mutex
	txt   map[string][]string
	calls int
}

func (r *recordDNS) count() {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
}
func (r *recordDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	r.count()
	if v, ok := r.txt[name]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}
func (r *recordDNS) LookupIP(context.Context, string, string) ([]net.IP, error) {
	r.count()
	return []net.IP{net.ParseIP("203.0.113.5")}, nil
}
func (r *recordDNS) LookupAddr(context.Context, string) ([]string, error) {
	r.count()
	return []string{"mail.example.test."}, nil
}

type deliverabilityRecord struct {
	Domain, Kind, Selector, RecordName, RecordType, RecordValue string
}

type relayStatus struct {
	Mode        string
	DKIM        []deliverabilityRecord
	Recommended *[]deliverabilityRecord
}

// requireNoDKIMPrivate refuses PEM markers and any 32-character run of a
// stored key's base64 encoding in out.
func requireNoDKIMPrivate(t *testing.T, out []byte, relay mailmsg.DomainRelay) {
	t.Helper()
	if bytes.Contains(out, []byte("PRIVATE KEY")) {
		t.Fatal("PEM private key exposed")
	}
	for _, k := range relay.DKIM {
		encoded := base64.StdEncoding.EncodeToString(k.Private)
		for i := 0; i+32 <= len(encoded); i += 16 {
			if bytes.Contains(out, []byte(encoded[i:i+32])) {
				t.Fatal("DKIM private key material exposed")
			}
		}
	}
}

func captureServerLogs(t *testing.T, s *Server) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	logger, err := logging.NewWithOutput(&out)
	if err != nil {
		t.Fatal(err)
	}
	s.logger = logger
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &out
}

func TestDKIMRecordsAPIDirectKeysAndRelaySwitch(t *testing.T) {
	s := newDirectoryTestServer(t)
	logs := captureServerLogs(t, s)
	ctx := context.Background()
	const password = "dkim-test-admin-password"
	admin, err := s.users.Create(ctx, "dkim-admin", password, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	token, csrf := mintSessionForTest(s, admin.ID)
	issuer := s.ssoStore.Load().IssuerURL
	for _, domain := range []string{"example.test", "second.test"} {
		if _, err := s.nativeDomains.ConfigureDomain(ctx, domain, issuer); err != nil {
			t.Fatal(err)
		}
	}
	proofLookups := 0
	s.nativeDomains.SetLookupForTest(func(_ context.Context, name string) ([]string, error) {
		proofLookups++
		set, err := s.nativeDomains.ReadSet()
		for _, d := range set.Domains {
			if name == d.RecordName()+"." {
				return []string{d.RecordValue()}, err
			}
		}
		return nil, &net.DNSError{IsNotFound: true}
	})
	resolver := &recordDNS{}
	s.dnsResolver = resolver
	call := func(method string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		var reader *strings.Reader
		if body != nil {
			body["authSecret"] = userAuthSecret(s, admin.ID, password)
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			reader = strings.NewReader(string(raw))
		} else {
			reader = strings.NewReader("")
		}
		req := httptest.NewRequest(method, "/api/admin/mail-relay", reader)
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatal(method, w.Code, w.Body)
		}
		return w
	}
	path, keyPath := filepath.Join(s.configDir, "native-relay.json"), filepath.Join(config.SecretDir(), "native-relay.key")
	stored := func() mailmsg.DomainRelay {
		t.Helper()
		c, exists, err := mailmsg.ReadDomainRelay(path, keyPath)
		if err != nil || !exists {
			t.Fatal("profile unreadable", err)
		}
		return c
	}
	direct := map[string]any{"mode": "direct", "host": "mail.example.test", "domains": []string{"example.test"}}
	call("PUT", direct)
	first := stored()
	if len(first.DKIM) != 1 || first.DKIM[0].Domain != "example.test" || !regexp.MustCompile(`^kypost-[0-9a-f]{8}$`).MatchString(first.DKIM[0].Selector) {
		t.Fatal("direct save did not create one key", first.DKIM)
	}
	call("PUT", direct)
	again := stored()
	if again.Generation == first.Generation || !bytes.Equal(again.DKIM[0].Private, first.DKIM[0].Private) || again.DKIM[0].Selector != first.DKIM[0].Selector {
		t.Fatal("saving the same profile changed the key")
	}
	call("PUT", map[string]any{"domains": []string{"example.test", "second.test"}})
	added := stored()
	if added.Generation != again.Generation || len(added.DKIM) != 2 || !bytes.Equal(added.DKIM[0].Private, first.DKIM[0].Private) || added.DKIM[1].Domain != "second.test" {
		t.Fatal("domains-only edit must add only the new domain's key and keep the generation", added.DKIM)
	}
	proofLookups, resolver.calls = 0, 0
	w := call("GET", nil)
	if proofLookups != 0 || resolver.calls != 0 {
		t.Fatal("status read did DNS I/O", proofLookups, resolver.calls)
	}
	var status relayStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.Mode != "direct" || len(status.DKIM) != 2 || status.Recommended == nil || len(*status.Recommended) != 4 {
		t.Fatal("direct status records", w.Body)
	}
	for i, r := range status.DKIM {
		k := added.DKIM[i]
		if r.Domain != k.Domain || r.Selector != k.Selector || r.RecordName != k.Selector+"._domainkey."+k.Domain || r.RecordType != "TXT" || !strings.HasPrefix(r.RecordValue, "v=DKIM1; k=rsa; p=") {
			t.Fatal("DKIM record", r)
		}
	}
	for _, r := range *status.Recommended {
		want := map[string]deliverabilityRecord{
			"spf":   {Domain: r.Domain, Kind: "spf", RecordName: r.Domain, RecordType: "TXT", RecordValue: "v=spf1 a:mail.example.test -all"},
			"dmarc": {Domain: r.Domain, Kind: "dmarc", RecordName: "_dmarc." + r.Domain, RecordType: "TXT", RecordValue: "v=DMARC1; p=quarantine; adkim=s; aspf=s"},
		}[r.Kind]
		if r != want {
			t.Fatal("recommended record", r)
		}
	}
	requireNoDKIMPrivate(t, w.Body.Bytes(), added)
	// Back to a relay: keys stay sealed, nothing is shown.
	w = call("PUT", map[string]any{"host": "smtp.example.test", "smtpUsername": "login", "smtpPassword": "secret"})
	requireNoDKIMPrivate(t, w.Body.Bytes(), added)
	relay := stored()
	if relay.Mode != "" || len(relay.DKIM) != 2 || !bytes.Equal(relay.DKIM[0].Private, first.DKIM[0].Private) {
		t.Fatal("relay save dropped keys")
	}
	w = call("GET", nil)
	status = relayStatus{}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.DKIM == nil || len(status.DKIM) != 0 || status.Recommended != nil {
		t.Fatal("relay status shows DKIM records", w.Body)
	}
	call("PUT", direct)
	if back := stored(); back.DKIM[0].Selector != first.DKIM[0].Selector || !bytes.Equal(back.DKIM[0].Private, first.DKIM[0].Private) {
		t.Fatal("direct -> relay -> direct changed the key")
	}
	requireNoDKIMPrivate(t, logs.Bytes(), added)
}

// snapshotDirs reads every file under dirs, so a read-only check can prove
// it wrote nothing.
func snapshotDirs(t *testing.T, dirs ...string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			raw, err := os.ReadFile(p)
			files[p] = string(raw)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func TestDeliverabilityAPI(t *testing.T) {
	s := newDirectoryTestServer(t)
	logs := captureServerLogs(t, s)
	ctx := context.Background()
	admin, err := s.users.Create(ctx, "check-admin", "check-admin-password", users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.users.Create(ctx, "check-member", "check-member-password", users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := mintSessionForTest(s, admin.ID)
	memberToken, _ := mintSessionForTest(s, member.ID)
	resolver := &recordDNS{txt: map[string][]string{}}
	s.dnsResolver = resolver
	get := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/admin/mail-relay/deliverability", nil)
		if token != "" {
			req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		}
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		return w
	}
	if w := get(""); w.Code != 401 {
		t.Fatal("anonymous check", w.Code)
	}
	if w := get(memberToken); w.Code != 403 {
		t.Fatal("member check", w.Code)
	}
	if w := get(token); w.Code != 409 || !strings.Contains(w.Body.String(), "direct delivery") {
		t.Fatal("check without a profile", w.Code, w.Body)
	}
	path, keyPath := filepath.Join(s.configDir, "native-relay.json"), filepath.Join(config.SecretDir(), "native-relay.key")
	issuer := s.ssoStore.Load().IssuerURL
	if _, err := mailmsg.SaveDomainRelay(ctx, path, keyPath, mailmsg.DomainRelay{Domains: []string{"example.test"}, Issuer: issuer, Host: "smtp.example.test", Port: 465, Username: "u", Password: "p"}); err != nil {
		t.Fatal(err)
	}
	if w := get(token); w.Code != 409 || resolver.calls != 0 {
		t.Fatal("relay profile checked", w.Code, resolver.calls)
	}
	direct, err := mailmsg.SaveDomainRelay(ctx, path, keyPath, mailmsg.DomainRelay{Mode: "direct", Domains: []string{"example.test"}, Issuer: issuer, Host: "mail.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range direct.DKIMRecords() {
		resolver.txt[r.RecordName+"."] = []string{r.RecordValue}
	}
	for _, r := range direct.RecommendedRecords() {
		resolver.txt[r.RecordName+"."] = []string{r.RecordValue}
	}
	before := snapshotDirs(t, s.configDir, s.stateDir, config.SecretDir())
	w := get(token)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var result struct {
		Host    string
		Domains []struct {
			Domain string
			Checks []struct{ Check, Status, Message string }
		}
		PTR            struct{ Check, Status, Message string }
		EgressObserved *bool
		MailSent       *bool
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.EgressObserved == nil || *result.EgressObserved || result.MailSent == nil || *result.MailSent || len(result.Domains) != 1 || len(result.Domains[0].Checks) != 3 {
		t.Fatal("deliverability shape", w.Body)
	}
	for i, c := range append(result.Domains[0].Checks, result.PTR) {
		if c.Check != []string{"dkim", "spf", "dmarc", "ptr"}[i] || c.Status != "pass" || c.Message == "" {
			t.Fatal("published records did not pass", w.Body)
		}
	}
	requireNoDKIMPrivate(t, w.Body.Bytes(), direct)
	calls := resolver.calls
	again := get(token)
	retry, _ := strconv.Atoi(again.Header().Get("Retry-After"))
	if again.Code != 429 || retry < 1 || retry > 10 || resolver.calls != calls {
		t.Fatal("back-to-back check not rate limited", again.Code, again.Header())
	}
	if after := snapshotDirs(t, s.configDir, s.stateDir, config.SecretDir()); len(after) != len(before) {
		t.Fatal("check created files")
	} else {
		for p, v := range before {
			if after[p] != v {
				t.Fatal("check changed", p)
			}
		}
	}
	requireNoDKIMPrivate(t, logs.Bytes(), direct)
}
