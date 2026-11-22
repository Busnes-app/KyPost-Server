package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

const nativeSignOnMaxAge = 5 * time.Minute

// handleNativeSignOn turns a KyIdentity device-grant ID token, plus proof of
// the app key the token names, into the same single-use pairing deep link the
// password path mints. The route is withTokenAuth; verifyNativeSignOnToken is
// the primitive the route-marker test looks for.
func (s *Server) handleNativeSignOn(w http.ResponseWriter, r *http.Request) {
	claims, settings, ok := s.verifyNativeSignOnToken(w, r)
	if !ok {
		return
	}

	if s.nativeSignOnBeforeAdmit != nil {
		s.nativeSignOnBeforeAdmit()
	}
	// Admission, provisioning and subscriber selection hold the lock
	// ApplyDirectory applies under, so an offboarding lands wholly before (the
	// fence refuses, nothing is provisioned) or after (it finds the account and
	// rotates the subscriber this link is minted for). An unknown subject's
	// disable writes only the fence, so a recheck alone would miss one recorded
	// after it.
	release, err := s.ssoLifecycle.LockDirectory()
	if err != nil {
		s.ssoFailure(w, "lifecycle", err)
		return
	}
	store, subscriberID, ok := s.admitNativeSignOn(w, claims, settings)
	release() // before the response: its SPKI probe may dial out
	if ok {
		s.writePairingResponse(w, store, subscriberID, true)
	}
}

// admitNativeSignOn runs under the directory lock and returns ok=false after
// writing the response.
func (s *Server) admitNativeSignOn(w http.ResponseWriter, claims *sso.SSOTokenClaims, settings sso.SSOSettings) (*state.Store, string, bool) {
	// Same directory and logout fences as the browser callback.
	directory, known, err := s.ssoLifecycle.Directory(settings.IssuerURL, claims.Sub)
	if err != nil {
		s.ssoFailure(w, "lifecycle", err)
		return nil, "", false
	}
	if known && !directory.Active {
		http.Error(w, "Access denied: your account was disabled by the directory.", http.StatusForbidden)
		return nil, "", false
	}
	if known && claims.IssuedAt < directory.RevokedBefore {
		http.Error(w, "Access denied: your directory access changed. Sign in again.", http.StatusForbidden)
		return nil, "", false
	}
	if s.nativeSignOnAfterAdmit != nil {
		s.nativeSignOnAfterAdmit()
	}
	user, err := s.resolveSSOUser(w, settings, claims, false)
	if err != nil {
		return nil, "", false // resolveSSOUser wrote the response
	}
	if !user.Active {
		http.Error(w, "Access denied: your KyPost account is deactivated.", http.StatusForbidden)
		return nil, "", false
	}
	identity := sso.SessionIdentity{
		Issuer:    claims.Issuer,
		ClientID:  settings.ClientID,
		Subject:   claims.Sub,
		SessionID: claims.SessionID,
		IssuedAt:  time.Unix(claims.IssuedAt, 0),
	}
	loggedOut, err := s.ssoLifecycle.LoggedOut(identity)
	if err != nil {
		s.ssoFailure(w, "lifecycle", err)
		return nil, "", false
	}
	if loggedOut {
		http.Error(w, "Access denied: this sign-in was ended by the identity provider. Sign in again.", http.StatusForbidden)
		return nil, "", false
	}
	if s.nativeSignOnBeforeIssue != nil {
		s.nativeSignOnBeforeIssue()
	}
	// Directory offboarding cannot land now; other revocation (password change,
	// admin reset, deactivation) flags the link and rotates the subscriber id under
	// pairingMu, so recheck and read the id under it: a revocation that lands
	// after the unlock rotates the id this token is minted for.
	s.pairingMu.Lock()
	current, err := s.users.Get(user.ID)
	if err != nil || !current.Active || current.SSOSub != claims.Sub || current.SSOLinkRevoked() {
		s.pairingMu.Unlock()
		http.Error(w, "Access denied: your account's sign-in changed. Sign in again.", http.StatusForbidden)
		return nil, "", false
	}
	defer s.pairingMu.Unlock()
	return s.pairingSubscriber(w, user.ID)
}

