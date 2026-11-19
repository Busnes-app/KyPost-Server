//go:build linux

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/mail"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso/ssotest"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func maskedFixture(t *testing.T) (*Server, users.User, func(method, path, body string) *httptest.ResponseRecorder) {
	t.Helper()
	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "masked-one", 1, runtimeDirectoryUser(true)))
	u, err := s.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	return s, u, func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		authRequestAs(s, req, u.ID)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		return w
	}
}

func maskedView(t *testing.T, w *httptest.ResponseRecorder) addressesView {
	t.Helper()
	var v addressesView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatal("addresses view", w.Code, w.Body)
	}
	return v
}

func sendFrom(call func(method, path, body string) *httptest.ResponseRecorder, from string) int {
	return call("POST", "/api/mail/send", `{"to":"visible@outside.test","subject":"s","body":"b","from":"`+from+`"}`).Code
}

var maskedShape = regexp.MustCompile(`^[a-z]+\.[a-z]+\.[1-9][0-9]{3}@example\.test$`)

func TestMaskedAddressLifecycle(t *testing.T) {
	s, u, call := maskedFixture(t)
	ctx := context.Background()
	v := maskedView(t, call("GET", "/api/addresses", ""))
	if !v.PlusAddressing || len(v.MaskedDomains) != 0 || len(v.Masked) != 0 {
		t.Fatalf("defaults %+v", v)
	}
	if w := call("POST", "/api/addresses/masked", `{"domain":"example.test"}`); w.Code != 400 {
		t.Fatal("masked address made before the administrator chose a domain", w.Code, w.Body)
	}
	put := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleMaskedDomains(w, httptest.NewRequest("PUT", "/api/admin/masked-domains", strings.NewReader(body)))
		return w
	}
	if w := put(`{"domains":["elsewhere.test"]}`); w.Code != 400 {
		t.Fatal("unconfigured masked domain accepted", w.Code)
	}
	if w := put(`{"domains":["Example.test"]}`); w.Code != 200 || !strings.Contains(w.Body.String(), `["example.test"]`) {
		t.Fatal("masked domain not saved", w.Code, w.Body)
	}

	w := call("POST", "/api/addresses/masked", `{"domain":"example.test","note":"used for: example-shop"}`)
	var m config.MaskedAddress
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &m) != nil || !maskedShape.MatchString(m.Address) || m.State != "active" || m.Note != "used for: example-shop" || m.ExpiresAt != 0 {
		t.Fatal("masked create", w.Code, w.Body)
	}
	if x, err := s.ssoLifecycle.NativeAddress(m.Address); err != nil || x.Mailbox != u.ID || x.State != "active" {
		t.Fatal("masked address not an active alias of the primary mailbox", x, err)
	}
	// No relay is configured: an admitted From ends in the outbox's 503.
	if code := sendFrom(call, m.Address); code != 503 {
		t.Fatal("live masked From refused", code)
	}

	patch := func(body string) int { return call("PUT", "/api/addresses/masked/"+m.Address, body).Code }
	if code := patch(`{"state":"paused"}`); code != 200 {
		t.Fatal("pause", code)
	}
	if x, _ := s.ssoLifecycle.NativeAddress(m.Address); x.State != "reserved" {
		t.Fatal("paused address still routes", x)
	}
	if code := sendFrom(call, m.Address); code != 403 {
		t.Fatal("paused masked From admitted", code)
	}
	admin := func(method, path, body string) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.SetPathValue("address", m.Address)
		if method == "DELETE" {
			s.handleNativeMailAddressRelease(w, req)
		} else {
			s.handleNativeMailAddressReassign(w, req)
		}
		return w.Code
	}
	if code := admin("POST", "/api/admin/mail-addresses/x/reassign", `{"mailbox":"`+u.ID+`"}`); code != 409 {
		t.Fatal("administrator reassigned a masked address", code)
	}
	if code := patch(`{"state":"active","note":"renamed"}`); code != 200 {
		t.Fatal("resume", code)
	}
	if x, _ := s.ssoLifecycle.NativeAddress(m.Address); x.State != "active" {
		t.Fatal("resumed address not active", x)
	}
	if code := admin("DELETE", "/api/admin/mail-addresses/x", ""); code != 409 {
		t.Fatal("administrator released a masked address", code)
	}
	if w := call("DELETE", "/api/addresses/masked/"+m.Address, ""); w.Code != 200 {
		t.Fatal("delete", w.Code, w.Body)
	}
	if x, _ := s.ssoLifecycle.NativeAddress(m.Address); x.State != "reserved" {
		t.Fatal("deleted address not reserved", x)
	}
	if code := patch(`{"state":"active"}`); code != 409 {
		t.Fatal("deleted address came back", code)
	}
	if _, err := s.ssoLifecycle.AddNativeAlias(ctx, s.stateDir, u.ID, m.Address); err == nil {
		t.Fatal("deleted masked address reissued")
	}

	// Disposable: refused once expired, then closed by the sweep.
	w = call("POST", "/api/addresses/masked", `{"domain":"example.test","expires":"24h"}`)
	var d config.MaskedAddress
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &d) != nil || d.ExpiresAt < time.Now().Add(23*time.Hour).Unix() {
		t.Fatal("disposable create", w.Code, w.Body)
	}
	if err := config.UpdateUserSettings(s.userSettingsPath(u.ID), func(st *config.UserSettings) error {
		st.MaskedAddresses[1].ExpiresAt = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code := sendFrom(call, d.Address); code != 403 {
		t.Fatal("expired disposable From admitted", code)
	}
	if v = maskedView(t, call("GET", "/api/addresses", "")); len(v.Masked) != 2 || v.Masked[0].State != "deleted" || v.Masked[1].State != "deleted" {
		t.Fatalf("listing %+v", v.Masked)
	}
	s.sweepMaskedAddresses(ctx)
	if x, _ := s.ssoLifecycle.NativeAddress(d.Address); x.State != "reserved" {
		t.Fatal("expired disposable address still routes after the sweep", x)
	}

	// Limits: creations in any 24 hours, deleted ones included.
	if err := config.UpdateUserSettings(s.userSettingsPath(u.ID), func(st *config.UserSettings) error {
		for range maskedPerDay {
			st.MaskedAddresses = append(st.MaskedAddresses, config.MaskedAddress{Address: "x@example.test", State: "deleted", CreatedAt: time.Now().Unix()})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w = call("POST", "/api/addresses/masked", `{"domain":"example.test"}`); w.Code != 429 {
		t.Fatal("creation rate not limited", w.Code, w.Body)
	}
}

func TestPlusAddressFromAndSwitch(t *testing.T) {
	_, _, call := maskedFixture(t)
	if code := sendFrom(call, "one+news@example.test"); code != 503 {
		t.Fatal("reply from own plus address refused", code)
	}
	if code := sendFrom(call, "nobody+one@example.test"); code != 403 {
		t.Fatal("plus tag admitted a foreign base", code)
	}
	if w := call("PUT", "/api/addresses/plus", `{"enabled":false}`); w.Code != 200 || maskedView(t, w).PlusAddressing {
		t.Fatal("plus switch", w.Code, w.Body)
	}
	if code := sendFrom(call, "one+news@example.test"); code != 403 {
		t.Fatal("plus From admitted after the owner refused plus addresses", code)
	}
}

// Mail from a masked address carries nothing that names the primary: From,
// the envelope sender (Return-Path), Message-ID domain, no Reply-To or
// Sender, no Autocrypt (it names the key's own address). DKIM carries no i=
// (mailmsg TestDKIMSignatureCarriesNoIdentity).
func TestMaskedSendLeaksNoPrimaryAddress(t *testing.T) {
	s, u, call := maskedFixture(t)
	if err := s.globalStore.SetSetting(maskedDomainsSetting, "example.test"); err != nil {
		t.Fatal(err)
	}
	var m config.MaskedAddress
	if w := call("POST", "/api/addresses/masked", `{"domain":"example.test"}`); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &m) != nil {
		t.Fatal("masked create", w.Code, w.Body)
	}
	if _, err := mailmsg.SaveDomainRelay(context.Background(), filepath.Join(s.configDir, "native-relay.json"), filepath.Join(config.SecretDir(), "native-relay.key"), mailmsg.DomainRelay{Mode: "direct", Domains: []string{"example.test"}, Issuer: "https://idp.example", Host: "mail.example.test"}); err != nil {
		t.Fatal(err)
	}
	w := call("POST", "/api/mail/send", `{"to":"visible@nowhere.invalid","subject":"s","body":"b","from":"`+m.Address+`"}`)
	var response struct{ OutboxID string }
	if json.Unmarshal(w.Body.Bytes(), &response) != nil || response.OutboxID == "" {
		t.Fatal("send did not queue", w.Code, w.Body)
	}
	a, _, err := s.ssoLifecycle.NativeMailboxAssignment(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	box, err := mailbox.OpenExisting(filepath.Join(a.Dir(s.stateDir), "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	key, err := cryptutil.LoadKey(filepath.Join(config.SecretDir(), "native-relay.key"))
	if err != nil {
		t.Fatal(err)
	}
	job, _, _, err := box.ReadOutbound(context.Background(), key, response.OutboxID)
	if err != nil || len(job.Deliveries) != 1 {
		t.Fatal(err)
	}
	if job.From != m.Address {
		t.Fatal("envelope sender is not the masked address", job.From)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(job.Deliveries[0].Raw))
	if err != nil {
		t.Fatal(err)
	}
	if from, err := msg.Header.AddressList("From"); err != nil || len(from) != 1 || from[0].Address != m.Address || from[0].Name != "" {
		t.Fatal("From", from, err)
	}
	if id := msg.Header.Get("Message-Id"); !strings.HasSuffix(id, "@example.test>") {
		t.Fatal("Message-ID domain", id)
	}
	for _, name := range []string{"Reply-To", "Sender", "Return-Path", "Autocrypt", "Dkim-Signature"} {
		if v := msg.Header.Get(name); v != "" {
			t.Fatal(name, "present before delivery", v)
		}
	}
	head, _, _ := bytes.Cut(job.Deliveries[0].Raw, []byte("\r\n\r\n"))
	if bytes.Contains(bytes.ToLower(head), []byte(a.Address)) || a.Address != "one@example.test" {
		t.Fatalf("headers name the primary %s:\n%s", a.Address, head)
	}
}

// A state database that cannot be read is an error, never "no domains": every
// read answers 503 and nothing overwrites the stored list.
func TestMaskedDomainsUnreadableStoreAnswers503(t *testing.T) {
	s, _, call := maskedFixture(t)
	if err := s.globalStore.SetSetting(maskedDomainsSetting, "example.test"); err != nil {
		t.Fatal(err)
	}
	good := s.globalStore
	broken, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := broken.Close(); err != nil {
		t.Fatal(err)
	}
	s.globalStore = broken
	admin := func(method, body string) int {
		w := httptest.NewRecorder()
		s.handleMaskedDomains(w, httptest.NewRequest(method, "/api/admin/masked-domains", strings.NewReader(body)))
		return w.Code
	}
	if code := admin("GET", ""); code != 503 {
		t.Fatal("admin list on an unreadable store", code)
	}
	if code := admin("PUT", `{"domains":[]}`); code != 503 {
		t.Fatal("admin save on an unreadable store", code)
	}
	if w := call("GET", "/api/addresses", ""); w.Code != 503 {
		t.Fatal("user view on an unreadable store", w.Code, w.Body)
	}
	if w := call("POST", "/api/addresses/masked", `{"domain":"example.test"}`); w.Code != 503 {
		t.Fatal("create on an unreadable store", w.Code, w.Body)
	}
	s.globalStore = good
	if v, err := good.Setting(maskedDomainsSetting); err != nil || v != "example.test" {
		t.Fatal("stored list changed", v, err)
	}
}

// two+legacy@ (an alias of one, issued before + was refused) never lets two,
// the owner of two@, send as it, active or not.
func TestLegacyPlusAddressFromKeepsItsOwner(t *testing.T) {
	s, one, _ := maskedFixture(t)
	two := scimUser("native-runtime-two", "runtime-two", true)
	two["emails"] = []map[string]any{{"value": "two@example.test", "primary": true}}
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "legacy-two", 1, two))
	second, err := s.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-two")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO addresses(address, mailbox, kind, state, generation) VALUES ('two+legacy@example.test', '" + one.ID + "', 'alias', 'active', 1)",
		"INSERT INTO address_history(address, generation, mailbox) VALUES ('two+legacy@example.test', 1, '" + one.ID + "')",
	} {
		if err := ssotest.DamageAuthority(s.configDir, q); err != nil {
			t.Fatal(err)
		}
	}
	sendAs := func(userID string) int {
		req := httptest.NewRequest("POST", "/api/mail/send", strings.NewReader(`{"to":"visible@outside.test","subject":"s","body":"b","from":"two+legacy@example.test"}`))
		req.Header.Set("Content-Type", "application/json")
		authRequestAs(s, req, userID)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		return w.Code
	}
	if code := sendAs(second.ID); code != 403 {
		t.Fatal("base owner sent as another mailbox's legacy address", code)
	}
	if code := sendAs(one.ID); code != 503 {
		t.Fatal("owner refused its active legacy address", code)
	}
	if _, err := s.ssoLifecycle.ReleaseNativeAlias(context.Background(), s.stateDir, "two+legacy@example.test"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{one.ID, second.ID} {
		if code := sendAs(id); code != 403 {
			t.Fatal("reserved legacy address sent", id, code)
		}
	}
}
