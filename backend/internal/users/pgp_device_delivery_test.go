package users

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// deviceEnvelope is a well-formed sealing of the given version: the server
// never opens one, so zero bytes of the right lengths are all the framing
// check needs.
func deviceEnvelope(version int) string {
	epk := make([]byte, 65)
	epk[0] = 4
	return `{"v":` + string(rune('0'+version)) + `,"alg":"ECDH-P256+HKDF-SHA256+A256GCM","epk":"` +
		base64.StdEncoding.EncodeToString(epk) + `","iv":"` + base64.StdEncoding.EncodeToString(make([]byte, 12)) +
		`","ct":"` + base64.StdEncoding.EncodeToString(make([]byte, 16)) + `"}`
}

// seedDeviceSlot delivers a v2 sealing to a device through the one writer of
// device slots, for tests that only care that a slot exists.
func seedDeviceSlot(store *Store, id, deviceID string) (User, error) {
	owner, err := store.Get(id)
	if err != nil {
		return User{}, err
	}
	u, _, err := store.SetPGPDeviceEnvelope(id, DeviceDelivery{DeviceID: deviceID, Envelope: deviceEnvelope(2), EnrollmentKey: "K", ExpectedFingerprint: owner.PGPFingerprint})
	return u, err
}

func TestParseDeviceEnvelopeVersion(t *testing.T) {
	for _, v := range []int{2, 3} {
		if got, err := ParseDeviceEnvelopeVersion(deviceEnvelope(v)); err != nil || got != v {
			t.Fatalf("v%d: got %d, %v", v, got, err)
		}
	}
	good := deviceEnvelope(3)
	point := base64.StdEncoding.EncodeToString(append([]byte{4}, make([]byte, 64)...))
	bad := map[string]string{
		"unknown version":  strings.Replace(good, `"v":3`, `"v":4`, 1),
		"legacy shape":     `{"v":2}`,
		"other algorithm":  strings.Replace(good, "A256GCM", "A128GCM", 1),
		"extra field":      strings.Replace(good, `{"v":3`, `{"x":1,"v":3`, 1),
		"trailing json":    good + `{}`,
		"short point":      strings.Replace(good, point, base64.StdEncoding.EncodeToString(make([]byte, 33)), 1),
		"compressed point": strings.Replace(good, point, base64.StdEncoding.EncodeToString(append([]byte{2}, make([]byte, 64)...)), 1),
		"short iv":         strings.Replace(good, `"iv":"`+base64.StdEncoding.EncodeToString(make([]byte, 12)), `"iv":"`+base64.StdEncoding.EncodeToString(make([]byte, 8)), 1),
		"no tag":           strings.Replace(good, `"ct":"`+base64.StdEncoding.EncodeToString(make([]byte, 16)), `"ct":"`+base64.StdEncoding.EncodeToString(make([]byte, 15)), 1),
		"not json":         "nope",
	}
	for name, envelope := range bad {
		if _, err := ParseDeviceEnvelopeVersion(envelope); !errors.Is(err, ErrInvalidDeviceEnvelope) {
			t.Errorf("%s: err = %v, want ErrInvalidDeviceEnvelope", name, err)
		}
	}
	if _, err := ParseDeviceEnvelopeVersion(strings.Repeat("x", MaxWrappedEnvelopeBytes+1)); !errors.Is(err, ErrWrappedEnvelopeTooLarge) {
		t.Errorf("oversized: %v", err)
	}
}

