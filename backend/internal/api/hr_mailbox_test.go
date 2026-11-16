//go:build linux

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestHRMailboxAPIReservesWithoutGrantingAccess(t *testing.T) {
	s := newNativeRuntimeServer(t)
	call := func(key, eventType, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("POST", "/api/sync/hr-mailbox", bytes.NewBufferString(body))
		if key != "" {
			h, err := syncauth.Sign([]byte(key), time.Now().UTC(), eventType, "hr-import-1", []byte(body))
			if err != nil {
				t.Fatal(err)
			}
			h.Apply(r)
		}
		w := httptest.NewRecorder()
		s.handleHRMailbox(w, r)
		return w
	}
	body := `{"userId":"hr-user-one","preferredName":"alice","domain":"example.test"}`
	for _, key := range []string{"", "wrong-secret-that-is-long-enough!!"} {
		if w := call(key, "hr.mailbox.reserve", body); w.Code != 401 {
			t.Fatal("unsigned request admitted", w.Code)
		}
	}
	if w := call(testSyncKey, "user.created", body); w.Code != 401 {
		t.Fatal("wrong signed purpose accepted", w.Code)
	}
	first := call(testSyncKey, "hr.mailbox.reserve", body)
	if first.Code != http.StatusOK {
		t.Fatal(first.Code, first.Body.String())
	}
	var response struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Email != "alice@example.test" {
		t.Fatal(response)
	}
	if _, err := s.users.GetBySSOSubIssuer("https://idp.example", "hr-user-one"); err != users.ErrNotFound {
		t.Fatal("reservation created usable account", err)
	}
	second := call(testSyncKey, "hr.mailbox.reserve", body)
	if second.Code != 200 || second.Body.String() != first.Body.String() {
		t.Fatal("retry changed reservation", second.Body.String())
	}
	resource := scimUser("hr-user-one", response.Email, true)
	resource["emails"] = []map[string]any{{"value": response.Email, "primary": true}}
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "hr-activated", 1, resource))
	u, err := s.users.GetBySSOSubIssuer("https://idp.example", "hr-user-one")
	if err != nil || u.NativeMailboxSource == "" {
		t.Fatal("normal SSO provisioning failed", u, err)
	}
}
