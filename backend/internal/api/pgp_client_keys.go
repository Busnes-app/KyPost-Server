package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Busness-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busness-app/kypost-server/backend/internal/users"
)

// maxWrappedKeyBytes bounds the opaque client-wrapped envelope. A wrapped
// Curve25519 private key is a couple of KB; an imported RSA-4096 key with
// many User IDs is still well under this. The server never parses the blob,
// so a cap is the only validation available to it.
const maxWrappedKeyBytes = 128 << 10

// handlePGPWrappedKey returns the caller's own wrapped private key envelope
// so their browser can unwrap it locally.
//
// Serving this to the authenticated owner is not a disclosure: the envelope
// is sealed under a key derived from their password, which the server does
// not have and cannot derive (it stores only a scrypt hash). An attacker
// with a stolen session gets a blob they still have to crack the password to
// open — which is the security property the whole mode exists to provide.
func (s *Server) handlePGPWrappedKey(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		http.Error(w, "user unavailable", http.StatusInternalServerError)
		return
	}
	if u.PGPFingerprint == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no pgp identity configured"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"protection":  u.PGPProtection(),
		"wrapped":     u.PGPPrivateKeyWrapped,
		"fingerprint": u.PGPFingerprint,
		"pgpRevision": u.PGPRevision,
		"keyring":     u.PGPKeyring,
		"keyId":       u.PGPKeyID,
		"publicKey":   u.PGPPublicKey,
	})
}

type clientIdentityRequest struct {
	ExpectedRevision *uint64 `json:"expectedRevision,omitempty"`
	PublicKey        string  `json:"publicKey"`
	Wrapped          string  `json:"wrapped"`
	Source           string  `json:"source"`

	// Step-up credential, required only when this REPLACES an existing identity
	// (pgp_stepup.go). Both forms are accepted because a client-protected
	// account never sends its password to this server — see login_params.go.
	Password   string `json:"password,omitempty"`
	AuthSecret string `json:"authSecret,omitempty"`
}

