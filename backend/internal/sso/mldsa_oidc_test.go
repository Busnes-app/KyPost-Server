package sso

import (
	"context"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

func TestProviderVerifiesMLDSA65IDAndLogoutTokens(t *testing.T) {
	mlKey, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatalf("generate ML-DSA-65 key: %v", err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	const (
		mlKID  = "mldsa-post-1"
		rsaKID = "rsa-post-1"
	)

	var issuer string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                issuer + "/authorize",
			"token_endpoint":                        issuer + "/token",
			"jwks_uri":                              issuer + "/jwks",
			"id_token_signing_alg_values_supported": []string{"ML-DSA-65", "RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{
			Keys: []jose.JSONWebKey{
				{Key: mlKey.PublicKey(), KeyID: mlKID, Algorithm: string(jose.MLDSA65), Use: "sig"},
				{Key: &rsaKey.PublicKey, KeyID: rsaKID, Algorithm: string(jose.RS256), Use: "sig"},
			},
		})
	})

	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	issuer = srv.URL

	prev := SetTransport(srv.Client().Transport)
	defer SetTransport(prev)

	cfg := testSettings(issuer)
	cfg.AllowInsecureIssuer = false
	provider, err := NewProvider(context.Background(), cfg, "https://mail.example.com/api/auth/oidc/callback")
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	now := time.Now().UTC()
	sign := func(typ string, alg jose.SignatureAlgorithm, key any, kid string, claims map[string]any) string {
		t.Helper()
		payload, err := json.Marshal(claims)
		if err != nil {
			t.Fatalf("marshal claims: %v", err)
		}
		signer, err := jose.NewSigner(
			jose.SigningKey{Algorithm: alg, Key: key},
			(&jose.SignerOptions{}).WithType(jose.ContentType(typ)).WithHeader("kid", kid),
		)
		if err != nil {
			t.Fatalf("new signer %s: %v", alg, err)
		}
		jws, err := signer.Sign(payload)
		if err != nil {
			t.Fatalf("sign %s: %v", alg, err)
		}
		tok, err := jws.CompactSerialize()
		if err != nil {
			t.Fatalf("compact serialize %s: %v", alg, err)
		}
		return tok
	}

	idClaims := map[string]any{
		"iss":   issuer,
		"sub":   "post-user-1",
		"aud":   cfg.ClientID,
		"exp":   now.Add(10 * time.Minute).Unix(),
		"iat":   now.Unix(),
		"email": "user@example.com",
	}

	mlIDToken := sign("JWT", jose.MLDSA65, mlKey, mlKID, idClaims)
	got, err := provider.VerifyIDToken(context.Background(), mlIDToken)
	if err != nil {
		t.Fatalf("VerifyIDToken(ML-DSA-65): %v", err)
	}
	if got.Sub != "post-user-1" {
		t.Fatalf("sub = %q, want post-user-1", got.Sub)
	}

	rsaIDToken := sign("JWT", jose.RS256, rsaKey, rsaKID, idClaims)
	if _, err := provider.VerifyIDToken(context.Background(), rsaIDToken); err != nil {
		t.Fatalf("VerifyIDToken(RS256): %v", err)
	}

	// Back-channel logout token signed with ML-DSA-65 via ky-primitives/oidcverify v0.12.0.
	logoutClaims := map[string]any{
		"iss": issuer,
		"sub": "post-user-1",
		"aud": cfg.ClientID,
		"iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
		"jti": "logout-jti-mldsa-1",
		"sid": "sid-mldsa-1",
		"events": map[string]any{
			"http://schemas.openid.net/event/backchannel-logout": map[string]any{},
		},
	}
	mlLogoutToken := sign("logout+jwt", jose.MLDSA65, mlKey, mlKID, logoutClaims)
	ev, err := provider.VerifyLogout(context.Background(), mlLogoutToken)
	if err != nil {
		t.Fatalf("VerifyLogout(ML-DSA-65): %v", err)
	}
	if ev.Subject != "post-user-1" || ev.SessionID != "sid-mldsa-1" {
		t.Fatalf("logout event = %+v, want sub=post-user-1 sid=sid-mldsa-1", ev)
	}

	// Tampered ML-DSA-65 ID token and cross-alg kid confusion must fail.
	tampered := mlIDToken[:len(mlIDToken)-4] + "AAAA"
	if tampered == mlIDToken {
		tampered = mlIDToken[:len(mlIDToken)-4] + "BBBB"
	}
	if _, err := provider.VerifyIDToken(context.Background(), tampered); err == nil {
		t.Fatal("tampered ML-DSA-65 ID token unexpectedly verified")
	}
	confused := sign("JWT", jose.RS256, rsaKey, mlKID, idClaims)
	if _, err := provider.VerifyIDToken(context.Background(), confused); err == nil {
		t.Fatal("cross-algorithm kid-confused token unexpectedly verified")
	}
}

func TestStrongJWKSTransportRejectsMalformedMLDSA65(t *testing.T) {
	badPub := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	validLenPub := base64.RawURLEncoding.EncodeToString(make([]byte, 1952))
	cases := []struct {
		name string
		body string
	}{
		{
			name: "unsupported AKP alg",
			body: `{"keys":[{"kty":"AKP","alg":"ML-DSA-44","kid":"k1","pub":"` + validLenPub + `"}]}`,
		},
		{
			name: "non-sig use",
			body: `{"keys":[{"kty":"AKP","alg":"ML-DSA-65","use":"enc","kid":"k1","pub":"` + validLenPub + `"}]}`,
		},
		{
			name: "AKP with RSA params",
			body: `{"keys":[{"kty":"AKP","alg":"ML-DSA-65","kid":"k1","n":"AAAA","pub":"` + validLenPub + `"}]}`,
		},
		{
			name: "wrong pub length",
			body: `{"keys":[{"kty":"AKP","alg":"ML-DSA-65","kid":"k1","pub":"` + badPub + `"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, "https://idp.example.test/jwks", nil)
			if _, err := (strongJWKSTransport{base: staticBody(tc.body)}).RoundTrip(req); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}
