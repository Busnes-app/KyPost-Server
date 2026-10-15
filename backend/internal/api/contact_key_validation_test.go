package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ProtonMail/gopenpgp/v3/crypto"

	"github.com/Busnes-app/kypost-server/backend/internal/contacts"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpdiscovery"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func contactCreateStatus(t *testing.T, srv *Server, userID, armored string) *httptest.ResponseRecorder {
	t.Helper()
	payload := map[string]any{
		"fn":     "Bob",
		"emails": []map[string]string{{"value": "bob@example.com"}},
		"pgpKey": armored,
	}
	return doJSONAuth(srv, srv.withAuth(srv.handleContacts), http.MethodPost, "/api/contacts", payload, userID)
}

// A pasted private key block is refused with a 400, not stored and re-served
// through CardDAV, the bootstrap signer keys and pgp-payload.
func TestContactCreateRejectsPrivateKey(t *testing.T) {
	srv := newTestServer(t)
	userID := srv.mustBootstrapUserID(t)
	key, err := crypto.PGP().KeyGeneration().AddUserId("Bob", "bob@example.com").New().GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	priv, err := key.Armor()
	if err != nil {
		t.Fatalf("Armor: %v", err)
	}

	rec := contactCreateStatus(t, srv, userID, priv)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "private key") {
		t.Fatalf("status = %d body = %q, want 400 naming the private key", rec.Code, rec.Body.String())
	}
	store := must1(srv.userContactsStore(userID))
	if list := must1(store.List()); len(list) != 0 {
		t.Fatalf("refused contact was stored: %+v", list)
	}
}

// Same rule as CardDAV: the key must carry one of the contact's addresses.
func TestContactCreateRejectsKeyForAnotherAddress(t *testing.T) {
	srv := newTestServer(t)
	userID := srv.mustBootstrapUserID(t)
	id, err := pgpmail.GenerateIdentity("Mallory", "mallory@evil.example")
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}

	rec := contactCreateStatus(t, srv, userID, id.ArmoredPublicKey)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// A JSON edit that resends the stored key keeps its provenance, and a stored
// key that has since expired does not block the edit.
func TestContactUpdateResendingStoredKeyIsNotRevalidated(t *testing.T) {
	srv := newTestServer(t)
	userID := srv.mustBootstrapUserID(t)
	expired, fp := generateExpiredIdentity(t, "Bob", "bob@example.com")
	store := must1(srv.userContactsStore(userID))
	existing, err := store.Upsert(contacts.Contact{
		FormattedName: "Bob", Emails: []contacts.ContactValue{{Value: "bob@example.com"}},
		PGPKey: expired, PGPKeySource: contacts.PGPSourceManual, PGPKeyVerified: true,
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	body := must1(json.Marshal(map[string]any{
		"fn":     "Bob Renamed",
		"emails": []map[string]string{{"value": "bob@example.com"}},
		"pgpKey": existing.PGPKey,
	}))
	req := httptest.NewRequest(http.MethodPut, "/api/contacts/"+existing.UID, bytes.NewReader(body))
	req.SetPathValue("id", existing.UID)
	authRequestAs(srv, req, userID)
	rec := httptest.NewRecorder()
	srv.withAuth(srv.handleContactByID)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got, _ := must2(store.Get(existing.UID))
	if got.PGPKeyFingerprint != fp || got.PGPKeySource != contacts.PGPSourceManual || !got.PGPKeyVerified {
		t.Fatalf("edit dropped key provenance: %+v", got)
	}
}

// A CardDAV PUT from a client that drops KEY keeps the stored key and pin.
func TestCardDAVPutWithoutKeyKeepsStoredKey(t *testing.T) {
	srv := newTestServer(t)
	u, err := srv.users.Create(context.Background(), "phone", "irrelevant-password", users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ac := AuthContext{UserID: u.ID, Username: u.Username, Role: u.Role}
	id, err := pgpmail.GenerateIdentity("Bob", "bob@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	store := must1(srv.userContactsStore(u.ID))
	if _, err := store.Upsert(contacts.Contact{
		UID: "bob", FormattedName: "Bob", Emails: []contacts.ContactValue{{Value: "bob@example.com"}},
		PGPKey: id.ArmoredPublicKey, PGPKeySource: contacts.PGPSourceManual, PGPKeyVerified: true,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	for _, card := range []string{
		"BEGIN:VCARD\r\nVERSION:4.0\r\nUID:bob\r\nFN:Bob Phone\r\nEMAIL:bob@example.com\r\nEND:VCARD\r\n",
		"BEGIN:VCARD\r\nVERSION:4.0\r\nUID:bob\r\nFN:Bob Phone\r\nEMAIL:bob@example.com\r\nKEY:garbage\r\nEND:VCARD\r\n",
	} {
		req := davAuthedRequest(ac, http.MethodPut, "/dav/"+u.Username+"/contacts/default/bob.vcf", bytes.NewReader([]byte(card)))
		rec := httptest.NewRecorder()
		srv.handleCardDAV(rec, req)
		if rec.Code >= 300 {
			t.Fatalf("PUT status = %d; body=%s", rec.Code, rec.Body.String())
		}
		got, _ := must2(store.Get("bob"))
		if got.FormattedName != "Bob Phone" {
			t.Fatalf("PUT did not land: %+v", got)
		}
		if got.PGPKey == "" || got.PGPKeyFingerprint != id.Fingerprint || !got.PGPKeyVerified {
			t.Fatalf("PUT without a usable KEY wiped the key or pin: key=%t pin=%q", got.PGPKey != "", got.PGPKeyFingerprint)
		}
	}
}

// A stored key its pin disputes (what an Autocrypt rotation now leaves) is a
// key change on the send side, even while that key is usable.
func TestResolveRefusesUsableKeyThatFailsItsPin(t *testing.T) {
	idOld, err := pgpmail.GenerateIdentity("Bob Old", "bob@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	idNew, err := pgpmail.GenerateIdentity("Bob New", "bob@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	store := must1(contacts.New(t.TempDir()))
	if _, err := store.Upsert(contacts.Contact{
		FormattedName: "Bob", Emails: []contacts.ContactValue{{Value: "bob@example.com"}},
		PGPKey: idNew.ArmoredPublicKey, PGPKeyFingerprint: idOld.Fingerprint, PGPKeySource: contacts.PGPSourceAutocrypt,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	resolver := &keyResolver{store: store, settings: pgpdiscovery.Settings{}, discover: false}
	if rk := resolver.resolve(context.Background(), "bob@example.com"); rk.Tier != tierKeyChanged || rk.Usable {
		t.Fatalf("tier = %q usable = %v, want key_changed", rk.Tier, rk.Usable)
	}
	keys := must1(boundSignerKeys(store))
	if len(keys) != 1 || !keys[0].Conflict || keys[0].PublicKey != "" {
		t.Fatalf("read side must flag the conflict without key material: %+v", keys)
	}
}
