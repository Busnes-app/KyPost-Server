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
	"time"

	"github.com/Busnes-app/ky-primitives/hybridsig"
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

	// Verify deterministic v2 hybridsig companion key + signature over the same vector.
	hk, err := relayIdentityHybridKey(key)
	if err != nil {
		t.Fatalf("relayIdentityHybridKey: %v", err)
	}
	pub, err := hybridsig.ParsePublicKey(hk.Public().Bytes())
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	sigV2, err := hk.Sign(tlsPinStatementContextV2, tlsPinStatementBody(vectorHost, vectorPin, vectorIssuedAt))
	if err != nil {
		t.Fatalf("hk.Sign: %v", err)
	}
	if err := pub.Verify(tlsPinStatementContextV2, tlsPinStatementBody(vectorHost, vectorPin, vectorIssuedAt), sigV2); err != nil {
		t.Fatalf("v2 hybridsig signature failed to verify: %v", err)
	}
	// Wrong context (v1 label) must not verify a v2 signature.
	if err := pub.Verify(tlsPinStatementContext, tlsPinStatementBody(vectorHost, vectorPin, vectorIssuedAt), sigV2); err == nil {
		t.Fatal("v2 signature verified under v1 context")
	}
	// Determinism: a second derivation from the same seed yields the exact same public key
	// and produces a valid signature verifiable by the first public key (ML-DSA-65 signing is hedged).
	hk2, err := relayIdentityHybridKey(key)
	if err != nil {
		t.Fatalf("second relayIdentityHybridKey: %v", err)
	}
	if !bytes.Equal(hk.Public().Bytes(), hk2.Public().Bytes()) {
		t.Fatal("hybrid public key derivation is not deterministic")
	}
	sigV2Again, err := hk2.Sign(tlsPinStatementContextV2, tlsPinStatementBody(vectorHost, vectorPin, vectorIssuedAt))
	if err != nil {
		t.Fatalf("second hk.Sign: %v", err)
	}
	if err := pub.Verify(tlsPinStatementContextV2, tlsPinStatementBody(vectorHost, vectorPin, vectorIssuedAt), sigV2Again); err != nil {
		t.Fatalf("signature from re-derived hybrid key failed verification: %v", err)
	}
	// Tampering with the pin must fail verification.
	if err := pub.Verify(tlsPinStatementContextV2, tlsPinStatementBody(vectorHost, "sha256/tampered", vectorIssuedAt), sigV2); err == nil {
		t.Fatal("v2 signature verified with tampered pin")
	}
}

func TestNativeTLSPinSignsTheServedLeaf(t *testing.T) {
	baseURL, want, roots := tlsOrigin(t)
	srv := newTestServer(t)
	srv.serverBaseURL = baseURL
	srv.pinProbeRoots = roots
	srv.pinProbe = true
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
		Host, Pin, Signature, SignatureV2 string
		IssuedAt                          int64
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
	sigV2, err := base64.StdEncoding.DecodeString(body.SignatureV2)
	if err != nil || len(sigV2) != hybridsig.SignatureBytes {
		t.Fatalf("signatureV2 len=%d err=%v, want %d", len(sigV2), err, hybridsig.SignatureBytes)
	}
	pubHybridRaw, err := base64.StdEncoding.DecodeString(srv.relayIdentityPublicKeyHybrid())
	if err != nil {
		t.Fatalf("decode relayIdentityPublicKeyHybrid: %v", err)
	}
	pubHybrid, err := hybridsig.ParsePublicKey(pubHybridRaw)
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	if err := pubHybrid.Verify(tlsPinStatementContextV2, tlsPinStatementBody(body.Host, body.Pin, body.IssuedAt), sigV2); err != nil {
		t.Fatalf("signatureV2 does not verify over the returned statement: %v", err)
	}
}

func TestNativeTLSPinRefusesWithoutAKeyOrAPin(t *testing.T) {
	baseURL, _, roots := tlsOrigin(t)
	for name, setup := range map[string]func(*Server){
		"no identity key": func(s *Server) { s.serverBaseURL, s.pinProbeRoots, s.relayIdentity = baseURL, roots, nil },
		"no servable pin": func(s *Server) {
			s.serverBaseURL, s.relayIdentity, s.pinProbe = closedHTTPSOrigin(t), vectorKey(), true
		},
		// The probe reaches a host-valid leaf under a trusted CA, but nobody
		// opted into trusting it: nothing is signed.
		"probe not opted in": func(s *Server) { s.serverBaseURL, s.pinProbeRoots, s.relayIdentity = baseURL, roots, vectorKey() },
		"http base url":      func(s *Server) { s.serverBaseURL, s.relayIdentity = "http://relay.example.com", vectorKey() },
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

// TestNativeTLSPinSignsOnlyTheFile: with KYPOST_TLS_CERT_FILE set, the leaf
// the probe is shown — host-valid, under a CA this server trusts — is not what
// gets signed.
func TestNativeTLSPinSignsOnlyTheFile(t *testing.T) {
	baseURL, probedPin, roots := tlsOrigin(t)
	srv := newTestServer(t)
	srv.serverBaseURL, srv.pinProbeRoots, srv.pinProbe = baseURL, roots, true
	srv.pinCertFile = filepath.Join(t.TempDir(), "front.pem")
	want := writeLeafPEM(t, srv.pinCertFile, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), "127.0.0.1")
	srv.relayIdentity = vectorKey()

	rec := httptest.NewRecorder()
	srv.handleNativeTLSPin(rec, httptest.NewRequest(http.MethodGet, "/api/notifications/native/tls-pin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Host, Pin, Signature, SignatureV2 string
		IssuedAt                          int64
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Pin == probedPin || body.Pin != want {
		t.Fatalf("signed pin %q, want the file's %q (probed %q)", body.Pin, want, probedPin)
	}
	sig, _ := base64.StdEncoding.DecodeString(body.Signature)
	if !ed25519.Verify(srv.relayIdentity.Public().(ed25519.PublicKey), tlsPinStatement(body.Host, body.Pin, body.IssuedAt), sig) {
		t.Fatal("signature does not verify over the returned statement")
	}
	sigV2, err := base64.StdEncoding.DecodeString(body.SignatureV2)
	if err != nil {
		t.Fatalf("decode signatureV2: %v", err)
	}
	hk, err := relayIdentityHybridKey(srv.relayIdentity)
	if err != nil {
		t.Fatalf("relayIdentityHybridKey: %v", err)
	}
	if err := hk.Public().Verify(tlsPinStatementContextV2, tlsPinStatementBody(body.Host, body.Pin, body.IssuedAt), sigV2); err != nil {
		t.Fatalf("signatureV2 does not verify over the file-backed statement: %v", err)
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
	if _, ok := srv.withRelayIdentity(map[string]any{})["relayIdentityKeyHybrid"]; ok {
		t.Fatal("no key configured must mean no relayIdentityKeyHybrid field")
	}
	srv.relayIdentity = vectorKey()
	resp := srv.withRelayIdentity(map[string]any{})
	if got := resp["relayIdentityKey"]; got != vectorPublicKey {
		t.Fatalf("relayIdentityKey = %v", got)
	}
	rawHybrid, err := base64.StdEncoding.DecodeString(resp["relayIdentityKeyHybrid"].(string))
	if err != nil {
		t.Fatalf("decode relayIdentityKeyHybrid: %v", err)
	}
	if _, err := hybridsig.ParsePublicKey(rawHybrid); err != nil {
		t.Fatalf("relayIdentityKeyHybrid is not a valid hybridsig public key: %v", err)
	}
}
