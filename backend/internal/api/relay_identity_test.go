package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// The cross-language vector: kypost-android's TlsPinRolloverTest verifies this
// exact signature with BouncyCastle; `openssl pkeyutl -sign -rawin` agrees. Ed25519 is deterministic, so a change to
// the statement format on either side breaks one of the two tests.
const (
	vectorHost      = "relay.example.com"
	vectorPin       = "sha256/47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="
	vectorIssuedAt  = int64(1700000000)
	vectorPublicKey = "iojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w="
	vectorSignature = "4Y5/TW0bEWsuVlekoKXVqA0lkRT3MSDzepeAf8Ts+jaogUE7EBwkEyiowiwIP8vcf4zIB2oa2btKzOgumbphAA=="
)

func vectorKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
}

func TestTLSPinStatementVector(t *testing.T) {
	key := vectorKey()
	if got := base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)); got != vectorPublicKey {
		t.Fatalf("public key = %s, want %s", got, vectorPublicKey)
	}
	sig := ed25519.Sign(key, tlsPinStatement(vectorHost, vectorPin, vectorIssuedAt))
	if got := base64.StdEncoding.EncodeToString(sig); got != vectorSignature {
		t.Fatalf("signature = %s, want %s", got, vectorSignature)
	}
}

func TestNativeTLSPinSignsTheServedLeaf(t *testing.T) {
	baseURL, want, roots := tlsOrigin(t)
	srv := newTestServer(t)
	srv.serverBaseURL = baseURL
	srv.pinProbeRoots = roots
	srv.relayIdentity = vectorKey()

	rec := httptest.NewRecorder()
	srv.handleNativeTLSPin(rec, httptest.NewRequest(http.MethodGet, "/api/notifications/native/tls-pin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("a signed pin must not be cached by an intermediary")
	}
	var body struct {
		Host, Pin, Signature string
		IssuedAt             int64
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Pin != want || body.Host != "127.0.0.1" {
		t.Fatalf("got host %q pin %q, want 127.0.0.1 %q", body.Host, body.Pin, want)
	}
	sig, _ := base64.StdEncoding.DecodeString(body.Signature)
	pub := srv.relayIdentity.Public().(ed25519.PublicKey)
	if !ed25519.Verify(pub, tlsPinStatement(body.Host, body.Pin, body.IssuedAt), sig) {
		t.Fatal("signature does not verify over the returned statement")
	}
}

func TestNativeTLSPinRefusesWithoutAKeyOrAPin(t *testing.T) {
	baseURL, _, roots := tlsOrigin(t)
	for name, setup := range map[string]func(*Server){
		"no identity key": func(s *Server) { s.serverBaseURL, s.pinProbeRoots, s.relayIdentity = baseURL, roots, nil },
		"no servable pin": func(s *Server) { s.serverBaseURL, s.relayIdentity = closedHTTPSOrigin(t), vectorKey() },
		"http base url":   func(s *Server) { s.serverBaseURL, s.relayIdentity = "http://relay.example.com", vectorKey() },
	} {
		t.Run(name, func(t *testing.T) {
			srv := newTestServer(t)
			setup(srv)
			rec := httptest.NewRecorder()
			srv.handleNativeTLSPin(rec, httptest.NewRequest(http.MethodGet, "/api/notifications/native/tls-pin", nil))
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", rec.Code)
			}
		})
	}
}

func TestRelayIdentityIsCreatedOnceAndKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay-identity.key")
	first := loadRelayIdentity(path, nil)
	if first == nil {
		t.Fatal("no key created")
	}
	if again := loadRelayIdentity(path, nil); !first.Equal(again) {
		t.Fatal("a second load must return the persisted key, never a new one")
	}
}

func TestWithRelayIdentityOmitsAnAbsentKey(t *testing.T) {
	srv := newTestServer(t)
	srv.relayIdentity = nil
	if _, ok := srv.withRelayIdentity(map[string]any{})["relayIdentityKey"]; ok {
		t.Fatal("no key configured must mean no field, not an empty one")
	}
	srv.relayIdentity = vectorKey()
	if got := srv.withRelayIdentity(map[string]any{})["relayIdentityKey"]; got != vectorPublicKey {
		t.Fatalf("relayIdentityKey = %v", got)
	}
}
