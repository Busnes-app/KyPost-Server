package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// accountAddresses is what a client leaves out of Reply All. It must name the
// From the relay actually sends with, never a login that is not an address,
// and every verified alias — including code-verified ones, which authorize From
// but stay out of suggestedUserIDs because they do not prove the domain.
func TestPGPBootstrapAccountAddresses(t *testing.T) {
	cases := []struct {
		name          string
		login         string // "" writes no IMAP config
		wantAccount   []string
		wantSuggested []string
	}{
		{
			name:          "login is an address",
			login:         "Alice@Example.com",
			wantAccount:   []string{"Alice@Example.com", "alice@other.example", "alice@code.example"},
			wantSuggested: []string{"Alice@Example.com", "alice@other.example"},
		},
		{
			// suggestedUserIDs keeps the bare login: it has key-generation
			// semantics and is deliberately left as it was.
			name:          "login is not an address",
			login:         "alice",
			wantAccount:   []string{"alice@example.com", "alice@other.example", "alice@code.example"},
			wantSuggested: []string{"alice", "alice@example.com", "alice@other.example"},
		},
		{
			name:          "no imap config",
			wantAccount:   []string{},
			wantSuggested: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			userID := srv.mustBootstrapUserID(t)
			if tc.login != "" {
				writeUnreachableSMTPIMAPConfig(t, srv, userID, tc.login)
			}
			store, err := srv.userSendAsStore(userID)
			if err != nil {
				t.Fatalf("userSendAsStore: %v", err)
			}
			// DKIM-proven; the first duplicates the login case-insensitively
			// when the login is an address.
			for _, addr := range []string{"alice@example.com", "alice@other.example"} {
				a, err := store.Create(userID, addr, "")
				if err != nil {
					t.Fatalf("Create %s: %v", addr, err)
				}
				if err := store.MarkVerified(a.ID); err != nil {
					t.Fatalf("MarkVerified %s: %v", addr, err)
				}
			}
			// Verified by typed code only: authorizes From, not the domain.
			code, err := store.Create(userID, "alice@code.example", "")
			if err != nil {
				t.Fatalf("Create code alias: %v", err)
			}
			if code, _, err = store.Get(code.ID); err != nil {
				t.Fatalf("Get code alias: %v", err)
			}
			if err := store.Confirm(code.ID, code.VerificationCode); err != nil {
				t.Fatalf("Confirm: %v", err)
			}
			// Pending authorizes nothing and appears in neither list.
			if _, err := store.Create(userID, "alice@pending.example", ""); err != nil {
				t.Fatalf("Create pending alias: %v", err)
			}

			req := httptest.NewRequest(http.MethodGet, "/api/pgp/bootstrap", nil)
			authRequestAs(srv, req, userID)
			rec := httptest.NewRecorder()
			srv.withMailAuth(srv.handlePGPBootstrap)(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
			}
			var got struct {
				AccountAddresses []string `json:"accountAddresses"`
				SuggestedUserIDs []string `json:"suggestedUserIDs"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			// nil here means JSON null, which a client must not have to handle.
			if got.AccountAddresses == nil || !slices.Equal(got.AccountAddresses, tc.wantAccount) {
				t.Errorf("accountAddresses = %#v, want %#v", got.AccountAddresses, tc.wantAccount)
			}
			if !slices.Equal(got.SuggestedUserIDs, tc.wantSuggested) {
				t.Errorf("suggestedUserIDs = %#v, want %#v", got.SuggestedUserIDs, tc.wantSuggested)
			}
		})
	}
}
