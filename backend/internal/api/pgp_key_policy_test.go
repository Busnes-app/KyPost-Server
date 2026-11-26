package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/contacts"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
)

// TestKeyPolicyAtEveryAPIEntry runs the allowlist set through each way a
// public key enters from the API side: contact JSON, mobile sync, CardDAV and
// vCard (validateContactKey), WKD (validateDiscoveredKey) and the keyserver.
func TestKeyPolicyAtEveryAPIEntry(t *testing.T) {
	allowLoopbackOutboundForTest(t)
	const email = "policy@example.org"
	keys, err := pgpmailtest.PolicyKeys(email)
	if err != nil {
		t.Fatal(err)
	}
	var served string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(served))
	}))
	defer srv.Close()
	old := keyserverBaseURL
	keyserverBaseURL = srv.URL
	defer func() { keyserverBaseURL = old }()

	for _, k := range keys {
		t.Run(k.Name, func(t *testing.T) {
			if err := validateContactKey(k.Armored, []contacts.ContactValue{{Value: email}}); k.Allowed != (err == nil) {
				t.Errorf("contact key: allowed=%v, got %v", k.Allowed, err)
			}
			if _, err := validateDiscoveredKey(k.Armored, email); k.Allowed != (err == nil) {
				t.Errorf("WKD key: allowed=%v, got %v", k.Allowed, err)
			}
			served = k.Armored
			if _, _, _, err := keyserverLookup(context.Background(), email); k.Allowed != (err == nil) {
				t.Errorf("keyserver key: allowed=%v, got %v", k.Allowed, err)
			}
		})
	}
}
