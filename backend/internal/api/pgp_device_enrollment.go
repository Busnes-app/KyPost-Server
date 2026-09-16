package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Busness-app/kypost-server/backend/internal/state"
	"github.com/Busness-app/kypost-server/backend/internal/users"
)

// Device-authenticated halves of the enrollment ceremony. Both resolve the
// caller through deviceAuthFromRequest, which returns the VERIFIED device
// record, so neither ever reads an identity out of the request.
//
// These are deliberately not withAuth and not withMailAuth. withMailAuth would
// admit a session, which has no device to scope to; withAuth would exclude the
// device entirely. See
// docs/superpowers/specs/2026-08-04-device-enrollment-design.md.

// maxEnrollmentPublicKeyBytes bounds the published key. An uncompressed P-256
// point is 65 bytes raw and a few hundred base64-wrapped in any sane encoding;
// this leaves generous headroom while keeping an unbounded string out of the
// device table.
const maxEnrollmentPublicKeyBytes = 4 << 10

// handlePGPPublishEnrollmentKey records the calling device's enrollment public
// key.
//
// A public key is not a capability — it lets a browser seal TO this device and
// confers nothing by itself — which is why a device may publish its own under
// its pairing credential while only a session may mint the sealing.
func (s *Server) handlePGPPublishEnrollmentKey(w http.ResponseWriter, r *http.Request) {
	// userID is the owner deviceAuthFromRequest resolved the credential
	// through; device.UserID is only a stamp on the row and is empty for
	// devices paired before it existed.
	userID, device, ok, retryAfter := s.deviceAuthFromRequest(r)
	if !ok {
		writeDeviceAuthFailure(w, retryAfter)
		return
	}
	// This route MUTATES on a device credential, which no shared middleware
	// meters. See meterDeviceWrite.
	if !s.meterDeviceWrite(w, r, userID) {
		return
	}
	var req struct {
		PublicKey        string `json:"publicKey"`
		EnvelopeVersions []int  `json:"envelopeVersions"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEnrollmentPublicKeyBytes))
	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	publicKey := strings.TrimSpace(req.PublicKey)
	if publicKey == "" {
		http.Error(w, "publicKey is required", http.StatusBadRequest)
		return
	}
	store, err := s.userStore(userID)
	if err != nil {
		http.Error(w, "state unavailable", http.StatusInternalServerError)
		return
	}
	// device.DeviceID comes from the verified credential, never from the body.
	if _, err := store.SetNativeDeviceEnrollmentKey(device.DeviceID, publicKey,
		time.Now().UTC().Format(time.RFC3339), req.EnvelopeVersions); err != nil {
		if errors.Is(err, state.ErrInvalidEnrollmentVersions) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "could not store the enrollment key", http.StatusInternalServerError)
		return
	}
	s.logger.Info("pgp enrollment key published", "user_id", userID, "device_id", device.DeviceID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handlePGPDeviceEnvelope serves the ONE envelope sealed for the calling device.
//
// It takes no slot parameter, by design. The general GET on
// /api/pgp/identity/envelope/{slot} stays session-only so a device cannot ask
// for ANOTHER DEVICE'S sealing. Here the slot name is built from the verified
// device record, so there is no input to abuse.
//
// Note what that rule does NOT withhold: the password-wrapped envelope is
// already reachable on a device credential, via GET /api/pgp/identity/wrapped
// and GET /api/pgp/bootstrap, both of which are withMailAuth. That is
// deliberate and documented (docs/E2E_PGP.md) — the blob is useless without the
// password-derived key. An earlier version of this comment claimed the opposite
// and contradicted the docs.
//
// Serving this one envelope to this one device is safe: it is sealed to a key
// whose private half is non-extractable from that device's secure element, so
// no other caller gains anything by obtaining it.
func (s *Server) handlePGPDeviceEnvelope(w http.ResponseWriter, r *http.Request) {
	userID, device, ok, retryAfter := s.deviceAuthFromRequest(r)
	if !ok {
		writeDeviceAuthFailure(w, retryAfter)
		return
	}
	u, err := s.users.Get(userID)
	if err != nil {
		http.Error(w, "user unavailable", http.StatusInternalServerError)
		return
	}
	slot := users.EnvelopeSlotDevicePrefix + device.DeviceID
	// WrappedEnvelopes() already omits expired entries, so a transport copy
	// whose TTL has passed correctly reads as absent rather than being served.
	// Beside the envelope, the snapshot it was prepared from, so the device
	// validates the ring it decrypts against the same metadata the browser
	// sealed and can acknowledge exactly that.
	for _, e := range u.WrappedEnvelopes() {
		if e.Slot == slot {
			writeJSON(w, http.StatusOK, map[string]any{
				"slot": e.Slot, "envelope": e.Envelope, "version": e.Version,
				"fingerprint": e.Fingerprint, "materialGeneration": e.MaterialGeneration,
				"pgpRevision": u.PGPRevision, "keyring": u.PGPKeyring, "publicKey": u.PGPPublicKey,
			})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"error": "no envelope sealed for this device"})
}

// maxEnrollmentStateBytes bounds the state report. The body is a boolean and
// at most three short fields; this is generous headroom and keeps an unbounded
// read off a device credential.
const maxEnrollmentStateBytes = 1 << 10

// handlePGPDeviceEnrollmentState records the calling device's own answer to
// "can I still open my local envelope", and, for a device that just imported
// a delivery, exactly what it holds.
//
// This exists as its own route rather than as a field on registration because the
// marker must not depend on any push transport. Registration cannot run without a
// push token -- the Android client returns early on a blank one -- so a pull-mode
// device with FCM disabled could never restate it, and the marker would freeze at
// whatever it was when that device last had a token. And on UnifiedPush the
// registration call is driven by a third-party distributor's cycle, which must not
// decide when a security-relevant marker is refreshed.
//
// The boolean is REQUIRED here, unlike the tri-state pointer on registration.
// Absent there means "no opinion", so an older client is never silently marked
// un-enrolled. Here, stating an opinion is the entire purpose, so an absent field
// is a malformed request rather than a false report -- accepting it as false would
// let a truncated body mark a working device unreadable.
//
// A converted account needs more than the boolean. Its ring has a material
// generation, and a device that says "enrolled" must say which generation,
// fingerprint and envelope version it holds; the answer is checked against the
// account's current material and refused with the current values when it is
// not that. The legacy boolean alone is never proof of v3 enrollment.
func (s *Server) handlePGPDeviceEnrollmentState(w http.ResponseWriter, r *http.Request) {
	userID, device, ok, retryAfter := s.deviceAuthFromRequest(r)
	if !ok {
		writeDeviceAuthFailure(w, retryAfter)
		return
	}
	// This route MUTATES on a device credential, which no shared middleware meters.
	if !s.meterDeviceWrite(w, r, userID) {
		return
	}
	var req struct {
		EncryptionEnrolled *bool   `json:"encryptionEnrolled"`
		EnvelopeVersion    int     `json:"envelopeVersion"`
		MaterialGeneration *uint64 `json:"materialGeneration"`
		Fingerprint        string  `json:"fingerprint"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxEnrollmentStateBytes)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.EncryptionEnrolled == nil {
		http.Error(w, "encryptionEnrolled is required", http.StatusBadRequest)
		return
	}
	store, err := s.userStore(userID)
	if err != nil {
		http.Error(w, "state unavailable", http.StatusInternalServerError)
		return
	}
	// device.DeviceID comes from the verified credential, never from the body.
	if !*req.EncryptionEnrolled {
		if err := store.SetNativeDeviceEncryptionEnrolled(device.DeviceID, false); err != nil {
			http.Error(w, "could not store the enrollment state", http.StatusInternalServerError)
			return
		}
		s.logger.Info("pgp enrollment state reported", "user_id", userID, "device_id", device.DeviceID, "enrolled", "false")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	u, err := s.users.Get(userID)
	if err != nil {
		http.Error(w, "user unavailable", http.StatusInternalServerError)
		return
	}
	var generation uint64
	if u.PGPKeyring != nil {
		generation = u.PGPKeyring.MaterialGeneration
	}
	fingerprint := strings.ToUpper(strings.TrimSpace(req.Fingerprint))
	if req.EnvelopeVersion == 0 && req.MaterialGeneration == nil && fingerprint == "" {
		if u.PGPKeyring != nil {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":              "this account's keyring needs a generation-aware acknowledgement: state envelopeVersion, materialGeneration and fingerprint",
				"generationRequired": true,
				"materialGeneration": generation,
				"fingerprint":        u.PGPFingerprint,
			})
			return
		}
		if err := store.SetNativeDeviceEncryptionEnrolled(device.DeviceID, true); err != nil {
			http.Error(w, "could not store the enrollment state", http.StatusInternalServerError)
			return
		}
		s.logger.Info("pgp enrollment state reported", "user_id", userID, "device_id", device.DeviceID, "enrolled", "true")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if req.EnvelopeVersion == 0 || req.MaterialGeneration == nil || fingerprint == "" {
		http.Error(w, "envelopeVersion, materialGeneration and fingerprint go together", http.StatusBadRequest)
		return
	}
	if !slices.Contains(device.EnrollmentEnvelopeVersions, req.EnvelopeVersion) || *req.MaterialGeneration != generation || !strings.EqualFold(fingerprint, u.PGPFingerprint) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":              "the acknowledged key material is not the account's current material; fetch the current envelope and enroll again",
			"pgpStateChanged":    true,
			"materialGeneration": generation,
			"fingerprint":        u.PGPFingerprint,
		})
		return
	}
	if err := store.SetNativeDeviceEnrollment(device.DeviceID, state.DeviceEnrollment{Version: req.EnvelopeVersion, Generation: generation, Fingerprint: u.PGPFingerprint}); err != nil {
		http.Error(w, "could not store the enrollment state", http.StatusInternalServerError)
		return
	}
	s.logger.Info("pgp enrollment acknowledged", "user_id", userID, "device_id", device.DeviceID,
		"version", strconv.Itoa(req.EnvelopeVersion), "generation", strconv.FormatUint(generation, 10))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
