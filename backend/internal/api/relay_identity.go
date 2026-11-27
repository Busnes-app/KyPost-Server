// Signed TLS pin rollover for native devices.
//
// A device pins the relay's TLS leaf. A certificate renewal that mints a new
// key fails every pinned call closed, and with 30-day certificates that meant a
// fresh pairing link every month. Instead, the relay holds a long-term Ed25519
// identity key whose public half reaches the device over the pinned channel
// (register, approval poll, refresh). After a renewal the device fetches
// GET /api/notifications/native/tls-pin over plain system trust, sending no
// credential, and accepts the new leaf only if this key signed that exact SPKI
// for this host. An interceptor with a locally trusted CA can present its own
// leaf but cannot produce the signature.

package api

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/ky-primitives/hybridsig"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
)

// tlsPinStatementContext domain-separates the signed bytes. The device builds
// the same string; change both together.
const (
	tlsPinStatementContext   = "kypost-tls-pin/1"
	tlsPinStatementContextV2 = "kypost-tls-pin/2"
	relayHybridSeedInfo      = "kypost-relay-identity-mldsa65/1"
)

// tlsPinStatementBody is the canonical statement body shared by v1 and v2.
func tlsPinStatementBody(host, pin string, issuedAt int64) []byte {
	return []byte(host + "\n" + pin + "\n" + strconv.FormatInt(issuedAt, 10))
}

// tlsPinStatement is the exact byte string signed and verified for v1 (Ed25519).
func tlsPinStatement(host, pin string, issuedAt int64) []byte {
	return []byte(tlsPinStatementContext + "\n" + host + "\n" + pin + "\n" + strconv.FormatInt(issuedAt, 10))
}

// loadRelayIdentity reads or creates the 32-byte seed. nil disables rollover;
// devices then fall back to a pairing link, which is what they did before.
func loadRelayIdentity(path string, logger *logging.Logger) ed25519.PrivateKey {
	seed, err := cryptutil.LoadOrCreateKey(path)
	if err != nil || len(seed) != ed25519.SeedSize {
		if logger != nil {
			logger.Error("could not generate or read the relay identity key; devices will need a pairing link after a certificate renewal")
		}
		return nil
	}
	return ed25519.NewKeyFromSeed(seed)
}

// relayIdentityHybridKey derives the companion ML-DSA-65+Ed25519 key from the
// persisted 32-byte relay identity seed so the Ed25519 half stays identical
// and existing backups cover both keys with no new file on disk.
func relayIdentityHybridKey(edKey ed25519.PrivateKey) (*hybridsig.PrivateKey, error) {
	if len(edKey) != ed25519.PrivateKeySize {
		return nil, hybridsig.ErrLength
	}
	edSeed := edKey.Seed()
	mldsaSeed, err := hkdf.Key(sha256.New, edSeed, nil, relayHybridSeedInfo, hybridsig.SeedBytes)
	if err != nil {
		return nil, err
	}
	return hybridsig.FromSeeds(mldsaSeed, edSeed)
}

// relayIdentityPublicKey is the standard-base64 raw public key, or "".
func (s *Server) relayIdentityPublicKey() string {
	if s.relayIdentity == nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(s.relayIdentity.Public().(ed25519.PublicKey))
}

// relayIdentityPublicKeyHybrid is the standard-base64 hybridsig public key, or "".
func (s *Server) relayIdentityPublicKeyHybrid() string {
	if s.relayIdentity == nil {
		return ""
	}
	hk, err := relayIdentityHybridKey(s.relayIdentity)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(hk.Public().Bytes())
}

// withRelayIdentity adds the public keys to a device-facing reply when a key exists.
func (s *Server) withRelayIdentity(resp map[string]any) map[string]any {
	if key := s.relayIdentityPublicKey(); key != "" {
		resp["relayIdentityKey"] = key
	}
	if key := s.relayIdentityPublicKeyHybrid(); key != "" {
		resp["relayIdentityKeyHybrid"] = key
	}
	return resp
}

// handleNativeTLSPin signs the leaf a device is served right now. Public: the
// statement is only useful to a holder of the public key, and it names the
// relay's own certificate, never the caller's input.
func (s *Server) handleNativeTLSPin(w http.ResponseWriter, r *http.Request) {
	base, err := url.Parse(s.serverBaseURL)
	if s.relayIdentity == nil || err != nil || base.Scheme != "https" || base.Hostname() == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "tls pin rollover is not available"})
		return
	}
	pin := s.pairingSPKIPin()
	if pin == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the serving certificate could not be established"})
		return
	}
	host := strings.ToLower(base.Hostname())
	issuedAt := time.Now().Unix()
	sig := ed25519.Sign(s.relayIdentity, tlsPinStatement(host, pin, issuedAt))
	hk, err := relayIdentityHybridKey(s.relayIdentity)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "tls pin rollover is not available"})
		return
	}
	sigV2, err := hk.Sign(tlsPinStatementContextV2, tlsPinStatementBody(host, pin, issuedAt))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "tls pin rollover is not available"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"host":        host,
		"pin":         pin,
		"issuedAt":    issuedAt,
		"signature":   base64.StdEncoding.EncodeToString(sig),
		"signatureV2": base64.StdEncoding.EncodeToString(sigV2),
	})
}

// tlsPinRateBurst/tlsPinRefillPerSec meter the public tls-pin route per IP.
// Each call may dial SERVER_BASE_URL (pairingSPKIPin); a device needs one call
// per renewal.
const (
	tlsPinRateBurst    = 10
	tlsPinRefillPerSec = 0.2
)

func (s *Server) withTLSPinRateLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.tlsPinLimiter != nil {
			if ok, retryAfter := s.tlsPinLimiter.allow(lockoutKeyForIP(clientIP(r))); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(retryAfter.Seconds())))))
				writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many requests"})
				return
			}
		}
		next(w, r)
	}
}
