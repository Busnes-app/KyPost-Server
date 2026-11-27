package sso

import (
	"bytes"
	"context"
	"crypto/mldsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejson "github.com/go-jose/go-jose/v4/json"
)

// This client belongs only to the upstream OIDC verifier's JWKS fetches. Keep
// discovery, caching, rotation, signatures and claims in coreos/go-oidc.
type strongJWKSTransport struct{ base http.RoundTripper }

func (t strongJWKSTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, errors.New("OIDC JWKS redirects are refused")
	}
	if resp.StatusCode != http.StatusOK {
		return resp, nil
	}
	defer resp.Body.Close()
	const maxJWKSBytes = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxJWKSBytes {
		return nil, errors.New("OIDC JWKS exceeds 1 MiB")
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, errors.New("invalid OIDC JWKS")
	}
	for _, rawKey := range set.Keys {
		// Match go-oidc's envelope and go-jose's case-sensitive JWK members
		// so the floor checks the exact key material the signature verifier uses.
		var key struct {
			Type string `json:"kty"`
			Alg  string `json:"alg"`
			Use  string `json:"use"`
			N    string `json:"n"`
			E    string `json:"e"`
			Pub  string `json:"pub"`
		}
		if err := josejson.Unmarshal(rawKey, &key); err != nil {
			return nil, errors.New("invalid OIDC JWK")
		}
		switch key.Type {
		case "RSA":
			if key.Pub != "" || strings.HasPrefix(key.Alg, "ML-DSA-") {
				return nil, errors.New("invalid OIDC RSA JWK")
			}
			n, err := base64.RawURLEncoding.DecodeString(key.N)
			if err != nil || new(big.Int).SetBytes(n).BitLen() < 2048 {
				return nil, errors.New("OIDC JWKS RSA keys must be at least 2048 bits")
			}
		case "AKP":
			if key.N != "" || key.E != "" {
				return nil, errors.New("OIDC JWKS AKP key must not contain RSA parameters")
			}
			if key.Alg != "ML-DSA-65" {
				return nil, errors.New("OIDC JWKS AKP keys must use ML-DSA-65")
			}
			if key.Use != "" && key.Use != "sig" {
				return nil, errors.New("OIDC JWKS AKP keys must have use sig")
			}
			pub, err := base64.RawURLEncoding.DecodeString(key.Pub)
			if err != nil {
				return nil, errors.New("invalid OIDC AKP public key encoding")
			}
			if _, err := mldsa.NewPublicKey(mldsa.MLDSA65(), pub); err != nil {
				return nil, errors.New("invalid OIDC ML-DSA-65 public key")
			}
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp, nil
}

const maxJWKSCacheTTL = 5 * time.Minute

// remoteJWKSKeySet implements coreos/go-oidc's KeySet interface using go-jose/v4
// directly so RFC 9964 AKP (ML-DSA-65) JWKs are retained alongside classical keys.
// coreos/go-oidc v3.21.0's RemoteKeySet drops JWKs whose "alg" is not in its
// hardcoded classical map before unmarshaling.
type remoteJWKSKeySet struct {
	jwksURL string
	client  *http.Client
	algs    []jose.SignatureAlgorithm

	mu          sync.RWMutex
	cachedKeys  []jose.JSONWebKey
	lastRefresh time.Time
	expiresAt   time.Time
	inflight    *jwksRefreshCall
}

type jwksRefreshCall struct {
	done chan struct{}
	keys []jose.JSONWebKey
	err  error
}

func newRemoteJWKSKeySet(jwksURL string, client *http.Client, algs []string) *remoteJWKSKeySet {
	sigAlgs := make([]jose.SignatureAlgorithm, 0, len(algs))
	for _, alg := range algs {
		sigAlgs = append(sigAlgs, jose.SignatureAlgorithm(alg))
	}
	return &remoteJWKSKeySet{
		jwksURL: jwksURL,
		client:  client,
		algs:    sigAlgs,
	}
}

func (r *remoteJWKSKeySet) VerifySignature(ctx context.Context, rawJWT string) ([]byte, error) {
	jws, err := jose.ParseSignedCompact(rawJWT, r.algs)
	if err != nil {
		return nil, err
	}
	if len(jws.Signatures) != 1 {
		return nil, errors.New("OIDC JWT must have exactly one signature")
	}
	hdr := jws.Signatures[0].Header
	if hdr.Algorithm == "ML-DSA-65" && hdr.KeyID == "" {
		return nil, errors.New("ML-DSA-65 JWT missing kid header")
	}

	keys, fetchedAt, expired := r.snapshot()
	if !expired {
		if payload, ok := verifyWithKeys(jws, hdr, keys); ok {
			return payload, nil
		}
	}

	freshKeys, err := r.refresh(ctx, fetchedAt)
	if err != nil {
		return nil, err
	}
	if payload, ok := verifyWithKeys(jws, hdr, freshKeys); ok {
		return payload, nil
	}
	return nil, errors.New("failed to verify OIDC token signature")
}

func verifyWithKeys(jws *jose.JSONWebSignature, hdr jose.Header, keys []jose.JSONWebKey) ([]byte, bool) {
	for i := range keys {
		key := &keys[i]
		if key.Use != "" && key.Use != "sig" {
			continue
		}
		if hdr.KeyID != "" && key.KeyID != hdr.KeyID {
			continue
		}
		if hdr.Algorithm == "ML-DSA-65" && (key.KeyID == "" || key.Algorithm != "ML-DSA-65") {
			continue
		}
		if key.Algorithm != "" && key.Algorithm != hdr.Algorithm {
			continue
		}
		if payload, err := jws.Verify(key); err == nil {
			return payload, true
		}
	}
	return nil, false
}

func (r *remoteJWKSKeySet) snapshot() ([]jose.JSONWebKey, time.Time, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	expired := r.lastRefresh.IsZero() || time.Now().After(r.expiresAt)
	return r.cachedKeys, r.lastRefresh, expired
}

func (r *remoteJWKSKeySet) refresh(ctx context.Context, prevFetchedAt time.Time) ([]jose.JSONWebKey, error) {
	r.mu.Lock()
	if !r.lastRefresh.IsZero() && r.lastRefresh.After(prevFetchedAt) && time.Now().Before(r.expiresAt) {
		keys := r.cachedKeys
		r.mu.Unlock()
		return keys, nil
	}
	if call := r.inflight; call != nil {
		r.mu.Unlock()
		select {
		case <-call.done:
			return call.keys, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &jwksRefreshCall{done: make(chan struct{})}
	r.inflight = call
	r.mu.Unlock()

	go func() {
		timeout := r.client.Timeout
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		fetchCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		keys, err := r.fetchKeys(fetchCtx)

		r.mu.Lock()
		if err == nil {
			now := time.Now()
			r.cachedKeys = keys
			r.lastRefresh = now
			r.expiresAt = now.Add(maxJWKSCacheTTL)
		}
		r.inflight = nil
		call.keys = keys
		call.err = err
		close(call.done)
		r.mu.Unlock()
	}()

	select {
	case <-call.done:
		return call.keys, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *remoteJWKSKeySet) fetchKeys(ctx context.Context) ([]jose.JSONWebKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("OIDC JWKS endpoint returned non-200 status")
	}
	const maxJWKSBytes = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxJWKSBytes {
		return nil, errors.New("OIDC JWKS exceeds 1 MiB")
	}

	var rawSet struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &rawSet); err != nil {
		return nil, errors.New("invalid OIDC JWKS")
	}
	keys := make([]jose.JSONWebKey, 0, len(rawSet.Keys))
	for _, rawKey := range rawSet.Keys {
		var meta struct {
			Alg string `json:"alg"`
		}
		if err := josejson.Unmarshal(rawKey, &meta); err != nil {
			return nil, errors.New("invalid OIDC JWK")
		}
		if meta.Alg != "" && !r.allowsAlg(meta.Alg) {
			continue
		}
		var jwk jose.JSONWebKey
		if err := json.Unmarshal(rawKey, &jwk); err != nil {
			if errors.Is(err, jose.ErrUnsupportedKeyType) {
				continue
			}
			return nil, err
		}
		keys = append(keys, jwk)
	}
	return keys, nil
}

func (s *remoteJWKSKeySet) allowsAlg(alg string) bool {
	target := jose.SignatureAlgorithm(alg)
	for _, allowed := range s.algs {
		if allowed == target {
			return true
		}
	}
	return false
}
