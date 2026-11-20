package api

import (
	"crypto/rand"
	"encoding/json"
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
	OwnerID      string             `json:"ownerId"`
	SubscriberID string             `json:"subscriberId"`
	Device       state.NativeDevice `json:"device"` // carries only the secret's hash
	// EncryptionEnrolled is the register request's value, applied on approval.
	EncryptionEnrolled *bool     `json:"encryptionEnrolled,omitempty"`
	OSVersion          string    `json:"osVersion"`
	RequestIP          string    `json:"requestIp"`
	ApprovalCode       string    `json:"approvalCode"`
	RequestedAt        time.Time `json:"requestedAt"`
	ExpiresAt          time.Time `json:"expiresAt"`
	// Approving claims the entry for one approval across replicas while it is persisted.
	Approving bool `json:"approving,omitempty"`
	// ApprovedID is set once approved: the id the device was persisted under,
	// which differs from the pending id when the upsert merged it into an
	// existing row. The entry stays until ExpiresAt so the device can read it.
	ApprovedID string `json:"approvedId,omitempty"`
}

// Pending devices live in memory (callers hold pairingMu), or on PostgreSQL as
// auth_challenges rows, so a device registering through one replica is approved from a
// browser on another. Every change is an atomic edit (pendingEdit).

// pendingGet returns the live entry for id. Caller holds pairingMu.
func (s *Server) pendingGet(id string, now time.Time) (pendingDevice, bool) {
	var out pendingDevice
	ok := s.pendingEdit(id, now, func(p *pendingDevice, found bool) (rowEdit, bool) {
		out = *p
		return rowKeep, found
	})
	return out, ok
}

// pendingEdit applies edit to the live entry for id atomically, deleting an expired one on
// the way. Caller holds pairingMu.
func (s *Server) pendingEdit(id string, now time.Time, edit func(p *pendingDevice, found bool) (rowEdit, bool)) bool {
	live := func(p *pendingDevice, found bool) (rowEdit, bool) {
		if found && !now.Before(p.ExpiresAt) {
			_, _ = edit(&pendingDevice{}, false)
			return rowDrop, false
		}
		return edit(p, found)
	}
	if s.shared != nil {
		return editRow(s.shared, "pending", id, live)
	}
	p, found := s.pendingDevices[id]
	var v pendingDevice
	if found {
		v = *p
	}
	action, verdict := live(&v, found)
	switch action {
	case rowWrite:
		s.pendingDevices[id] = &v
	case rowDrop:
		delete(s.pendingDevices, id)
	}
	return verdict
}

// pendingPut records a new entry. Caller holds pairingMu.
func (s *Server) pendingPut(p pendingDevice) error {
	if s.shared != nil {
		return putRow(s.shared, "pending", p.Device.DeviceID, p.OwnerID, p, p.ExpiresAt, false)
	}
	if s.pendingDevices == nil {
		s.pendingDevices = map[string]*pendingDevice{}
	}
	s.pendingDevices[p.Device.DeviceID] = &p
	return nil
}