func TestSetPGPDeviceEnvelopeBindsVersionGenerationAndKey(t *testing.T) {
	s, id, update := keyringCandidate(t)
	u, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	delivery := func(version int) DeviceDelivery {
		return DeviceDelivery{DeviceID: "phone", Envelope: deviceEnvelope(version), AddedAt: "now", EnrollmentKey: "DEVKEY", ExpectedFingerprint: u.PGPFingerprint}
	}

	// A legacy account takes v2 only, and records what it delivered.
	if _, _, err := s.SetPGPDeviceEnvelope(id, delivery(3)); !errors.Is(err, ErrDeviceEnvelopeVersion) {
		t.Fatalf("v3 on a legacy account: %v", err)
	}
	stored, version, err := s.SetPGPDeviceEnvelope(id, delivery(2))
	if err != nil || version != 2 {
		t.Fatalf("v2 on a legacy account: %d %v", version, err)
	}
	var slot WrappedEnvelope
	for _, e := range stored.WrappedEnvelopes() {
		if e.Slot == EnvelopeSlotDevicePrefix+"phone" {
			slot = e
		}
	}
	if slot.Version != 2 || slot.MaterialGeneration != 0 || slot.Fingerprint != u.PGPFingerprint || slot.EnrollmentKey != "DEVKEY" || slot.ExpiresAt == "" {
		t.Fatalf("recorded slot = %+v", slot)
	}
	incomplete := delivery(2)
	incomplete.EnrollmentKey = ""
	if _, _, err := s.SetPGPDeviceEnvelope(id, incomplete); !errors.Is(err, ErrDeviceDeliveryIncomplete) {
		t.Fatalf("no enrollment key: %v", err)
	}
	wrongKey := delivery(2)
	wrongKey.ExpectedFingerprint = strings.Repeat("B", 40)
	if _, _, err := s.SetPGPDeviceEnvelope(id, wrongKey); !errors.Is(err, ErrPGPIdentityChanged) {
		t.Fatalf("stale fingerprint: %v", err)
	}
	if _, err := s.SetPGPWrappedEnvelope(id, EnvelopeSlotDevicePrefix+"phone", deviceEnvelope(2), "now", "", nil); !errors.Is(err, ErrInvalidEnvelopeSlot) {
		t.Fatalf("the generic slot writer still takes device slots: %v", err)
	}

	// Conversion clears device slots; afterwards only v3 at the current
	// generation and revision is accepted.
	u, err = s.CommitPGPKeyring(context.Background(), id, update)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range u.WrappedEnvelopes() {
		if strings.HasPrefix(e.Slot, EnvelopeSlotDevicePrefix) {
			t.Fatalf("conversion kept a device slot: %+v", e)
		}
	}
	rev := u.PGPRevision
	converted := func(version int, generation uint64) DeviceDelivery {
		d := delivery(version)
		d.ExpectedGeneration, d.ExpectedRevision = generation, &rev
		return d
	}
	if _, _, err := s.SetPGPDeviceEnvelope(id, converted(2, 1)); !errors.Is(err, ErrDeviceEnvelopeVersion) {
		t.Fatalf("v2 on a converted account: %v", err)
	}
	if _, _, err := s.SetPGPDeviceEnvelope(id, converted(3, 0)); !errors.Is(err, ErrPGPGenerationChanged) {
		t.Fatalf("stale generation: %v", err)
	}
	noRevision := converted(3, 1)
	noRevision.ExpectedRevision = nil
	if _, _, err := s.SetPGPDeviceEnvelope(id, noRevision); !errors.Is(err, ErrPGPKeyringUpgradeRequired) {
		t.Fatalf("no revision on a converted account: %v", err)
	}
	stored, version, err = s.SetPGPDeviceEnvelope(id, converted(3, 1))
	if err != nil || version != 3 {
		t.Fatalf("v3 delivery: %d %v", version, err)
	}
	for _, e := range stored.WrappedEnvelopes() {
		if e.Slot == EnvelopeSlotDevicePrefix+"phone" && (e.Version != 3 || e.MaterialGeneration != 1 || e.EnrollmentKey != "DEVKEY") {
			t.Fatalf("recorded v3 slot = %+v", e)
		}
	}
	// A device delivery does not advance the revision, so the same snapshot
	// can re-deliver (a retry), replacing the slot rather than adding one.
	again, _, err := s.SetPGPDeviceEnvelope(id, converted(3, 1))
	if err != nil {
		t.Fatalf("re-delivery: %v", err)
	}
	n := 0
	for _, e := range again.WrappedEnvelopes() {
		if strings.HasPrefix(e.Slot, EnvelopeSlotDevicePrefix) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("device slots after re-delivery = %d, want 1", n)
	}
}
