package state

import (
	"errors"
	"reflect"
	"slices"
	"testing"
)

func TestEnrollmentCapabilitiesPersistAndRemainDeviceControlled(t *testing.T) {
	s := enrollmentTestStore(t)
	seedDevice(t, s, "dev-1")
	d, _ := s.GetNativeDevice("dev-1")
	if !slices.Equal(d.EnrollmentEnvelopeVersions, []int{2}) {
		t.Fatal("legacy default missing")
	}
	if _, err := s.SetNativeDeviceEnrollmentKey("dev-1", "KEY", "published", []int{4, 3, 2}); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(s.baseDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	d, _ = reopened.GetNativeDevice("dev-1")
	if !slices.Equal(d.EnrollmentEnvelopeVersions, []int{2, 3, 4}) || d.EnrollmentPublicKey != "KEY" {
		t.Fatal("key/capabilities not durable", d)
	}
	for _, incoming := range []NativeDevice{
		{DeviceID: "dev-1", Platform: "android", PushToken: "rotated", EnrollmentEnvelopeVersions: []int{3}},
		{DeviceID: "different", Platform: "android", PushToken: "rotated", EnrollmentEnvelopeVersions: []int{3}},
	} {
		if err := s.UpsertNativeDevice(incoming); err != nil {
			t.Fatal(err)
		}
		d, _ = s.GetNativeDevice("dev-1")
		if !slices.Equal(d.EnrollmentEnvelopeVersions, []int{2, 3, 4}) {
			t.Fatal("registration changed capabilities", d)
		}
	}
	if err := s.UpsertNativeDevice(NativeDevice{DeviceID: "forged", EnrollmentEnvelopeVersions: []int{3}}); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetNativeDevice("forged")
	if !slices.Equal(d.EnrollmentEnvelopeVersions, []int{2}) {
		t.Fatal("new registration forged capabilities")
	}
	// Older client re-publication must not retain a previous v3 claim, even for the same key.
	d, err = s.SetNativeDeviceEnrollmentKey("dev-1", "KEY", "later", nil)
	if err != nil || !slices.Equal(d.EnrollmentEnvelopeVersions, []int{2}) {
		t.Fatal("stale claim survived old client", err)
	}
	if _, err := s.SetNativeDeviceEnrollmentKey("dev-1", "NEWKEY", "later", []int{3}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClearDeviceEnrollments(); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetNativeDevice("dev-1")
	if d.EnrollmentPublicKey != "" || !slices.Equal(d.EnrollmentEnvelopeVersions, []int{2}) {
		t.Fatal("reset retained capability claim", d)
	}
}

func TestInvalidEnrollmentCapabilitiesCannotChangeKey(t *testing.T) {
	s := enrollmentTestStore(t)
	seedDevice(t, s, "dev")
	before, err := s.SetNativeDeviceEnrollmentKey("dev", "KEY", "published", []int{2, 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, versions := range [][]int{{}, {1}, {0}, {-1}, {65536}, {2, 2}, {2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18}} {
		if _, err := s.SetNativeDeviceEnrollmentKey("dev", "ATTACKER", "changed", versions); !errors.Is(err, ErrInvalidEnrollmentVersions) {
			t.Fatalf("accepted %v: %v", versions, err)
		}
		after, _ := s.GetNativeDevice("dev")
		if !reflect.DeepEqual(before, after) {
			t.Fatal("invalid claim changed stored key")
		}
	}
	if _, err := s.db.Exec(`UPDATE native_devices SET enrollment_envelope_versions = '[2,2]'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListNativeDevicesStrict(); err == nil {
		t.Fatal("corrupt capability metadata read as valid")
	}
}
