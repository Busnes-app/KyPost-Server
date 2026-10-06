package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

const separationIssuer = "https://idp.example"

// legacyMixedUseAdmin leaves native-runtime-one in the state migration gives
// an administrator that already held a mailbox: created everyday at revision
// 1, promoted at revision 2, then migrated from version 1 (legacyMixedUse).
func legacyMixedUseAdmin(t *testing.T, s *Server, resource map[string]any) {
	t.Helper()
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "legacy-create", 1, resource))
	promoted := runtimeDirectoryUser(true)
	promoted["roles"] = []string{"kypost.admin"}
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.updated", "legacy-promote", 2, promoted))
	key := filepath.Join(t.TempDir(), "native-relay.key")
	if err := sso.WriteNativeV1ForTest(s.configDir, key); err != nil {
		t.Fatal(err)
	}
	if migrated, err := sso.MigrateNative(context.Background(), s.configDir, key); err != nil || !migrated {
		t.Fatal("legacy migration", migrated, err)
	}
}

func adminRuntimeUser(admin bool) map[string]any {
	u := runtimeDirectoryUser(true)
	if admin {
		u["roles"] = []string{"kypost.admin"}
	}
	return u
}

func requestAs(s *Server, userID, method, path string) *httptest.ResponseRecorder {
	return requestBodyAs(s, userID, method, path, "")
}

func requestBodyAs(s *Server, userID, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	authRequestAs(s, r, userID)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	return w
}