// pendingForUser returns userID's live entries still awaiting approval, purging expired
// ones on the way. Caller holds pairingMu.
func (s *Server) pendingForUser(userID string, now time.Time) ([]pendingDevice, error) {
	var out []pendingDevice
	if s.shared != nil {
		ctx, cancel := sharedContext()
		defer cancel()
		rows, err := s.shared.UserChallenges(ctx, "pending:", userID)
		if err != nil {
			return nil, err
		}
		for _, data := range rows {
			var p pendingDevice
			if json.Unmarshal(data, &p) == nil && now.Before(p.ExpiresAt) && p.ApprovedID == "" {
				out = append(out, p)
			}
		}
		return out, nil
	}
	for id := range s.pendingDevices {
		if p, ok := s.pendingGet(id, now); ok && p.OwnerID == userID && p.ApprovedID == "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// dropPendingDevicesLocked forgets every pending device of userID, for
// credential revocation. Caller holds pairingMu.
func (s *Server) dropPendingDevicesLocked(userID string) {
	if s.shared != nil {
		ctx, cancel := sharedContext()
		defer cancel()
		_ = s.shared.DropUserChallenges(ctx, "pending:", userID)
		return
	}
	for id, p := range s.pendingDevices {
		if p.OwnerID == userID {
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
	if p, ok := s.pendingGet(device.DeviceID, now); ok && (p.OwnerID != ownerID || p.ApprovedID == "") {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "a pairing request for this device is already waiting for approval"})
		return
	}
	waiting, err := s.pendingForUser(ownerID, now)
	if err != nil {
		writeUnavailable(w)
		return
	}
	if len(waiting) >= maxPendingDevicesPerUser {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "too many devices are waiting for approval; approve or deny them first"})
		return
	}
	code, err := newApprovalCode()
	if err != nil {
		http.Error(w, "failed to mint approval code", http.StatusInternalServerError)
		return
	}
	p := pendingDevice{
		OwnerID:            ownerID,
		SubscriberID:       subscriberID,
		Device:             device,
		EncryptionEnrolled: req.EncryptionEnrolled,
		OSVersion:          clampField(req.OSVersion, maxDeviceTextLen),
		RequestIP:          clientIP(r),
		ApprovalCode:       code,
		RequestedAt:        now,
		ExpiresAt:          now.Add(pendingDeviceTTL),
	}
	// The same device ID re-registering after its approval replaces the old entry.
	s.pendingEdit(device.DeviceID, now, func(*pendingDevice, bool) (rowEdit, bool) { return rowDrop, true })
	if err := s.pendingPut(p); err != nil {
		writeUnavailable(w)
		return
	}
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
		"expiresAt":           p.ExpiresAt.Format(time.RFC3339),
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
		entry, found := s.pendingGet(deviceID, time.Now().UTC())
		s.pairingMu.Unlock()
		if found {
			okSecret, err := users.VerifyDeviceSecret(r.Context(), entry.Device.SecretHash, deviceSecret)
			if err != nil {
				writeKDFBusy(w)
				return
			}
			if okSecret && entry.ApprovedID == "" {
				writeJSON(w, http.StatusAccepted, map[string]any{
					"status":              "pending",
					"approvalCode":        entry.ApprovalCode,
					"expiresAt":           entry.ExpiresAt.Format(time.RFC3339),
					"pollIntervalSeconds": pendingPollSeconds,
				})
				return
			}
			if okSecret {
				s.writeApprovedStatus(w, entry.OwnerID, entry.ApprovedID, entry.Device.Transport)
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
	writeJSON(w, http.StatusOK, s.withRelayIdentity(map[string]any{
		"ok":           true,
		"synced":       true,
		"status":       "approved",
		"deviceId":     deviceID,
		"deliveryMode": store.NativeDeliveryMode(),
		"pullEndpoint": s.nativeEndpoint("/api/notifications/native/pull"),
		"transport":    transport,
	}))
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
	entries, err := s.pendingForUser(ac.UserID, time.Now().UTC())
	if err != nil {
		s.pairingMu.Unlock()
		writeUnavailable(w)
		return
	}
	out := make([]map[string]any, 0, len(entries))
	for _, p := range entries {
		out = append(out, map[string]any{
			"deviceId":     p.Device.DeviceID,
			"deviceName":   p.Device.DeviceName,
			"platform":     p.Device.Platform,
			"osVersion":    p.OSVersion,
			"appVersion":   p.Device.AppVersion,
			"userAgent":    p.Device.UserAgent,
			"requestIp":    p.RequestIP,
			"approvalCode": p.ApprovalCode,
			"requestedAt":  p.RequestedAt.Format(time.RFC3339),
			"expiresAt":    p.ExpiresAt.Format(time.RFC3339),
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
	// Through the device write: a revocation on another replica waits for it, or runs first.
	release, err := s.holdUserPairing(r.Context(), ac.UserID)
	if err != nil {
		writeUnavailable(w)
		return
	}
	defer release()
	// Claim the entry first: of two approvals racing on two replicas, one persists the device.
	var p pendingDevice
	if !s.pendingEdit(id, time.Now().UTC(), func(v *pendingDevice, found bool) (rowEdit, bool) {
		if !found || v.OwnerID != ac.UserID || v.ApprovedID != "" || v.Approving {
			return rowKeep, false
		}
		v.Approving = true
		p = *v
		return rowWrite, true
	}) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no device with that id is waiting for approval; it may have expired"})
		return
	}
	drop := func() {
		s.pendingEdit(id, time.Now().UTC(), func(*pendingDevice, bool) (rowEdit, bool) { return rowDrop, true })
	}
	// Revocation rotates the subscriber id and drops pending entries under this
	// lock; recheck anyway, as register does, so nothing minted before a
	// revocation is written after it.
	store, err := s.userStore(ac.UserID)
	if err != nil {
		drop()
		http.Error(w, "failed to open user state", http.StatusInternalServerError)
		return
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil || !u.Active || store.SubscriberID() != p.SubscriberID {
		drop()
		writeJSON(w, http.StatusGone, map[string]any{"error": "this pairing request is no longer valid; pair the device again"})
		return
	}
	if s.nativeApproveBeforePersist != nil {
		s.nativeApproveBeforePersist()
	}
	registeredID, devices, ok := s.persistNativeDevice(w, r, store, ac.UserID, p.Device, p.EncryptionEnrolled)
	if !ok {
		drop()
		return
	}
	// The outcome stays readable for a full window after approval. A merge
	// into an existing row (same push token) gives the device another id, and
	// this entry is the only place the device can learn it.
	s.pendingEdit(id, time.Now().UTC(), func(v *pendingDevice, found bool) (rowEdit, bool) {
		if !found {
			return rowKeep, false
		}
		v.ApprovedID, v.Approving, v.ExpiresAt = registeredID, false, time.Now().UTC().Add(pendingDeviceTTL)
		return rowWrite, true
	})
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
	if !s.pendingEdit(id, time.Now().UTC(), func(v *pendingDevice, found bool) (rowEdit, bool) {
		if !found || v.OwnerID != ac.UserID || v.ApprovedID != "" || v.Approving {
			return rowKeep, false
		}
		return rowDrop, true
	}) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no device with that id is waiting for approval; it may have expired"})
		return
	}
	s.logger.Info("native device pairing denied", "user_id", ac.UserID, "device_id", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
