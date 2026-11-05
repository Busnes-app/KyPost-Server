package api

import (
	"bytes"
	"encoding/json"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	openpgp "github.com/ProtonMail/go-crypto/openpgp/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/contacts"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
	"github.com/ProtonMail/gopenpgp/v3/crypto"
)

func replayKeySync(t *testing.T, srv *Server, device, secret string, changes any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"changes": changes})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/contacts/sync", bytes.NewReader(body))
	setDeviceHeaders(req, device, secret)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func TestContactsSyncReplayOmitsOnlyUnusableKeys(t *testing.T) {
	valid := must1(pgpmailtest.GenerateIdentity("Bob", "bob@example.com"))
	wrong := must1(pgpmailtest.GenerateIdentity("Mallory", "mallory@example.com"))
	expired, _ := generateExpiredIdentity(t, "Bob", "bob@example.com")
	revoked := generateRevokedArmoredKey(t, "Bob", "bob@example.com")
	private := must1(crypto.PGP().KeyGeneration().AddUserId("Bob", "bob@example.com").New().GenerateKey())
	for _, tc := range []struct {
		name, key string
		valid     bool
	}{
		{"valid", valid.ArmoredPublicKey, true},
		{"older-expired-newer-valid", replayMultiUIDKey(t, true, false, false), true},
		{"older-valid-newer-expired", replayMultiUIDKey(t, false, true, false), false},
		{"primary-older-valid-newer-expired", replayMultiUIDKey(t, false, true, true), true},
		{"valid-v6", replayV6Key(t), true}, {"expired", expired, false},
		{"revoked", revoked, false}, {"wrong-address", wrong.ArmoredPublicKey, false},
		{"private", must1(private.Armor()), false}, {"malformed", "garbage", false},
	} {
		if got := validateContactKey(tc.key, []contacts.ContactValue{{Value: "bob@example.com"}}) == nil; got != tc.valid {
			t.Fatalf("fixture%s usable%v want%v", tc.name, got, tc.valid)
		}
		for _, omit := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "-strict", true: "-replay"}[omit], func(t *testing.T) {
				srv := newTestServer(t)
				uid := srv.mustBootstrapUserID(t)
				device, secret := pairNativeDevice(t, srv, uid, "replay-device")
				changes := []map[string]any{{"uid": "replayed", "fn": "Bob", "emails": []map[string]string{{"value": "bob@example.com"}}, "pgpKey": tc.key, "omitUnusableKey": omit}}
				rec := replayKeySync(t, srv, device, secret, changes)
				want := http.StatusOK
				if !tc.valid && !omit {
					want = http.StatusBadRequest
				}
				if rec.Code != want {
					t.Fatalf("status%d want%d: %s", rec.Code, want, rec.Body.String())
				}
				store := must1(srv.userContactsStore(uid))
				got, found := must2(store.Get("replayed"))
				if want == http.StatusBadRequest {
					if found {
						t.Fatal("refused key committed a contact")
					}
					return
				}
				if !found || got.FormattedName != "Bob" {
					t.Fatal("contact missing")
				}
				if tc.valid {
					if got.PGPKey == "" || validateContactKey(got.PGPKey, got.Emails) != nil {
						t.Fatal("valid key lost")
					}
				} else if got.PGPKey != "" {
					t.Fatal("unusable key retained")
				}
				// Replaying the same admitted request remains one contact.
				if again := replayKeySync(t, srv, device, secret, changes); again.Code != http.StatusOK {
					t.Fatalf("retry%d: %s", again.Code, again.Body.String())
				}
				if rows := must1(store.List()); len(rows) != 1 {
					t.Fatalf("retry duplicated contact: %d", len(rows))
				}
			})
		}
	}
}

