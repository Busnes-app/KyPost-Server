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
	// Delivery writes the record unconfirmed; only the exact record confirms.
	if err := store.RecordNativeDeviceDelivery("dev-1", DeviceEnrollment{Version: 3, Generation: 7, Fingerprint: "AAAA1111"}); err != nil {
		t.Fatal(err)
	}
	d, ok := store.GetNativeDevice("dev-1")
	if !ok || d.EncryptionEnrolled || d.EnrolledVersion != 3 || d.EnrolledGeneration != 7 || d.EnrolledFingerprint != "AAAA1111" {
		t.Fatalf("after delivery: %+v", d)
	}
	for name, wrong := range map[string]DeviceEnrollment{
		"generation":  {Version: 3, Generation: 8, Fingerprint: "AAAA1111"},
		"version":     {Version: 2, Generation: 7, Fingerprint: "AAAA1111"},
		"fingerprint": {Version: 3, Generation: 7, Fingerprint: "BBBB2222"},
		"empty":       {},
	} {
		if ok, err := store.ConfirmNativeDeviceEnrollment("dev-1", wrong); ok || err != nil {
			t.Fatalf("%s: confirmed a delivery that never happened: %v %v", name, ok, err)
		}
	}
	if d, _ = store.GetNativeDevice("dev-1"); d.EncryptionEnrolled {
		t.Fatal("a refused confirmation set the marker")
	}
	if ok, err := store.ConfirmNativeDeviceEnrollment("dev-1", DeviceEnrollment{Version: 3, Generation: 7, Fingerprint: "AAAA1111"}); !ok || err != nil {
		t.Fatalf("confirm: %v %v", ok, err)
	}
	if d, _ = store.GetNativeDevice("dev-1"); !d.EncryptionEnrolled || d.EnrolledGeneration != 7 {
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

	if err := store.RecordNativeDeviceDelivery("dev-1", DeviceEnrollment{Version: 3, Generation: 8, Fingerprint: "AAAA1111"}); err != nil {
		t.Fatal(err)
	}
	if n, err := store.ClearDeviceEnrollments(); err != nil || n != 1 {
		t.Fatalf("clear: %d %v", n, err)
	}
	if d, _ = store.GetNativeDevice("dev-1"); d.EnrolledGeneration != 0 || d.EnrolledVersion != 0 || d.EnrolledFingerprint != "" {
		t.Fatalf("rotation kept the acknowledgement: %+v", d)
	}
	if err := store.RecordNativeDeviceDelivery("nobody", DeviceEnrollment{Version: 3, Generation: 1, Fingerprint: "AAAA1111"}); err == nil {
		t.Fatal("delivery to an unknown device succeeded")
	}

	// Registration can never assert an acknowledgement, on a new row either.
	if err := store.UpsertNativeDevice(NativeDevice{DeviceID: "dev-new", Platform: "android", PushToken: "tok-new", EncryptionEnrolled: true, EnrolledVersion: 3, EnrolledGeneration: 9, EnrolledFingerprint: "AAAA1111"}); err != nil {
		t.Fatal(err)
	}
	if d, _ = store.GetNativeDevice("dev-new"); d.EncryptionEnrolled || d.EnrolledVersion != 0 || d.EnrolledGeneration != 0 || d.EnrolledFingerprint != "" {
		t.Fatalf("registration asserted an acknowledgement: %+v", d)
	}
}
