package sso

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
)

// Every verifier KyPost uses: browser exchange, native sign-on and back-channel
// logout. Undersized provider keys must be refused on each.
func TestEveryTokenVerifierRefusesRSABelow2048(t *testing.T) {
	for _, bits := range []int{1024, 2047} {
		key, err := rsa.GenerateKey(rand.Reader, bits)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"exchange", "verify", "logout"} {
			t.Run(path, func(t *testing.T) {
				idp := newIdP(t)
				idp.UseSigningKey(key)
				p, err := NewProvider(context.Background(), testSettings(idp.URL()), "https://mail.example.com/api/auth/oidc/callback")
				if err != nil {
					t.Fatal(err)
				}
				switch path {
				case "exchange":
					var claims *SSOTokenClaims
					claims, err = exchange(t, idp)
					if claims != nil {
						t.Fatalf("RSA-%d ID token accepted", bits)
					}
				case "verify":
					_, err = p.VerifyIDToken(context.Background(), idp.IDToken())
				case "logout":
					_, err = p.VerifyLogout(context.Background(), idp.LogoutToken(t, "sso-sub-12345", "sid-1", nil))
				}
				if err == nil {
					t.Fatalf("RSA-%d %s token accepted", bits, path)
				}
				if path != "logout" && !strings.Contains(err.Error(), "at least 2048 bits") {
					t.Fatalf("RSA-%d %s refused for another reason: %v", bits, path, err)
				}
			})
		}
	}
	// Control: the default 2048-bit provider passes both ID-token paths.
	idp := newIdP(t)
	if claims, err := exchange(t, idp); err != nil || claims == nil {
		t.Fatalf("RSA-2048 exchange refused: %v", err)
	}
}

type staticBody string

func (b staticBody) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(b))), Header: http.Header{}}, nil
}

func TestStrongJWKSTransportReadsKeysLikeTheVerifier(t *testing.T) {
	n := func(bits int) string {
		return base64.RawURLEncoding.EncodeToString(new(big.Int).Lsh(big.NewInt(1), uint(bits-1)).Bytes())
	}
	for _, tc := range []struct {
		name, jwks string
		ok         bool
	}{
		{"weak", `{"keys":[{"kty":"RSA","n":"` + n(2047) + `","e":"AQAB"}]}`, false},
		{"weak under capitalised envelope", `{"Keys":[{"kty":"RSA","n":"` + n(1024) + `","e":"AQAB"}]}`, false},
		{"weak beside strong", `{"keys":[{"kty":"RSA","n":"` + n(2048) + `","e":"AQAB"},{"kty":"RSA","n":"` + n(1024) + `","e":"AQAB"}]}`, false},
		{"malformed modulus", `{"keys":[{"kty":"RSA","n":"!","e":"AQAB"}]}`, false},
		{"not JSON", `nope`, false},
		{"strong", `{"keys":[{"kty":"RSA","n":"` + n(2048) + `","e":"AQAB"}]}`, true},
		{"EC is left to go-jose", `{"keys":[{"kty":"EC","crv":"P-256","x":"AA","y":"AA"}]}`, true},
	} {
		req, _ := http.NewRequest(http.MethodGet, "https://idp.example.test/jwks", nil)
		resp, err := strongJWKSTransport{base: staticBody(tc.jwks)}.RoundTrip(req)
		if (err == nil) != tc.ok {
			t.Fatalf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
		if tc.ok {
			body, _ := io.ReadAll(resp.Body)
			if string(body) != tc.jwks {
				t.Fatalf("%s: body not passed through", tc.name)
			}
		}
	}
}