// handlePGPIdentityClient stores an identity whose private half the browser
// generated (or imported) and wrapped itself. The server receives the public
// key and an opaque envelope, and gains no ability to read the user's mail.
//
// The fingerprint and key ID are derived from the uploaded public key here,
// never taken from the request: a client that could assert an arbitrary
// fingerprint could get its own key published under someone else's identity
// through WKD or Autocrypt.
func (s *Server) handlePGPIdentityClient(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	var req clientIdentityRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxWrappedKeyBytes)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	// Replacing an EXISTING identity needs the account credential, not just the
	// session: the new public key outlives the session, gets published through
	// WKD and Autocrypt, and redirects every future correspondent to it. First
	// -time setup is not gated — see pgp_stepup.go.
	if !s.requirePGPStepUp(w, r, ac.UserID, req.Password, req.AuthSecret) {
		return
	}
	wrapped := strings.TrimSpace(req.Wrapped)
	if wrapped == "" {
		http.Error(w, "wrapped private key is required", http.StatusBadRequest)
		return
	}
	info, err := pgpmail.InspectPublicKey(req.PublicKey)
	if err != nil {
		http.Error(w, "invalid public key: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Refuse a key that is already unusable, so a user cannot end up with an
	// identity that silently fails on first send.
	if status, serr := pgpmail.CheckKeyStatus(info.ArmoredPublicKey); serr == nil && !status.Usable() {
		http.Error(w, "this key is revoked or expired", http.StatusBadRequest)
		return
	}

	source := strings.TrimSpace(req.Source)
	if source != "imported" {
		source = "generated"
	}
	u, err := s.users.SetPGPIdentityClientProtected(
		ac.UserID, info.Fingerprint, info.KeyID, info.ArmoredPublicKey,
		wrapped, source, time.Now().UTC().Format(time.RFC3339), req.ExpectedRevision)
	if err != nil {
		writeUserStoreError(w, err)
		return
	}
	// SetPGPIdentityClientProtected clears PGPWrappedEnvelopes, including any
	// device: slot — but ONLY when the fingerprint actually changed (see the
	// guard added in 3e50a0e). Re-submitting an unchanged identity leaves the
	// slots in place, so do not read this call as "the envelopes are already
	// gone": it is unconditional precisely because the other store's marker
	// must not be left claiming an enrollment the browser may have just
	// invalidated. This is the custody mode device enrollment exists for, so it
	// is the path where a stale "enrolled" marker is most likely to be believed.
	//
	// The two stores are therefore not guaranteed to agree after this call, and
	// that asymmetry is safe only because the marker gates nothing:
	// handlePGPDeviceEnvelope iterates WrappedEnvelopes() directly and never
	// reads it. If that ever changes, reconcile the clear conditions first.
	s.clearDeviceEnrollmentsFor(u.ID, "client-protected identity "+source)
	s.logger.Info("pgp identity stored with client-side protection",
		"user_id", u.ID, "fingerprint", u.PGPFingerprint, "source", source)
	// The identity shape, not users.Public: this endpoint returns the identity
	// it just stored, and the browser keeps that response as the page's current
	// one. Public names the same fields differently (pgpFingerprint) and carries
	// no public key, so answering with it handed the client an identity whose
	// fingerprint was undefined — which is only noticed later, by whatever first
	// reads it (see handleDownloadRecoveryBackup).
	writeJSON(w, http.StatusOK, pgpIdentityResponse{
		Fingerprint: u.PGPFingerprint,
		PGPRevision: u.PGPRevision,
		Keyring:     u.PGPKeyring,
		KeyID:       u.PGPKeyID,
		PublicKey:   u.PGPPublicKey,
		Source:      source,
		CreatedAt:   u.PGPKeyCreatedAt,
		// Both false by construction: the guard above refuses a key that is
		// already revoked or expired, so a stored one never is.
	})
}

// handlePGPRewrapKey repairs password sealing using locally recovered material.
// Password changes commit credentials and their envelope through /auth/password.
func (s *Server) handlePGPRewrapKey(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	var req struct {
		Wrapped             string  `json:"wrapped"`
		ExpectedFingerprint string  `json:"expectedFingerprint,omitempty"`
		ExpectedRevision    *uint64 `json:"expectedRevision,omitempty"`
		KeyringVersion      *int    `json:"keyringVersion,omitempty"`
		// Step-up credential. Always required here: rewrapping presupposes an
		// identity to rewrap, so this endpoint is never first-time setup.
		Password   string `json:"password,omitempty"`
		AuthSecret string `json:"authSecret,omitempty"`
	}
	// JSON quoting and step-up fields add overhead; the store caps the envelope itself.
	if err := json.NewDecoder(io.LimitReader(r.Body, 3*maxWrappedKeyBytes)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Wrapped) == "" {
		http.Error(w, "wrapped private key is required", http.StatusBadRequest)
		return
	}
	// Bind the revision before step-up, which independently reloads the credential.
	snapshot, err := s.users.Get(ac.UserID)
	if err != nil {
		writeUserStoreError(w, err)
		return
	}
	// A stolen session could otherwise overwrite the envelope with a blob whose
	// contents nobody can check — the server cannot open it by design — leaving
	// the owner locked out of every message ever encrypted to them.
	if !s.requirePGPStepUp(w, r, ac.UserID, req.Password, req.AuthSecret) {
		return
	}
	var u users.User
	if req.KeyringVersion != nil {
		if *req.KeyringVersion != 1 || req.ExpectedRevision == nil || strings.TrimSpace(req.ExpectedFingerprint) == "" {
			http.Error(w, "keyringVersion 1 requires an expected revision and fingerprint", http.StatusBadRequest)
			return
		}
		if *req.ExpectedRevision != snapshot.PGPRevision {
			writeUserStoreError(w, users.ErrPGPRevisionChanged)
			return
		}
		u, err = s.users.RewrapPGPKeyring(ac.UserID, strings.TrimSpace(req.Wrapped), strings.TrimSpace(req.ExpectedFingerprint), req.ExpectedRevision)
	} else {
		u, err = s.users.RewrapPGPPrivateKey(ac.UserID, strings.TrimSpace(req.Wrapped), strings.TrimSpace(req.ExpectedFingerprint), req.ExpectedRevision)
	}
	if err != nil {
		writeUserStoreError(w, err)
		return
	}
	s.logger.Info("pgp private key rewrapped", "user_id", ac.UserID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pgpRevision": u.PGPRevision})
}

// handlePGPExportLegacyKey hands a legacy server-sealed private key back to
// its owner exactly once per call, so their browser can rewrap it under a
// key the server does not hold and then POST it to
// /api/pgp/identity/client — after which the server-readable copy is gone.
//
// This is the only endpoint in the codebase that returns a private key, and
// it is gated on a fresh password re-entry rather than the session alone. A
// session cookie is a bearer token that could have been stolen; the password
// is the same secret that protects the key everywhere else, so requiring it
// here means this endpoint cannot be used to obtain something the caller
// could not already obtain. It refuses outright once the account is already
// client-protected, so it can never be used to downgrade.
func (s *Server) handlePGPExportLegacyKey(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	var req struct {
		Password string `json:"password"`
		// AuthSecret is the client-derived credential, for an account whose
		// password never reaches this server. See login_params.go.
		AuthSecret string `json:"authSecret,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	// Shared with the PGP step-up gates (pgp_stepup.go): same throttle, same
	// two credential forms, same answers. This endpoint set the standard the
	// others now meet, and one implementation of it is one place for it to be
	// got right.
	if !s.confirmAccountCredential(w, r, ac.UserID, req.Password, req.AuthSecret) {
		return
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		http.Error(w, "user unavailable", http.StatusInternalServerError)
		return
	}

	if u.PGPProtection() == users.PGPProtectionClient {
		http.Error(w, "this key is already client-protected", http.StatusConflict)
		return
	}
	if !u.HasServerReadableKey() {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no legacy pgp key to migrate"})
		return
	}
	identity, err := pgpmail.OpenPrivateKey(u.PGPPrivateKeyEnc, s.pgpPrivateKeyPath)
	if err != nil {
		http.Error(w, "failed to load pgp identity", http.StatusInternalServerError)
		return
	}
	armored, err := identity.ExportArmoredPrivateKey()
	if err != nil {
		http.Error(w, "failed to export pgp identity", http.StatusInternalServerError)
		return
	}
	s.logger.Info("legacy pgp key exported for client-side rewrap", "user_id", ac.UserID)
	writeJSON(w, http.StatusOK, map[string]any{
		"privateKey":  armored,
		"publicKey":   identity.ArmoredPublicKey,
		"pgpRevision": u.PGPRevision,
		"keyring":     u.PGPKeyring,
	})
}

// Envelope slots are the additional sealings of a client-protected private key
// — a recovery code today, enrolled devices later. See
// docs/superpowers/specs/2026-08-04-multi-wrapped-key-custody-design.md.
//
// All three are withAuth (session only), never withMailAuth. A paired device
// must not be able to mint a sealing of the account key: that is the property
// the passphrase-only tier is enforced by, and enforcing it at the route is the
// only place the server can enforce it at all.
//
// PUT and DELETE additionally require requirePGPStepUp (pgp_stepup.go), the
// same standard as identity/client, rewrap, and DELETE /api/pgp/identity:
// installing a slot plants an envelope the server cannot validate, and
// deleting one destroys a sealing that cannot be re-minted without the
// unwrapped key. Neither is undoable, so a session alone is not enough.
//
// GET is unchanged, but NOT for the reason this comment used to give. It said
// GET "serves the same bytes" as GET /api/pgp/identity/wrapped, which is true
// only of the synthesised password slot; once a recovery or device slot exists
// those are different bytes, and /wrapped never serves them. The real reason is
// that every slot's key-encryption key is at least as strong as the password
// one: the design seals `recovery` under a 128-bit random secret and `device:*`
// under a non-extractable secure-element ECDH key, so a session that can already
// fetch the password envelope gains no weaker offline target by fetching these.

func (s *Server) handlePGPPutEnvelopeSlot(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	var req struct {
		Envelope            string  `json:"envelope"`
		ExpectedFingerprint string  `json:"expectedFingerprint,omitempty"`
		ExpectedRevision    *uint64 `json:"expectedRevision,omitempty"`
		KeyringVersion      *int    `json:"keyringVersion,omitempty"`
		// Device slots only: the device key the envelope was sealed to, and
		// the material generation it was prepared from. See putDeviceEnvelope.
		EnrollmentPublicKey string  `json:"enrollmentPublicKey,omitempty"`
		MaterialGeneration  *uint64 `json:"materialGeneration,omitempty"`
		// Step-up credential (pgp_stepup.go). Installing a slot mints or
		// replaces a sealing of the private key: a stolen session must not be
		// able to plant an envelope the server cannot validate, and the user
		// would only discover it was bogus when they actually needed it.
		Password   string `json:"password,omitempty"`
		AuthSecret string `json:"authSecret,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 3*maxWrappedKeyBytes)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	envelope := strings.TrimSpace(req.Envelope)
	if envelope == "" {
		http.Error(w, "envelope is required", http.StatusBadRequest)
		return
	}
	snapshot, err := s.users.Get(ac.UserID)
	if err != nil {
		writeUserStoreError(w, err)
		return
	}
	if !s.requirePGPStepUp(w, r, ac.UserID, req.Password, req.AuthSecret) {
		return
	}
	if deviceID, isDevice := strings.CutPrefix(r.PathValue("slot"), users.EnvelopeSlotDevicePrefix); isDevice {
		if req.KeyringVersion != nil {
			http.Error(w, "keyringVersion 1 requires the recovery slot, revision and fingerprint", http.StatusBadRequest)
			return
		}
		// A stale snapshot is refused before the device is even looked up;
		// the store checks again under its lock.
		if req.ExpectedRevision != nil && *req.ExpectedRevision != snapshot.PGPRevision {
			writeUserStoreError(w, users.ErrPGPRevisionChanged)
			return
		}
		s.putDeviceEnvelope(w, r, ac.UserID, deviceID, envelope, req.EnrollmentPublicKey,
			strings.TrimSpace(req.ExpectedFingerprint), req.MaterialGeneration, req.ExpectedRevision)
		return
	}
	var u users.User
	if req.KeyringVersion != nil {
		if *req.KeyringVersion != 1 || r.PathValue("slot") != users.EnvelopeSlotRecovery || req.ExpectedRevision == nil || strings.TrimSpace(req.ExpectedFingerprint) == "" {
			http.Error(w, "keyringVersion 1 requires the recovery slot, revision and fingerprint", http.StatusBadRequest)
			return
		}
		if *req.ExpectedRevision != snapshot.PGPRevision {
			writeUserStoreError(w, users.ErrPGPRevisionChanged)
			return
		}
		u, err = s.users.SetPGPKeyringRecovery(ac.UserID, envelope, time.Now().UTC().Format(time.RFC3339), strings.TrimSpace(req.ExpectedFingerprint), req.ExpectedRevision)
	} else {
		u, err = s.users.SetPGPWrappedEnvelope(ac.UserID, r.PathValue("slot"), envelope, time.Now().UTC().Format(time.RFC3339), strings.TrimSpace(req.ExpectedFingerprint), req.ExpectedRevision)
	}
	if err != nil {
		writeUserStoreError(w, err)
		return
	}
	s.logger.Info("pgp envelope slot stored", "user_id", ac.UserID, "slot", r.PathValue("slot"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pgpRevision": u.PGPRevision})
}

func (s *Server) handlePGPGetEnvelopeSlot(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		http.Error(w, "user unavailable", http.StatusInternalServerError)
		return
	}
	slot := r.PathValue("slot")
	for _, e := range u.WrappedEnvelopes() {
		if e.Slot == slot {
			writeJSON(w, http.StatusOK, map[string]any{
				"slot": e.Slot, "envelope": e.Envelope,
				"fingerprint": u.PGPFingerprint,
				"pgpRevision": u.PGPRevision,
				"keyring":     u.PGPKeyring, "publicKey": u.PGPPublicKey,
			})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"error": "no envelope in that slot"})
}

func (s *Server) handlePGPDeleteEnvelopeSlot(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	var req struct {
		ExpectedRevision *uint64 `json:"expectedRevision,omitempty"`
		// Step-up credential (pgp_stepup.go). Deleting a sealing that cannot
		// be re-minted without the unwrapped key is not undoable, the same
		// reasoning as DELETE /api/pgp/identity — so a caller must prove the
		// account credential, not just present a session cookie. Decoding the
		// body is mandatory: a request with no body at all fails to decode
		// and is refused (400) rather than silently treated as "no
		// credential needed."
		Password   string `json:"password,omitempty"`
		AuthSecret string `json:"authSecret,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxWrappedKeyBytes)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if !s.requirePGPStepUp(w, r, ac.UserID, req.Password, req.AuthSecret) {
		return
	}
	u, err := s.users.DeletePGPWrappedEnvelope(ac.UserID, r.PathValue("slot"), req.ExpectedRevision)
	if err != nil {
		writeUserStoreError(w, err)
		return
	}
	// Removing a device's sealing is the owner saying that device is done:
	// its enrollment record goes too, so the send gate refuses it until it
	// enrolls again. The server's copy of the sealing is all this can reach;
	// the device keeps what it already imported, and revoking that means
	// rotating the identity.
	if deviceID, isDevice := strings.CutPrefix(r.PathValue("slot"), users.EnvelopeSlotDevicePrefix); isDevice {
		if !s.clearDeviceEnrollment(w, ac.UserID, deviceID) {
			return
		}
	}
	s.logger.Info("pgp envelope slot deleted", "user_id", ac.UserID, "slot", r.PathValue("slot"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pgpRevision": u.PGPRevision})
}
