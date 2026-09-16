package state

import "testing"

// A generation-aware acknowledgement is what a device holds, recorded with
// the enrolled marker; saying "not enrolled" forgets it, and an identity
// rotation forgets it for every device.
func TestDeviceAcknowledgementPersistsAndClears(t *testing.T) {
	store := enrollmentTestStore(t)
	seedDevice(t, store, "dev-1")
	if _, err := store.SetNativeDeviceEnrollmentKey("dev-1", "PUBKEY", "2026-09-16T00:00:00Z", []int{2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetNativeDeviceEnrollment("dev-1", DeviceEnrollment{Version: 3, Generation: 7, Fingerprint: "AAAA1111"}); err != nil {
		t.Fatal(err)
	}
	d, ok := store.GetNativeDevice("dev-1")
	if !ok || !d.EncryptionEnrolled || d.EnrolledVersion != 3 || d.EnrolledGeneration != 7 || d.EnrolledFingerprint != "AAAA1111" {
		t.Fatalf("after acknowledgement: %+v", d)
	}

	// Registration re-upserts the row; the acknowledgement rides along.
	d.AppVersion = "2.0"
	if err := store.UpsertNativeDevice(NativeDevice{DeviceID: "dev-1", Platform: d.Platform, PushToken: d.PushToken, AppVersion: "2.0"}); err != nil {
		t.Fatal(err)
	}
	if d, _ = store.GetNativeDevice("dev-1"); d.EnrolledGeneration != 7 || d.EnrolledVersion != 3 || d.EnrolledFingerprint != "AAAA1111" || d.AppVersion != "2.0" {
		t.Fatalf("re-registration lost the acknowledgement: %+v", d)
	}

	// A bare "still enrolled" keeps it; "not enrolled" forgets it.
	if err := store.SetNativeDeviceEncryptionEnrolled("dev-1", true); err != nil {
		t.Fatal(err)
	}
	if d, _ = store.GetNativeDevice("dev-1"); d.EnrolledGeneration != 7 {
		t.Fatalf("restating enrolled dropped the generation: %+v", d)
	}
	if err := store.SetNativeDeviceEncryptionEnrolled("dev-1", false); err != nil {
		t.Fatal(err)
	}
	if d, _ = store.GetNativeDevice("dev-1"); d.EncryptionEnrolled || d.EnrolledGeneration != 0 || d.EnrolledVersion != 0 || d.EnrolledFingerprint != "" {
		t.Fatalf("un-enrolling kept the acknowledgement: %+v", d)
	}

	if err := store.SetNativeDeviceEnrollment("dev-1", DeviceEnrollment{Version: 3, Generation: 8, Fingerprint: "AAAA1111"}); err != nil {
		t.Fatal(err)
	}
	if n, err := store.ClearDeviceEnrollments(); err != nil || n != 1 {
		t.Fatalf("clear: %d %v", n, err)
	}
	if d, _ = store.GetNativeDevice("dev-1"); d.EnrolledGeneration != 0 || d.EnrolledVersion != 0 || d.EnrolledFingerprint != "" {
		t.Fatalf("rotation kept the acknowledgement: %+v", d)
	}
	if err := store.SetNativeDeviceEnrollment("nobody", DeviceEnrollment{Version: 3, Generation: 1}); err == nil {
		t.Fatal("acknowledgement for an unknown device succeeded")
	}
}
