//go:build linux

package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func newNativeRuntimeServer(t *testing.T) *Server {
	t.Helper()
	s := newDirectoryTestServer(t)
	s.EnableNativeMail()
	if _, err := s.nativeDomains.Configure(context.Background(), "example.test", "https://idp.example"); err != nil {
		t.Fatal(err)
	}
	s.nativeDomains.SetLookupForTest(func(context.Context, string) ([]string, error) {
		d, err := s.nativeDomains.Read()
		return []string{d.RecordValue()}, err
	})
	return s
}

func runtimeDirectoryUser(active bool) map[string]any {
	u := scimUser("native-runtime-one", "runtime-one", active)
	u["emails"] = []map[string]any{{"value": "one@example.test", "primary": true}}
	return u
}

func TestNativeRuntimeSignedDirectoryToMailbox(t *testing.T) {
	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "runtime-create", 1, runtimeDirectoryUser(true)))
	u, err := s.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil || u.NativeMailboxSource == "" {
		t.Fatalf("native publication: %+v %v", u, err)
	}
	if _, err := os.Stat(s.userIMAPConfigPath(u.ID)); !os.IsNotExist(err) {
		t.Fatal("native publication required IMAP configuration")
	}
	a, _, err := s.nativeMailAssignment(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	store, err := mailbox.OpenExisting(filepath.Join(s.userStateDir(u.ID), "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw := mailmsg.Message{From: "sender@outside.test", To: []string{a.Address}, Subject: "runtime native message", Body: "native runtime body"}.Build()
	id, err := store.Import(context.Background(), mailbox.Receipt{Gateway: "qualified-test", Delivery: "one", Sender: "sender@outside.test", Recipients: []mailbox.Recipient{{Address: a.Address, Generation: 1}}}, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
		authRequestAs(s, r, u.ID)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return w
	}
	path := "/api/mail/body?mailbox=INBOX&messageId=" + strconv.FormatInt(id, 10)
	if w := request("GET", path, ""); w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("native runtime body")) {
		t.Fatalf("native body: %d %s", w.Code, w.Body.String())
	}
	if w := request("GET", "/api/imap/config", ""); w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"native":true`)) {
		t.Fatalf("managed native settings: %d %s", w.Code, w.Body.String())
	}
	if got := s.suggestedKeyUserIDs(u.ID); len(got) != 1 || got[0] != a.Address {
		t.Fatalf("native PGP identity addresses: %v", got)
	}
	held, err := s.userMailClient(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Even a leftover credential file cannot authorize native legacy SMTP.
	if err := writeIMAPConfigPayload(s.userIMAPConfigPath(u.ID), s.imapConfigKeyPath, imapConfigPayload{Host: "127.0.0.1", Username: a.Address, Password: "unused", UpdatedAt: "leftover"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.outboundMailConfig(u.ID); !errors.Is(err, errNativeRelayUnavailable) {
		t.Fatalf("legacy SMTP enabled: %v", err)
	}
	if w := request("POST", "/api/imap/test", `{}`); w.Code != http.StatusForbidden {
		t.Fatalf("native IMAP connection test: %d %s", w.Code, w.Body.String())
	}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		r := httptest.NewRequest(method, "/api/admin/users/"+u.ID+"/imap", bytes.NewBufferString(`{}`))
		r.SetPathValue("id", u.ID)
		w := httptest.NewRecorder()
		s.handleAdminUserIMAPConfig(w, r)
		if w.Code != http.StatusConflict {
			t.Fatalf("native admin IMAP %s: %d %s", method, w.Code, w.Body.String())
		}
	}
	for path, body := range map[string]string{
		"/api/mail/send":    `{"to":"recipient@outside.test","subject":"native","body":"native"}`,
		"/api/pgp/pickup":   `{"recipient":"recipient@outside.test","sealed":"opaque"}`,
		"/api/mail/send-as": `{"email":"alias@outside.test"}`,
	} {
		if w := request("POST", path, body); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("native outbound refusal %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.updated", "runtime-disable", 2, runtimeDirectoryUser(false)))
	if _, err := held.ListOverviews(context.Background(), "INBOX", 10); err == nil {
		t.Fatal("cached pointer retained permission after offboarding")
	}
	if w := request("GET", path, ""); w.Code == 200 {
		t.Fatal("cached HTTP body served after offboarding")
	}
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.updated", "runtime-reactivate", 3, runtimeDirectoryUser(true)))
	restored, err := s.users.Get(u.ID)
	if err != nil || restored.NativeMailboxSource != u.NativeMailboxSource {
		t.Fatal("reactivation replaced namespace")
	}
	if w := request("GET", path, ""); w.Code != 200 {
		t.Fatalf("reactivated mailbox: %d %s", w.Code, w.Body.String())
	}
	if err := s.ssoStore.Save(sso.SSOSettings{Enabled: true, IssuerURL: "https://foreign.example", ClientID: "kypost"}); err != nil {
		t.Fatal(err)
	}
	if _, err := held.ListOverviews(context.Background(), "INBOX", 10); err == nil {
		t.Fatal("cached pointer trusted replaced issuer")
	}
	if w := request("GET", path, ""); w.Code == 200 {
		t.Fatal("cached HTTP body trusted replaced issuer")
	}
	if w := request("GET", "/api/decisions", ""); w.Code == 200 {
		t.Fatal("mail decision history trusted replaced issuer")
	}
}

func TestNativeRuntimeRetriesRetainedDirectoryWithoutJIT(t *testing.T) {
	s := newNativeRuntimeServer(t)
	s.nativeDomains.SetLookupForTest(func(context.Context, string) ([]string, error) { return nil, errors.New("DNS unavailable") })
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "runtime-pending", 1, runtimeDirectoryUser(true)))
	if _, err := s.users.GetBySSOSub("native-runtime-one"); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("pending provision created legacy user: %v", err)
	}
	w := httptest.NewRecorder()
	if _, err := s.resolveSSOUser(w, s.ssoStore.Load(), &sso.SSOTokenClaims{Issuer: "https://idp.example", Sub: "native-runtime-one", PreferredUsername: "runtime-one"}); err == nil || w.Code != 403 {
		t.Fatal("JIT bypassed domain proof")
	}
	s.nativeDomains.SetLookupForTest(func(context.Context, string) ([]string, error) {
		d, err := s.nativeDomains.Read()
		return []string{d.RecordValue()}, err
	})
	if err := s.reconcileNativeSubject(context.Background(), "https://idp.example", "native-runtime-one"); err != nil {
		t.Fatal(err)
	}
	u, err := s.users.GetBySSOSub("native-runtime-one")
	if err != nil || u.NativeMailboxSource == "" {
		t.Fatal("restart repair did not publish retained work")
	}
	if got := directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "runtime-pending", 1, runtimeDirectoryUser(true))); got != "already_applied" {
		t.Fatal(got)
	}
	if _, err := s.users.SetRole(u.ID, users.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.nativeMailAssignment(context.Background(), u.ID); err == nil {
		t.Fatal("local role divergence granted mail")
	}
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.updated", "runtime-role", 2, runtimeDirectoryUser(true)))
	if _, _, err := s.nativeMailAssignment(context.Background(), u.ID); err != nil {
		t.Fatal(err)
	}
}

func TestNativeRuntimeStorageRetryPreservesLocalDeactivation(t *testing.T) {
	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "runtime-initial", 1, runtimeDirectoryUser(true)))
	u, err := s.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	s.nativeDomains.SetLookupForTest(func(context.Context, string) ([]string, error) { return nil, errors.New("DNS unavailable") })
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.updated", "runtime-dns-failure", 2, runtimeDirectoryUser(true)))
	if _, err := s.users.Deactivate(u.ID); err != nil {
		t.Fatal(err)
	}
	_ = s.reconcileNativeSubject(context.Background(), "https://idp.example", "native-runtime-one")
	if current, err := s.users.Get(u.ID); err != nil || current.Active {
		t.Fatalf("pending storage replay undid local deactivation: %+v %v", current, err)
	}
	s.nativeDomains.SetLookupForTest(func(context.Context, string) ([]string, error) {
		d, err := s.nativeDomains.Read()
		return []string{d.RecordValue()}, err
	})
	if err := s.reconcileNativeSubject(context.Background(), "https://idp.example", "native-runtime-one"); err != nil {
		t.Fatal(err)
	}
	if current, err := s.users.Get(u.ID); err != nil || current.Active {
		t.Fatalf("successful storage retry undid local deactivation: %+v %v", current, err)
	}
	if _, _, err := s.nativeMailAssignment(context.Background(), u.ID); err == nil {
		t.Fatal("storage retry reauthorized locally revoked mailbox")
	}
}
