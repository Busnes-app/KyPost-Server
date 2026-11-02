package api

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// Device approval (#516). A device that redeems a web-minted pairing token is
// held here, PENDING, instead of being written to the account: it has no row in
// native_devices, so deviceAuthFromRequest cannot find it, and it gets no mail,
// contacts, push or App Pull until the signed-in user approves it. The token is
// a bearer credential that can leak to another app (implicit VIEW intent,
// screenshots), so redeeming it proves nothing about WHO redeemed it.
//
// Memory only, on purpose: a restart drops every pending device, which is the
// safe direction, and expiry needs no sweeper — an entry past its deadline is
// ignored and deleted the next time the map is touched. Guarded by pairingMu,
// which already serializes registration with credential revocation.
//
// There is no switch, env var or setting that skips this. Only a token minted
// with the signed `direct` claim registers immediately, and only native sign-on
// and review pairing mint one: both return the token in the HTTP response to
// the client that just proved the account credential, so it never travels as
// a link. See docs/device-approval.md.

const (
	// pendingDeviceTTL bounds how long a pending device waits for approval and
	// how long an approved outcome stays readable at the status endpoint.
	pendingDeviceTTL = 5 * time.Minute
	// maxPendingDevicesPerUser caps entries one account can hold. Each needs a
	// fresh step-up-minted token, so this only bounds memory.
	maxPendingDevicesPerUser = 5
	// pendingPollSeconds is the poll interval the status endpoint advertises.
	pendingPollSeconds = 3
)

type pendingDevice struct {
	ownerID      string
	subscriberID string
	device       state.NativeDevice
	// encryptionEnrolled is the register request's value, applied on approval.
	encryptionEnrolled *bool
	osVersion          string
	requestIP          string
	approvalCode       string
	requestedAt        time.Time
	expiresAt          time.Time
	// approvedID is set once approved: the id the device was persisted under,
	// which differs from the pending id when the upsert merged it into an
	// existing row. The entry stays until expiresAt so the device can read it.
	approvedID string
}

// pendingLocked returns the live entry for id, deleting it if expired.
// Caller holds pairingMu.
func (s *Server) pendingLocked(id string, now time.Time) (*pendingDevice, bool) {
	p, ok := s.pendingDevices[id]
	if !ok {
		return nil, false
	}
	if !now.Before(p.expiresAt) {
		delete(s.pendingDevices, id)
		return nil, false
	}
	return p, true
}

// pendingForUserLocked returns userID's live entries still awaiting approval,
// purging every expired entry on the way. Caller holds pairingMu.
func (s *Server) pendingForUserLocked(userID string, now time.Time) []*pendingDevice {
	var out []*pendingDevice
	for id := range s.pendingDevices {
		if p, ok := s.pendingLocked(id, now); ok && p.ownerID == userID && p.approvedID == "" {
			out = append(out, p)
		}
	}
	return out
}

// dropPendingDevicesLocked forgets every pending device of userID, for
// credential revocation. Caller holds pairingMu.
func (s *Server) dropPendingDevicesLocked(userID string) {
	for id, p := range s.pendingDevices {
		if p.ownerID == userID {
			delete(s.pendingDevices, id)
		}
	}
}

// newApprovalCode is a 6-digit code shown on the device and in the web prompt,
// so the user can tell their own device's request from one made by whoever
// else redeemed the link.
func newApprovalCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// holdPendingDevice records device as pending and answers 202. Caller holds
// pairingMu and has consumed the pairing token's nonce.
func (s *Server) holdPendingDevice(w http.ResponseWriter, r *http.Request, ownerID, subscriberID string, device state.NativeDevice, rawSecret string, req nativeRegisterRequest) {
	now := time.Now().UTC()
	if p, ok := s.pendingLocked(device.DeviceID, now); ok && (p.ownerID != ownerID || p.approvedID == "") {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "a pairing request for this device is already waiting for approval"})
		return
	}
	if len(s.pendingForUserLocked(ownerID, now)) >= maxPendingDevicesPerUser {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "too many devices are waiting for approval; approve or deny them first"})
		return
	}
	code, err := newApprovalCode()
	if err != nil {
		http.Error(w, "failed to mint approval code", http.StatusInternalServerError)
		return
	}
	if s.pendingDevices == nil {
		s.pendingDevices = map[string]*pendingDevice{}
	}
	p := &pendingDevice{
		ownerID:            ownerID,
		subscriberID:       subscriberID,
		device:             device,
		encryptionEnrolled: req.EncryptionEnrolled,
		osVersion:          clampField(req.OSVersion, maxDeviceTextLen),
		requestIP:          clientIP(r),
		approvalCode:       code,
		requestedAt:        now,
		expiresAt:          now.Add(pendingDeviceTTL),
	}
	s.pendingDevices[device.DeviceID] = p
	s.logger.Info("native device pairing awaiting approval", "user_id", ownerID, "device_id", device.DeviceID)
	writeJSON(w, http.StatusAccepted, map[string]any{
		// ok/synced false: a client that predates approval reads this as a
		// failed registration instead of a paired device.
		"ok":                  false,
		"synced":              false,
		"status":              "pending",
		"error":               "approve this device on the web page that showed the pairing code",
		"deviceId":            device.DeviceID,
		"deviceSecret":        rawSecret,
		"approvalCode":        code,
		"expiresAt":           p.expiresAt.Format(time.RFC3339),
		"statusEndpoint":      s.nativeEndpoint("/api/notifications/native/pairing-status"),
		"pollIntervalSeconds": pendingPollSeconds,
	})
}