// assertAdministratorRefusal is the distinct 403 (never the 401 the SPA
// answers with a reload).
func assertAdministratorRefusal(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"administratorIdentity":true`) {
		t.Fatalf("%s: want administrator 403, got %d %s", what, w.Code, w.Body.String())
	}
}

// assertMailboxless checks the account is an ordinary non-native account with
// no ledger reservation and no mailbox storage, before and after repair.
func assertMailboxless(t *testing.T, s *Server, subject string, role users.Role) users.User {
	t.Helper()
	for range 2 {
		u, err := s.users.GetBySSOSubIssuer(separationIssuer, subject)
		if err != nil || u.Role != role || u.NativeMailboxIssuer != "" || u.NativeMailboxSource != "" {
			t.Fatalf("not a mailbox-less %s account: %+v %v", role, u, err)
		}
		if _, reserved, err := s.ssoLifecycle.NativeAssignment(separationIssuer, subject); err != nil || reserved {
			t.Fatal("mailbox reserved", reserved, err)
		}
		if _, native, err := s.nativeMailAssignment(context.Background(), u.ID); native || err != nil {
			t.Fatal("treated as native", native, err)
		}
		if _, err := os.Stat(filepath.Join(s.userStateDir(u.ID), "mailbox")); !os.IsNotExist(err) {
			t.Fatal("mailbox storage prepared", err)
		}
		if err := s.reconcileNativeSubject(context.Background(), separationIssuer, subject); err != nil {
			t.Fatal("worker repair", err)
		}
	}
	u, _ := s.users.GetBySSOSubIssuer(separationIssuer, subject)
	return u
}

func TestNativeAdministratorGetsMailboxlessAccount(t *testing.T) {
	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "admin-create", 1, adminRuntimeUser(true)))
	u := assertMailboxless(t, s, "native-runtime-one", users.RoleAdmin)
	if w := requestAs(s, u.ID, "GET", "/api/admin/mail-domain"); w.Code != 200 {
		t.Fatalf("mailbox-less administrator cannot administer: %d %s", w.Code, w.Body.String())
	}
	// Demotion keeps the account non-native: an everyday identity is a separate subject.
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.updated", "admin-demote", 2, adminRuntimeUser(false)))
	assertMailboxless(t, s, "native-runtime-one", users.RoleUser)
}

// The webhook creates the account itself, so an administrator can still be
// provisioned while native mail is held after restore and repair cannot run.
func TestNativeAdministratorProvisionedDuringRestoreHold(t *testing.T) {
	s := newNativeRuntimeServer(t)
	if err := os.WriteFile(filepath.Join(s.stateDir, sso.NativeRestoreHoldFile), []byte("hold"), 0600); err != nil {
		t.Fatal(err)
	}
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "held-admin-create", 1, adminRuntimeUser(true)))
	u, err := s.users.GetBySSOSubIssuer(separationIssuer, "native-runtime-one")
	if err != nil || u.Role != users.RoleAdmin || u.NativeMailboxSource != "" {
		t.Fatal("administrator not provisioned under hold", u, err)
	}
}

// A retained administrator resource with no account (applied before this
// rule) is repaired into a mailbox-less account, never allocated.
func TestNativeProvisioningWorkerCreatesMailboxlessAdministrator(t *testing.T) {
	s := newNativeRuntimeServer(t)
	user := adminRuntimeUser(true)
	user["meta"] = map[string]any{"version": `W/"1"`}
	raw, err := json.Marshal(user)
	if err != nil {
		t.Fatal(err)
	}
	var resource sso.DirectoryUser
	if err := json.Unmarshal(raw, &resource); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: "retained-admin", Type: "user.created", At: time.Now()}
	if _, err := s.ssoLifecycle.ApplyDirectoryUser(separationIssuer, ev, resource, sso.EventDigest(ev.Type, raw), func() (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileNativeSubject(context.Background(), separationIssuer, "native-runtime-one"); err != nil {
		t.Fatal(err)
	}
	assertMailboxless(t, s, "native-runtime-one", users.RoleAdmin)
}

func TestNativePromotionRefusesMailUntilDemotion(t *testing.T) {
	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "everyday-create", 1, adminRuntimeUser(false)))
	u, err := s.users.GetBySSOSubIssuer(separationIssuer, "native-runtime-one")
	if err != nil || u.NativeMailboxSource == "" {
		t.Fatal("everyday subject not native", err)
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
	raw := mailmsg.Message{From: "sender@outside.test", To: []string{a.Address}, Subject: "kept", Body: "retained body"}.Build()
	id, err := store.Import(context.Background(), mailbox.Receipt{Gateway: "qualified-test", Delivery: "one", Sender: "sender@outside.test", Recipients: []mailbox.Recipient{{Address: a.Address, Generation: 1}}}, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	deviceID, deviceSecret := pairNativeDevice(t, s, u.ID, "promoted-device")
	viaDevice := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		setDeviceHeaders(r, deviceID, deviceSecret)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return w
	}
	body := fmt.Sprintf("/api/mail/body?mailbox=INBOX&messageId=n1:%s:%s", store.MessageReferenceGeneration(), strconv.FormatInt(id, 10))
	if w := requestAs(s, u.ID, "GET", body); w.Code != 200 {
		t.Fatalf("everyday body: %d %s", w.Code, w.Body.String())
	}
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.updated", "everyday-promote", 2, adminRuntimeUser(true)))
	if _, _, err := s.nativeMailAssignment(context.Background(), u.ID); !errors.Is(err, sso.ErrNativeProvisioning) {
		t.Fatal("promoted subject admitted", err)
	}
	assertAdministratorRefusal(t, "promoted session mail", requestAs(s, u.ID, "GET", body))
	assertAdministratorRefusal(t, "promoted device mail", viaDevice(body))
	assertAdministratorRefusal(t, "promoted decision history", requestAs(s, u.ID, "GET", "/api/decisions"))
	assertAdministratorRefusal(t, "promoted IMAP settings", requestAs(s, u.ID, "GET", "/api/imap/config"))
	// A send whose authority changed after withMailAuth answers the same 403.
	promoted, err := s.users.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.finishNativeSend(w, httptest.NewRequest("POST", "/api/mail/send", nil), AuthContext{UserID: u.ID, NativeSendEpoch: promoted.NativeSendEpoch}, promoted, a.Address, []mailbox.OutboundDelivery{{Recipients: []string{"recipient@outside.test"}, Raw: raw}}, nil, false, nil, 0, "")
	assertAdministratorRefusal(t, "promoted outbound send", w)
	// Handover: the promoted identity can still administer KyPost.
	if w := requestAs(s, u.ID, "GET", "/api/admin/mail-domain"); w.Code != 200 {
		t.Fatalf("promoted administrator cannot administer: %d %s", w.Code, w.Body.String())
	}
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.updated", "everyday-demote", 3, adminRuntimeUser(false)))
	if w := requestAs(s, u.ID, "GET", body); w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("retained body")) {
		t.Fatalf("demoted subject lost retained mail: %d %s", w.Code, w.Body.String())
	}
	if w := viaDevice(body); w.Code != 200 {
		t.Fatalf("demoted device refused: %d %s", w.Code, w.Body.String())
	}
}

const adminIMAPBody = `{"host":"imap.example.test","username":"admin@example.test","password":"test-only"}`

// Native mode keeps administrators off ordinary IMAP mail too, without
// touching configuration stored before (that is the mixed-use migration).
func TestNativeAdministratorIMAPSetupRefused(t *testing.T) {
	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "admin-create", 1, adminRuntimeUser(true)))
	u := assertMailboxless(t, s, "native-runtime-one", users.RoleAdmin)
	if err := writeIMAPConfigPayload(s.userIMAPConfigPath(u.ID), s.imapConfigKeyPath, imapConfigPayload{Host: "imap.example.test", Username: "kept", Password: "kept", UpdatedAt: "earlier"}); err != nil {
		t.Fatal(err)
	}
	assertAdministratorRefusal(t, "IMAP save", requestBodyAs(s, u.ID, "POST", "/api/imap/config", adminIMAPBody))
	assertAdministratorRefusal(t, "IMAP test", requestBodyAs(s, u.ID, "POST", "/api/imap/test", adminIMAPBody))
	r := httptest.NewRequest("PUT", "/api/users/"+u.ID+"/imap-config", strings.NewReader(adminIMAPBody))
	r.SetPathValue("id", u.ID)
	w := httptest.NewRecorder()
	s.handleAdminUserIMAPConfig(w, r)
	assertAdministratorRefusal(t, "admin IMAP assignment", w)
	if w := requestAs(s, u.ID, "GET", "/api/imap/config"); w.Code != 200 || !strings.Contains(w.Body.String(), "kept") {
		t.Fatalf("stored IMAP configuration changed: %d %s", w.Code, w.Body.String())
	}
}

func TestIMAPOnlyAdministratorIMAPSetupUnchanged(t *testing.T) {
	s := newDirectoryTestServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "admin-create", 1, adminRuntimeUser(true)))
	u, err := s.users.GetBySSOSubIssuer(separationIssuer, "native-runtime-one")
	if err != nil || u.Role != users.RoleAdmin {
		t.Fatal(u.Role, err)
	}
	if w := requestBodyAs(s, u.ID, "POST", "/api/imap/config", adminIMAPBody); w.Code != 200 {
		t.Fatalf("IMAP-only administrator setup refused: %d %s", w.Code, w.Body.String())
	}
}

func TestNativeLegacyMixedUseAdministratorKeepsMail(t *testing.T) {
	s := newNativeRuntimeServer(t)
	legacyMixedUseAdmin(t, s, runtimeDirectoryUser(true))
	u, err := s.users.GetBySSOSubIssuer(separationIssuer, "native-runtime-one")
	if err != nil || u.Role != users.RoleAdmin {
		t.Fatal(u.Role, err)
	}
	if _, native, err := s.nativeMailAssignment(context.Background(), u.ID); !native || err != nil {
		t.Fatal("legacy mixed-use administrator refused", native, err)
	}
	if w := requestAs(s, u.ID, "GET", "/api/admin/mail-domain"); w.Code != 200 {
		t.Fatalf("legacy administrator cannot administer: %d", w.Code)
	}
}