func TestContactsSyncReplayKeepsUnchangedStoredKeysAndStrictBatchRollback(t *testing.T) {
	srv := newTestServer(t)
	uid := srv.mustBootstrapUserID(t)
	device, secret := pairNativeDevice(t, srv, uid, "replay-controls")
	store := must1(srv.userContactsStore(uid))
	expired, _ := generateExpiredIdentity(t, "Bob", "bob@example.com")
	seeded := must1(store.Upsert(contacts.Contact{UID: "existing", FormattedName: "Bob", Emails: []contacts.ContactValue{{Value: "bob@example.com"}}, PGPKey: expired, PGPKeySource: contacts.PGPSourceManual, PGPKeyVerified: true}))
	changes := []map[string]any{{"uid": "existing", "fn": "Renamed", "emails": []map[string]string{{"value": "bob@example.com"}}, "pgpKey": seeded.PGPKey, "omitUnusableKey": true}}
	if rec := replayKeySync(t, srv, device, secret, changes); rec.Code != http.StatusOK {
		t.Fatalf("unchanged%d: %s", rec.Code, rec.Body.String())
	}
	got, _ := must2(store.Get("existing"))
	if got.PGPKey != seeded.PGPKey || got.PGPKeyFingerprint != seeded.PGPKeyFingerprint || !got.PGPKeyVerified {
		t.Fatal("unchanged expired key/provenance lost")
	}
	changes = []map[string]any{{"uid": "marked", "fn": "One", "pgpKey": "bad", "omitUnusableKey": true}, {"uid": "strict", "fn": "Two", "pgpKey": "bad"}}
	if rec := replayKeySync(t, srv, device, secret, changes); rec.Code != http.StatusBadRequest {
		t.Fatalf("strict batch%d", rec.Code)
	}
	if rows := must1(store.List()); len(rows) != 1 {
		t.Fatalf("refusal partially committed: %d", len(rows))
	}
	changes = []map[string]any{{"uid": "bad-flag", "fn": "One", "pgpKey": "bad", "omitUnusableKey": "true"}}
	if rec := replayKeySync(t, srv, device, secret, changes); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed flag%d", rec.Code)
	}
}

// These two opposite cases caused the previous Android-only validator to be reverted.
// The contact's address belongs to the older/secondary UID, but the newer UID controls expiry.
func replayMultiUIDKey(t *testing.T, olderExpired, newerExpired, preferOlder bool) string {
	t.Helper()
	older := time.Now().Add(-72 * time.Hour)
	e, err := openpgp.NewEntity("Older", "", "bob@example.com", &packet.Config{RSABits: 2048, Time: func() time.Time { return older }})
	if err != nil {
		t.Fatal(err)
	}
	newer := older.Add(48 * time.Hour)
	if err = e.AddUserId("Newer", "", "other@example.com", &packet.Config{Time: func() time.Time { return newer }}); err != nil {
		t.Fatal(err)
	}
	for _, id := range e.Identities {
		sig := id.SelfCertifications[0].Packet
		preferred := preferOlder && id.UserId.Email == "bob@example.com"
		sig.IsPrimaryId = &preferred
		expired := olderExpired
		if id.UserId.Email != "bob@example.com" {
			expired = newerExpired
		}
		if expired {
			seconds := uint32(3600)
			sig.SigLifetimeSecs = &seconds
		} else {
			sig.SigLifetimeSecs = nil
		}
		if err = sig.SignUserId(id.Name, e.PrimaryKey, e.PrivateKey, &packet.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	return replayPublicArmor(t, e)
}
func replayV6Key(t *testing.T) string {
	t.Helper()
	e, err := openpgp.NewEntity("Bob", "", "bob@example.com", &packet.Config{V6Keys: true, RSABits: 2048})
	if err != nil {
		t.Fatal(err)
	}
	if e.PrimaryKey.Version != 6 {
		t.Fatal("not a v6 fixture")
	}
	return replayPublicArmor(t, e)
}
func replayPublicArmor(t *testing.T, e *openpgp.Entity) string {
	t.Helper()
	var b bytes.Buffer
	w, err := armor.Encode(&b, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestContactsSyncReplayDoesNotSuppressOtherFailuresOrWebValidation(t *testing.T) {
	srv := newTestServer(t)
	uid := srv.mustBootstrapUserID(t)
	device, secret := pairNativeDevice(t, srv, uid, "replay-failure-controls")
	store := must1(srv.userContactsStore(uid))
	if _, err := store.Upsert(contacts.Contact{UID: "keep", FormattedName: "Unchanged"}); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"fn": "Rejected", "pgpKey": "bad", "omitUnusableKey": true}
	if rec := doJSONAuth(srv, srv.withAuth(srv.handleContacts), http.MethodPost, "/api/contacts", payload, uid); rec.Code != http.StatusBadRequest {
		t.Fatalf("web validation%d", rec.Code)
	}
	path := filepath.Join(srv.userStateDir(uid), "contacts.json")
	before := must1(os.ReadFile(path))
	corruptContactsFile(t, srv, uid)
	changes := []map[string]any{{"uid": "new", "fn": "New", "pgpKey": "bad", "omitUnusableKey": true}}
	if rec := replayKeySync(t, srv, device, secret, changes); rec.Code != http.StatusInternalServerError {
		t.Fatalf("read failure%d: %s", rec.Code, rec.Body.String())
	}
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	if rows := must1(store.List()); len(rows) != 1 || rows[0].UID != "keep" {
		t.Fatal("failed import changed contacts")
	}
}