// verifyNativeSignOnToken authenticates the request and returns ok=false after
// writing the response. Order: SSO and pairing configured, body shape, rate
// limit, signature and claims, device binding, origin, freshness, proof of the
// app key, single use. The jti is spent only once the request is proven valid,
// so a stolen token without the key cannot burn the user's single use; later failures in the handler
// (directory, lookup, 5xx) still spend it.
func (s *Server) verifyNativeSignOnToken(w http.ResponseWriter, r *http.Request) (*sso.SSOTokenClaims, sso.SSOSettings, bool) {
	settings := s.ssoStore.Load()
	if !settings.Enabled || settings.IssuerURL == "" || settings.ClientID == "" {
		http.Error(w, "Single Sign-On is not configured or disabled", http.StatusServiceUnavailable)
		return nil, settings, false
	}
	if s.pairingSecret == "" || s.pairingBaseURL() == "" {
		http.Error(w, "pairing is not configured on the server", http.StatusServiceUnavailable)
		return nil, settings, false
	}
	var req struct {
		IDToken  string `json:"idToken"`
		ProofKey string `json:"proofKey"`
		Proof    string `json:"proof"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&req); err != nil || strings.TrimSpace(req.IDToken) == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return nil, settings, false
	}
	// Charged after the cheap refusals so junk bodies cannot drain the shared
	// login bucket, and before any outbound discovery or JWKS fetch.
	if s.ssoRateLimited(w, r) {
		return nil, settings, false
	}
	provider, _, err := s.ssoProvider(r, settings)
	if err != nil {
		s.ssoFailure(w, "discovery", err)
		return nil, settings, false
	}
	claims, err := provider.VerifyIDToken(r.Context(), req.IDToken)
	if err != nil {
		http.Error(w, "Access denied: the identity token could not be verified.", http.StatusForbidden)
		return nil, settings, false
	}
	if claims.SignOnMethod != "device" || claims.JTI == "" {
		http.Error(w, "Access denied: this token was not issued for device sign-in.", http.StatusForbidden)
		return nil, settings, false
	}
	origin := canonicalOrigin(s.pairingBaseURL())
	if origin == "" || canonicalOrigin(claims.Origin) != origin {
		http.Error(w, "Access denied: this token was issued for a different server.", http.StatusForbidden)
		return nil, settings, false
	}
	age := time.Since(time.Unix(claims.IssuedAt, 0))
	if claims.IssuedAt == 0 || age > nativeSignOnMaxAge {
		http.Error(w, "Access denied: the identity token is too old. Sign in again.", http.StatusForbidden)
		return nil, settings, false
	}
	if age < -30*time.Second {
		http.Error(w, "Access denied: the identity token is not valid yet.", http.StatusForbidden)
		return nil, settings, false
	}
	if claims.Cnf.SPKIS256 == "" {
		http.Error(w, "Update KyAuth and KyIdentity to sign in from this app.", http.StatusForbidden)
		return nil, settings, false
	}
	if !verifySignOnProof(claims.Cnf.SPKIS256, origin, req.IDToken, req.ProofKey, req.Proof) {
		http.Error(w, "Access denied: the sign-in proof does not match.", http.StatusForbidden)
		return nil, settings, false
	}
	fresh, err := s.ssoLifecycle.RecordSignOnJTI(claims.Issuer, settings.ClientID, claims.JTI, time.Now().Add(nativeSignOnMaxAge+time.Minute))
	if err != nil {
		s.ssoFailure(w, "lifecycle", err)
		return nil, settings, false
	}
	if !fresh {
		http.Error(w, "Access denied: this token was already used.", http.StatusForbidden)
		return nil, settings, false
	}
	return claims, settings, true
}

// signOnProofMessage is what the app signs: the version tag, this relay's
// canonical origin and base64url(SHA-256(idToken)), newline-separated.
func signOnProofMessage(origin, idToken string) []byte {
	idt := sha256.Sum256([]byte(idToken))
	return []byte("kypost-signon-pop/1\n" + origin + "\n" + base64.RawURLEncoding.EncodeToString(idt[:]))
}

// verifySignOnProof reports whether proofKey (std base64 P-256 SPKI DER) is the
// key thumb names and proof (std base64 ASN.1 ECDSA) signs the message.
func verifySignOnProof(thumb, origin, idToken, proofKey, proof string) bool {
	der, err := base64.StdEncoding.DecodeString(proofKey)
	if err != nil {
		return false
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return false
	}
	key, ok := pub.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return false
	}
	sum := sha256.Sum256(der)
	if subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(thumb)) != 1 {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(proof)
	if err != nil {
		return false
	}
	digest := sha256.Sum256(signOnProofMessage(origin, idToken))
	return ecdsa.VerifyASN1(key, digest[:], sig)
}

// canonicalOrigin reduces a URL to scheme://host[:port]: host lowercased, an
// explicit :443 dropped for https, path ignored. Unparseable input yields "";
// the caller refuses when this server's own origin is "".
func canonicalOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && (u.Scheme != "https" || port != "443") {
		host += ":" + port
	}
	return strings.ToLower(u.Scheme) + "://" + host
}
