package users

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Busnes-app/ky-primitives/xwing"
)

func xwingEnvelopeForTest(t *testing.T) (string, string) {
	t.Helper()
	data, err := os.ReadFile("../../../testdata/device-envelope-xwing.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		PublicKey string
		Envelope  json.RawMessage
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return string(f.Envelope), f.PublicKey
}

func TestV6DeviceEnvelopeStrictFraming(t *testing.T) {
	envelope, key := xwingEnvelopeForTest(t)
	if v, err := ParseDeviceEnvelopeVersion(envelope); err != nil || v != 6 {
		t.Fatalf("v6: %d %v", v, err)
	}
	var e map[string]any
	if err := json.Unmarshal([]byte(envelope), &e); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 65, 1088, 1119, 1121, 1216} {
		e["enc"] = base64.StdEncoding.EncodeToString(make([]byte, size))
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseDeviceEnvelopeVersion(string(raw)); !errors.Is(err, ErrInvalidDeviceEnvelope) {
			t.Fatalf("accepted enc size %d: %v", size, err)
		}
	}
	if err := ValidateXWingEnrollmentKey(key); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"KEY", strings.TrimRight(key, "="), base64.StdEncoding.EncodeToString(make([]byte, 65)), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{255}, xwing.PublicKeyBytes))} {
		if err := ValidateXWingEnrollmentKey(bad); err == nil {
			t.Fatal("accepted invalid X-Wing key")
		}
	}
}

func TestV6DeviceDeliveryValidatesKeyAndKeepsGeneration(t *testing.T) {
	envelope, key := xwingEnvelopeForTest(t)
	s, id, update := keyringCandidate(t)
	owner, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	d := DeviceDelivery{DeviceID: "phone", Envelope: envelope, EnrollmentKey: key, ExpectedFingerprint: owner.PGPFingerprint, ExpectedRevision: &owner.PGPRevision}
	// Legacy and converted accounts both support v6; the recorded generation
	// determines whether the plaintext is one key or the complete ring.
	for _, converted := range []bool{false, true} {
		if converted {
			update.ExpectedRevision = owner.PGPRevision
			owner, err = s.CommitPGPKeyring(context.Background(), id, update)
			if err != nil {
				t.Fatal(err)
			}
			d.ExpectedGeneration = owner.PGPKeyring.MaterialGeneration
			d.ExpectedRevision = &owner.PGPRevision
		}
		bad := d
		bad.EnrollmentKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{255}, 1216))
		if _, _, err := s.SetPGPDeviceEnvelope(id, bad); !errors.Is(err, ErrInvalidDeviceEnvelope) {
			t.Fatalf("invalid device key: %v", err)
		}
		got, v, err := s.SetPGPDeviceEnvelope(id, d)
		if err != nil || v != 6 {
			t.Fatalf("delivery: %d %v", v, err)
		}
		for _, e := range got.WrappedEnvelopes() {
			if e.Slot == "device:phone" && (e.Version != 6 || e.MaterialGeneration != d.ExpectedGeneration || e.EnrollmentKey != key) {
				t.Fatal("lost binding")
			}
		}
		owner = got
		d.ExpectedRevision = &owner.PGPRevision
	}
}
