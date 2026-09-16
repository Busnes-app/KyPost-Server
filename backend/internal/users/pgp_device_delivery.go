package users

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

// Device delivery: the transport copy of a sealing prepared for one paired
// device. The server cannot open it; what it can do is bind it to the key it
// was sealed to, the identity and key-material generation it was prepared
// from, and the envelope version the account's keyring requires, so a device
// never receives a sealing that was not meant for exactly its current key and
// the account's current material.

var (
	ErrInvalidDeviceEnvelope    = errors.New("device envelope is not a well-formed v2 or v3 sealing")
	ErrDeviceEnvelopeVersion    = errors.New("device envelope version does not match the account: a converted account takes v3, a legacy account v2")
	ErrDeviceDeliveryIncomplete = errors.New("device delivery needs the enrollment key it was sealed to and the expected fingerprint")
	ErrPGPGenerationChanged     = errors.New("PGP key material changed; reload and prepare the delivery again")
)

const deviceEnvelopeAlg = "ECDH-P256+HKDF-SHA256+A256GCM"

// DeviceDelivery is one sealing the browser prepared for one device.
type DeviceDelivery struct {
	DeviceID string
	Envelope string
	AddedAt  string
	// EnrollmentKey is the device public key the envelope was sealed to; the
	// caller has already checked it is the key the device currently publishes.
	EnrollmentKey       string
	ExpectedFingerprint string
	// ExpectedGeneration is the material generation the browser sealed; 0 for
	// a legacy account, which has none.
	ExpectedGeneration uint64
	ExpectedRevision   *uint64
}

// ParseDeviceEnvelopeVersion checks the framing of a sealed device envelope
// without decrypting it and returns its version. Anything but the exact
// {v, alg, epk, iv, ct} shape with a P-256 point, a 12-byte IV and at least a
// tag's worth of ciphertext is refused before it reaches the store.
func ParseDeviceEnvelopeVersion(envelope string) (int, error) {
	if err := ValidateWrappedEnvelope(envelope); err != nil {
		return 0, err
	}
	var e struct {
		V   int    `json:"v"`
		Alg string `json:"alg"`
		EPK string `json:"epk"`
		IV  string `json:"iv"`
		CT  string `json:"ct"`
	}
	dec := json.NewDecoder(strings.NewReader(envelope))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return 0, ErrInvalidDeviceEnvelope
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return 0, ErrInvalidDeviceEnvelope
	}
	if (e.V != 2 && e.V != 3) || e.Alg != deviceEnvelopeAlg {
		return 0, ErrInvalidDeviceEnvelope
	}
	epk, err := base64.StdEncoding.DecodeString(e.EPK)
	if err != nil || len(epk) != 65 || epk[0] != 0x04 {
		return 0, ErrInvalidDeviceEnvelope
	}
	iv, err := base64.StdEncoding.DecodeString(e.IV)
	if err != nil || len(iv) != 12 {
		return 0, ErrInvalidDeviceEnvelope
	}
	ct, err := base64.StdEncoding.DecodeString(e.CT)
	if err != nil || len(ct) < 16 {
		return 0, ErrInvalidDeviceEnvelope
	}
	return e.V, nil
}

// SetPGPDeviceEnvelope stores the transport copy for one device and returns
// the envelope version it recorded. It is the only writer of device slots.
//
// A converted account (one with a keyring) accepts only a v3 envelope, which
// carries the complete ring; a legacy account only v2. The caller's expected
// fingerprint, revision and material generation must all be the account's
// current ones, so a sealing prepared from a stale snapshot is refused rather
// than delivered. The record keeps the version, generation, fingerprint and
// enrollment key; while it lives, the device's acknowledgement is checked
// against exactly that delivery, including that the device still publishes
// the key it was sealed to (handlePGPDeviceEnrollmentState).
func (s *Store) SetPGPDeviceEnvelope(id string, d DeviceDelivery) (User, int, error) {
	version, err := ParseDeviceEnvelopeVersion(d.Envelope)
	if err != nil {
		return User{}, 0, err
	}
	slot := EnvelopeSlotDevicePrefix + d.DeviceID
	if !ValidEnvelopeSlot(slot) {
		return User{}, 0, ErrInvalidEnvelopeSlot
	}
	if strings.TrimSpace(d.EnrollmentKey) == "" || strings.TrimSpace(d.ExpectedFingerprint) == "" {
		return User{}, 0, ErrDeviceDeliveryIncomplete
	}
	expiresAt := time.Now().UTC().Add(DeviceEnvelopeTTL).Format(time.RFC3339)
	u, err := s.mutatePGP(id, d.ExpectedRevision, func(u *User) error {
		if u.PGPFingerprint == "" {
			return ErrNoPGPIdentity
		}
		if !strings.EqualFold(u.PGPFingerprint, d.ExpectedFingerprint) {
			return ErrPGPIdentityChanged
		}
		if u.PGPProtection() != PGPProtectionClient {
			return ErrNotClientProtected
		}
		generation, want := uint64(0), 2
		if u.PGPKeyring != nil {
			generation, want = u.PGPKeyring.MaterialGeneration, 3
		}
		if version != want {
			return ErrDeviceEnvelopeVersion
		}
		if d.ExpectedGeneration != generation {
			return ErrPGPGenerationChanged
		}
		record := WrappedEnvelope{
			Slot: slot, Envelope: d.Envelope, AddedAt: d.AddedAt, ExpiresAt: expiresAt,
			Version: version, MaterialGeneration: generation, Fingerprint: u.PGPFingerprint, EnrollmentKey: d.EnrollmentKey,
		}
		for i := range u.PGPWrappedEnvelopes {
			if u.PGPWrappedEnvelopes[i].Slot == slot {
				u.PGPWrappedEnvelopes[i] = record
				return nil
			}
		}
		// A new slot, not a replace: the cap applies, counting live entries
		// only so an expired transport copy frees its headroom.
		live := 0
		for _, e := range u.PGPWrappedEnvelopes {
			if !e.expired() {
				live++
			}
		}
		if live >= maxWrappedEnvelopeSlots {
			return ErrTooManyEnvelopeSlots
		}
		u.PGPWrappedEnvelopes = append(u.PGPWrappedEnvelopes, record)
		return nil
	})
	return u, version, err
}
