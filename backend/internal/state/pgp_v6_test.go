package state

import "testing"

func TestPGPV6ReadyRequiresEveryDeviceCodecAndCurrentPQDelivery(t *testing.T) {
	s := enrollmentTestStore(t)
	const fp = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	check := func(want bool) {
		t.Helper()
		ready, err := s.PGPV6Ready(2, fp)
		if err != nil || ready != want {
			t.Fatalf("ready=%v want=%v: %v", ready, want, err)
		}
	}
	check(true) // browser-only account
	seedDevice(t, s, "linux")
	seedDevice(t, s, "phone")
	check(false)
	for _, id := range []string{"linux", "phone"} {
		if _, err := s.SetNativeDeviceEnrollmentKey(id, "KEY", "now", []int{6}, []string{"v4", "v6", "rfc9980"}); err != nil {
			t.Fatal(err)
		}
		check(false) // codecs alone never prove the new private key was imported
		delivery := DeviceEnrollment{Version: 6, Generation: 2, Fingerprint: fp}
		if err := s.RecordNativeDeviceDelivery(id, delivery); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.ConfirmNativeDeviceEnrollment(id, delivery, []string{"v4", "v6", "rfc9980"}); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	check(true)
	if err := s.SetNativeDeviceEncryptionEnrolled("linux", true, []string{"v4"}); err != nil {
		t.Fatal(err)
	}
	check(false)
	// A previously capable device, reset or at an old generation, blocks again.
	if _, err := s.SetNativeDeviceEnrollmentKey("linux", "KEY", "now", []int{6}, []string{"v4", "v6", "rfc9980"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordNativeDeviceDelivery("linux", DeviceEnrollment{Version: 6, Generation: 1, Fingerprint: fp}); err != nil {
		t.Fatal(err)
	}
	check(false)
}