func (s *Server) nativeEndpoint(path string) string {
	base := s.pairingBaseURL()
	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + path
}

// handleNativePairingStatus tells a device whether it was approved. It
// authenticates with the device's own id and secret: a pending entry is
// checked first, then the paired-device list (deviceAuthFromRequest), so a
// device that polls after its pending entry expired still learns it was
// approved. Denied, expired and unknown are deliberately one answer, 410.
func (s *Server) handleNativePairingStatus(w http.ResponseWriter, r *http.Request) {
	deviceID, deviceSecret := deviceCredentialsFromRequest(r)
	if deviceID != "" && deviceSecret != "" {
		s.pairingMu.Lock()
		p, found := s.pendingLocked(deviceID, time.Now().UTC())
		var entry pendingDevice
		if found {
			entry = *p
		}
		s.pairingMu.Unlock()
		if found {
			okSecret, err := users.VerifyDeviceSecret(r.Context(), entry.device.SecretHash, deviceSecret)
			if err != nil {
				writeKDFBusy(w)
				return
			}
			if okSecret && entry.approvedID == "" {
				writeJSON(w, http.StatusAccepted, map[string]any{
					"status":              "pending",
					"approvalCode":        entry.approvalCode,
					"expiresAt":           entry.expiresAt.Format(time.RFC3339),
					"pollIntervalSeconds": pendingPollSeconds,
				})
				return
			}
			if okSecret {
				s.writeApprovedStatus(w, entry.ownerID, entry.approvedID, entry.device.Transport)
				return
			}
		}
	}
	userID, device, ok, retryAfter := s.deviceAuthFromRequest(r)
	if !ok {
		if retryAfter != 0 {
			writeDeviceAuthFailure(w, retryAfter)
			return
		}
		writeJSON(w, http.StatusGone, map[string]any{
			"status": "rejected",
			"error":  "this pairing was denied or has expired; scan a new pairing code",
		})
		return
	}
	s.writeApprovedStatus(w, userID, device.DeviceID, device.Transport)
}

func (s *Server) writeApprovedStatus(w http.ResponseWriter, userID, deviceID, transport string) {
	store, err := s.userStore(userID)
	if err != nil {
		http.Error(w, "failed to open user state", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"synced":       true,
		"status":       "approved",
		"deviceId":     deviceID,
		"deliveryMode": store.NativeDeliveryMode(),
		"pullEndpoint": s.nativeEndpoint("/api/notifications/native/pull"),
		"transport":    transport,
	})
}

// handleNativePendingDevices lists the signed-in user's devices awaiting
// approval, with what a user needs to recognise their own: model, OS, the
// request IP and the approval code the device shows.
func (s *Server) handleNativePendingDevices(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	s.pairingMu.Lock()
	entries := s.pendingForUserLocked(ac.UserID, time.Now().UTC())
	out := make([]map[string]any, 0, len(entries))
	for _, p := range entries {
		out = append(out, map[string]any{
			"deviceId":     p.device.DeviceID,
			"deviceName":   p.device.DeviceName,
			"platform":     p.device.Platform,
			"osVersion":    p.osVersion,
			"appVersion":   p.device.AppVersion,
			"userAgent":    p.device.UserAgent,
			"requestIp":    p.requestIP,
			"approvalCode": p.approvalCode,
			"requestedAt":  p.requestedAt.Format(time.RFC3339),
			"expiresAt":    p.expiresAt.Format(time.RFC3339),
		})
	}
	s.pairingMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"pending": out})
}

// handleNativePendingApprove writes the signed-in user's pending device to the
// account. Another user's id answers exactly like an unknown one.
func (s *Server) handleNativePendingApprove(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	id := strings.TrimSpace(r.PathValue("deviceId"))
	s.pairingMu.Lock()
	defer s.pairingMu.Unlock()
	p, found := s.pendingLocked(id, time.Now().UTC())
	if !found || p.ownerID != ac.UserID || p.approvedID != "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no device with that id is waiting for approval; it may have expired"})
		return
	}
	// Revocation rotates the subscriber id and drops pending entries under this
	// lock; recheck anyway, as register does, so nothing minted before a
	// revocation is written after it.
	store, err := s.userStore(ac.UserID)
	if err != nil {
		http.Error(w, "failed to open user state", http.StatusInternalServerError)
		return
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil || !u.Active || store.SubscriberID() != p.subscriberID {
		delete(s.pendingDevices, id)
		writeJSON(w, http.StatusGone, map[string]any{"error": "this pairing request is no longer valid; pair the device again"})
		return
	}
	registeredID, devices, ok := s.persistNativeDevice(w, r, store, ac.UserID, p.device, p.encryptionEnrolled)
	if !ok {
		delete(s.pendingDevices, id)
		return
	}
	p.approvedID = registeredID
	s.logger.Info("native device pairing approved", "user_id", ac.UserID, "device_id", registeredID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deviceId": registeredID, "devices": devices})
}

// handleNativePendingDeny deletes the signed-in user's pending device at once.
func (s *Server) handleNativePendingDeny(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	id := strings.TrimSpace(r.PathValue("deviceId"))
	s.pairingMu.Lock()
	defer s.pairingMu.Unlock()
	p, found := s.pendingLocked(id, time.Now().UTC())
	if !found || p.ownerID != ac.UserID || p.approvedID != "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no device with that id is waiting for approval; it may have expired"})
		return
	}
	delete(s.pendingDevices, id)
	s.logger.Info("native device pairing denied", "user_id", ac.UserID, "device_id", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
