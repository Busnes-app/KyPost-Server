package sso

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"

	josejson "github.com/go-jose/go-jose/v4/json"
)

// strongJWKSTransport carries only go-oidc's JWKS fetches for the ID-token
// verifier. It refuses the whole set if any RSA key is below 2048 bits;
// discovery, caching, rotation, signatures and claims stay in go-oidc. Logout
// tokens use oidcverify, which applies the same floor itself.
type strongJWKSTransport struct{ base http.RoundTripper }

func (t strongJWKSTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, errors.New("invalid OIDC JWKS")
	}
	for _, rawKey := range set.Keys {
		// go-oidc's envelope is case-insensitive encoding/json; go-jose reads JWK
		// members case-sensitively. Match both so this checks the verifier's modulus.
		var key struct {
			Type string `json:"kty"`
			N    string `json:"n"`
		}
		if err := josejson.Unmarshal(rawKey, &key); err != nil {
			return nil, errors.New("invalid OIDC JWK")
		}
		if key.Type != "RSA" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(key.N)
		if err != nil || new(big.Int).SetBytes(n).BitLen() < 2048 {
			return nil, errors.New("OIDC JWKS RSA keys must be at least 2048 bits")
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp, nil
}
