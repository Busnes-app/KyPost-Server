package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// putDeviceEnvelope is the device-slot arm of PUT /api/pgp/identity/envelope/{slot}.
//
// The browser sealed to a device key it verified by SAS moments ago; this
// binds the upload to that key, to the version the device advertised, and to
// the account's current material, so the device never receives a sealing
// prepared against a key or a ring the account has moved past. The caller
// has already passed the session step-up.
func (s *Server) putDeviceEnvelope(w http.ResponseWriter, r *http.Request, userID, deviceID, envelope, enrollmentKey, expectedFingerprint string, generation, expectedRevision *uint64) {
	store, err := s.userStore(userID)
	if err != nil {
		http.Error(w, "state unavailable", http.StatusInternalServerError)
		return
	}
	device, ok := store.GetNativeDevice(deviceID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such paired device"})
		return
	}
	enrollmentKey = strings.TrimSpace(enrollmentKey)
	if enrollmentKey == "" {
		http.Error(w, "enrollmentPublicKey is required for a device slot", http.StatusBadRequest)
		return
	}
	if device.EnrollmentPublicKey == "" || enrollmentKey != device.EnrollmentPublicKey {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":             "the device's enrollment key changed since it was verified; restart the enrollment",
			"enrollmentChanged": true,
		})
		return
	}
	version, err := users.ParseDeviceEnvelopeVersion(envelope)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !slices.Contains(device.EnrollmentEnvelopeVersions, version) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":             fmt.Sprintf("the device does not support envelope version %d", version),
			"enrollmentChanged": true,
			"envelopeVersions":  device.EnrollmentEnvelopeVersions,
		})
		return
	}
	var expectedGeneration uint64
	if generation != nil {
		expectedGeneration = *generation
	}
	u, _, err := s.users.SetPGPDeviceEnvelope(userID, users.DeviceDelivery{
		DeviceID: deviceID, Envelope: envelope, AddedAt: time.Now().UTC().Format(time.RFC3339),
		EnrollmentKey: enrollmentKey, ExpectedFingerprint: expectedFingerprint,
		ExpectedGeneration: expectedGeneration, ExpectedRevision: expectedRevision,
	})
	if errors.Is(err, users.ErrPGPGenerationChanged) {
		s.writeGenerationChanged(w, userID, err.Error())
		return
	}
	if err != nil {
		writeUserStoreError(w, err)
		return
	}
	// The device row now says what was delivered, unconfirmed. This is the
	// only thing an acknowledgement can later confirm; the device itself can
	// never write it.
	var delivered uint64
	if u.PGPKeyring != nil {
		delivered = u.PGPKeyring.MaterialGeneration
	}
	if err := store.RecordNativeDeviceDelivery(deviceID, state.DeviceEnrollment{Version: version, Generation: delivered, Fingerprint: u.PGPFingerprint}); err != nil {
		http.Error(w, "the sealing was stored but the delivery could not be recorded; retry", http.StatusInternalServerError)
		return
	}
	s.logger.Info("pgp device envelope delivered", "user_id", userID, "device_id", deviceID, "version", strconv.Itoa(version))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pgpRevision": u.PGPRevision, "version": version})
}

// clearDeviceEnrollment forgets one device's enrollment record after its
// sealing was removed. A device that is not paired has nothing to forget; a
// failure to forget one that is answers 500 so the caller retries, because
// the slot is already gone and the send gate must not keep passing.
func (s *Server) clearDeviceEnrollment(w http.ResponseWriter, userID, deviceID string) bool {
	store, err := s.userStore(userID)
	if err != nil {
		http.Error(w, "state unavailable", http.StatusInternalServerError)
		return false
	}
	if _, ok := store.GetNativeDevice(deviceID); !ok {
		return true
	}
	if err := store.SetNativeDeviceEncryptionEnrolled(deviceID, false); err != nil {
		http.Error(w, "the sealing was removed but the device's enrollment could not be cleared; retry", http.StatusInternalServerError)
		return false
	}
	s.logger.Info("pgp device enrollment cleared with its slot", "user_id", userID, "device_id", deviceID)
	return true
}

// writeGenerationChanged answers a request prepared against key material the
// account has moved past, naming the current generation so the client can
// reload rather than retry blindly.
func (s *Server) writeGenerationChanged(w http.ResponseWriter, userID, msg string) {
	var current uint64
	if u, err := s.users.Get(userID); err == nil && u.PGPKeyring != nil {
		current = u.PGPKeyring.MaterialGeneration
	}
	writeJSON(w, http.StatusConflict, map[string]any{"error": msg, "pgpStateChanged": true, "materialGeneration": current})
}

// requireCurrentGeneration gates a client-sealed send on the account's
// current key material. On a converted account the caller must assert the
// generation it encrypted and signed with, and a paired device must also be
// enrolled at that generation: a device holding a retired ring cannot send
// until it re-enrolls, which is what makes clearing a slot mean something.
// Legacy accounts have no generation and are not gated.
func (s *Server) requireCurrentGeneration(w http.ResponseWriter, ac AuthContext, asserted *uint64) bool {
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		http.Error(w, "user unavailable", http.StatusInternalServerError)
		return false
	}
	if u.PGPKeyring == nil {
		return true
	}
	current := u.PGPKeyring.MaterialGeneration
	if asserted == nil || *asserted != current {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":              "the account's key material changed; reload and send again",
			"pgpStateChanged":    true,
			"materialGeneration": current,
		})
		return false
	}
	if ac.DeviceID == "" {
		return true
	}
	store, err := s.userStore(ac.UserID)
	if err != nil {
		http.Error(w, "state unavailable", http.StatusInternalServerError)
		return false
	}
	device, ok := store.GetNativeDevice(ac.DeviceID)
	if !ok || !device.EncryptionEnrolled || device.EnrolledGeneration != current || !strings.EqualFold(device.EnrolledFingerprint, u.PGPFingerprint) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":                "this device holds retired key material; enroll it again before sending",
			"reenrollmentRequired": true,
			"materialGeneration":   current,
		})
		return false
	}
	return true
}
