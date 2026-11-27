package state

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
)

func TestDeviceOpenPGPPostgresUpgrade(t *testing.T) {
	dbbackend.RequirePostgres(t)
	dir := t.TempDir()
	old := strings.Replace(pgSchema, `ALTER TABLE native_devices ADD COLUMN IF NOT EXISTS openpgp TEXT NOT NULL DEFAULT '["v4"]';`, "", 1)
	db, err := dbbackend.Open(filepath.Join(dir, "state.db"), dbbackend.Options{Kind: Kind, DDL: old, Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(legacyDeviceSeedSQL); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d, ok := s.GetNativeDevice("legacy-dev")
	if !ok || !slices.Equal(d.OpenPGP, []string{"v4"}) {
		t.Fatal("upgrade lost device or overstated codecs", d)
	}
}

func TestDeviceOpenPGPPersistsAcrossReopenAndRegistration(t *testing.T) {
	s := enrollmentTestStore(t)
	seedDevice(t, s, "dev")
	if d, _ := s.GetNativeDevice("dev"); !slices.Equal(d.OpenPGP, []string{"v4"}) {
		t.Fatal(d)
	}
	d, err := s.SetNativeDeviceEnrollmentKey("dev", "KEY", "now", nil, []string{" RFC9980 ", "v6", "V4", "v6"})
	if err != nil || !slices.Equal(d.OpenPGP, []string{"v4", "v6", "rfc9980"}) {
		t.Fatal(d, err)
	}
	for _, id := range []string{"dev", "replacement"} {
		if err := s.UpsertNativeDevice(NativeDevice{DeviceID: id, Platform: "android", PushToken: "tok-dev", OpenPGP: []string{"v4"}}); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := New(s.baseDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	d, ok := reopened.GetNativeDevice("dev")
	if !ok || !slices.Equal(d.OpenPGP, []string{"v4", "v6", "rfc9980"}) {
		t.Fatal("registration lost report", d)
	}
	// Capability is independent of key custody and survives an identity reset.
	if _, err := s.ClearDeviceEnrollments(); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetNativeDevice("dev")
	if !slices.Equal(d.OpenPGP, []string{"v4", "v6", "rfc9980"}) {
		t.Fatal(d)
	}
	if err := s.SetNativeDeviceEncryptionEnrolled("dev", false, nil); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetNativeDevice("dev")
	if !slices.Equal(d.OpenPGP, []string{"v4"}) {
		t.Fatal("legacy report retained newer support", d)
	}
}

func TestDeviceOpenPGPInvalidReportsDoNotMutate(t *testing.T) {
	s := enrollmentTestStore(t)
	seedDevice(t, s, "dev")
	for _, caps := range [][]string{{}, {"v5"}, {""}, {"v4", "9980"}, make([]string, 17)} {
		if _, err := s.SetNativeDeviceEnrollmentKey("dev", "NEW", "now", nil, caps); !errors.Is(err, ErrInvalidOpenPGP) {
			t.Fatal(caps, err)
		}
		if err := s.SetNativeDeviceEncryptionEnrolled("dev", true, caps); !errors.Is(err, ErrInvalidOpenPGP) {
			t.Fatal(caps, err)
		}
		d, _ := s.GetNativeDevice("dev")
		if d.EnrollmentPublicKey != "" || d.EncryptionEnrolled || !slices.Equal(d.OpenPGP, []string{"v4"}) {
			t.Fatal(d)
		}
	}
	if _, err := s.db.Exec(`UPDATE native_devices SET openpgp = '["invented"]'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListNativeDevicesStrict(); err == nil {
		t.Fatal("corrupt report became a readiness claim")
	}
}

func TestDeviceOpenPGPAcknowledgementIsAtomic(t *testing.T) {
	s := enrollmentTestStore(t)
	seedDevice(t, s, "dev")
	delivery := DeviceEnrollment{Version: 6, Generation: 3, Fingerprint: strings.Repeat("A", 64)}
	if err := s.RecordNativeDeviceDelivery("dev", delivery); err != nil {
		t.Fatal(err)
	}
	caps := []string{"v4", "v6", "rfc9980"}
	if ok, err := s.ConfirmNativeDeviceEnrollment("dev", delivery, caps); err != nil || !ok {
		t.Fatal(ok, err)
	}
	delivery.Generation++
	if ok, err := s.ConfirmNativeDeviceEnrollment("dev", delivery, []string{"v4"}); err != nil || ok {
		t.Fatal(ok, err)
	}
	d, _ := s.GetNativeDevice("dev")
	if !d.EncryptionEnrolled || !slices.Equal(d.OpenPGP, caps) {
		t.Fatal("failed acknowledgement changed codec report", d)
	}
}
