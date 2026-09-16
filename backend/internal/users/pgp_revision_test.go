package users

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestPGPRevisionRejectsStaleWritersAfterSameKeyChange(t *testing.T) {
	s, id := newClientProtectedUser(t)
	before, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.RewrapPGPPrivateKey(id, `{"v":2,"new":true}`, before.PGPFingerprint, &before.PGPRevision)
	if err != nil {
		t.Fatal(err)
	}
	if current.PGPRevision != before.PGPRevision+1 || current.PGPFingerprint != before.PGPFingerprint {
		t.Fatal("same-key change did not advance revision")
	}
	original, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	writes := []struct {
		name string
		run  func() (User, error)
	}{
		{"identity", func() (User, error) {
			return s.SetPGPIdentityClientProtected(id, "FPR", "KID", "PUBLIC", `{"v":2}`, "imported", "now", &before.PGPRevision)
		}},
		{"rewrap", func() (User, error) { return s.RewrapPGPPrivateKey(id, `{"v":2}`, "FPR", &before.PGPRevision) }},
		{"recovery", func() (User, error) {
			return s.SetPGPWrappedEnvelope(id, EnvelopeSlotRecovery, `{"v":2}`, "now", "FPR", &before.PGPRevision)
		}},
		{"device", func() (User, error) {
			out, _, err := s.SetPGPDeviceEnvelope(id, DeviceDelivery{DeviceID: "test", Envelope: deviceEnvelope(2), AddedAt: "now", EnrollmentKey: "K", ExpectedFingerprint: "FPR", ExpectedRevision: &before.PGPRevision})
			return out, err
		}},
		{"slot-delete", func() (User, error) { return s.DeletePGPWrappedEnvelope(id, EnvelopeSlotRecovery, &before.PGPRevision) }},
		{"identity-delete", func() (User, error) { return s.ClearPGPIdentity(id, &before.PGPRevision) }},
		{"password-and-envelope", func() (User, error) {
			return s.SetDerivedAuthAndRewrapPGP(context.Background(), id, strings.Repeat("a", 64), base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")), 600000, false, `{"v":2}`, &before.PGPRevision)
		}},
		{"admin-reset", func() (User, error) {
			return s.SetPassword(context.Background(), id, "admin-reset-test-value", true, &before.PGPRevision)
		}},
		{"password", func() (User, error) {
			return s.SetPassword(context.Background(), id, "new-password-test-value", false, &before.PGPRevision)
		}},
	}
	for _, test := range writes {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.run(); !errors.Is(err, ErrPGPRevisionChanged) {
				t.Fatalf("got %v", err)
			}
			after, err := os.ReadFile(s.path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original, after) {
				t.Fatal("stale request changed persisted state")
			}
		})
	}
}

func TestPGPRevisionTracksRecoveryAndCredentialButNotDevices(t *testing.T) {
	s, id := newClientProtectedUser(t)
	u, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	initial := u.PGPRevision
	for _, device := range []string{"one", "two"} {
		u, _, err = s.SetPGPDeviceEnvelope(id, DeviceDelivery{DeviceID: device, Envelope: deviceEnvelope(2), AddedAt: "now", EnrollmentKey: "K", ExpectedFingerprint: "FPR", ExpectedRevision: &initial})
		if err != nil || u.PGPRevision != initial {
			t.Fatalf("independent device write: %v, revision %d", err, u.PGPRevision)
		}
	}
	u, err = s.SetPGPWrappedEnvelope(id, EnvelopeSlotRecovery, `{"v":2,"recovery":1}`, "now", "FPR", &initial)
	if err != nil || u.PGPRevision != initial+1 {
		t.Fatalf("recovery creation: %v", err)
	}
	prior := u.PGPRevision
	u, err = s.SetPGPWrappedEnvelope(id, EnvelopeSlotRecovery, `{"v":2,"recovery":2}`, "now", "FPR", &prior)
	if err != nil || u.PGPRevision != prior+1 {
		t.Fatalf("in-place recovery replacement: %v", err)
	}
	prior = u.PGPRevision
	u, err = s.SetPassword(context.Background(), id, "admin-reset-test-password", true, nil)
	if err != nil || u.PGPRevision != prior+1 {
		t.Fatalf("admin reset: %v", err)
	}
	if _, err = s.RewrapPGPPrivateKey(id, `{"v":2}`, "FPR", &prior); !errors.Is(err, ErrPGPRevisionChanged) {
		t.Fatalf("reset allowed stale rewrap: %v", err)
	}
	prior = u.PGPRevision
	u, err = s.DeletePGPWrappedEnvelope(id, EnvelopeSlotRecovery, &prior)
	if err != nil || u.PGPRevision != prior+1 {
		t.Fatalf("recovery removal: %v", err)
	}
	prior = u.PGPRevision
	u, err = s.DeletePGPWrappedEnvelope(id, EnvelopeSlotRecovery, &prior)
	if err != nil || u.PGPRevision != prior {
		t.Fatalf("absent recovery removal: %v", err)
	}
	u, err = s.ClearPGPIdentity(id, &prior)
	if err != nil || u.PGPRevision != prior+1 {
		t.Fatalf("identity deletion: %v", err)
	}
}

func TestPGPRevisionCASAcrossStoreInstances(t *testing.T) {
	s, id := newClientProtectedUser(t)
	other, err := LoadOrMigrate(context.Background(), filepath.Dir(s.path), filepath.Join(filepath.Dir(s.path), "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for index, store := range []*Store{s, other} {
		wg.Go(func() {
			<-start
			_, err := store.RewrapPGPPrivateKey(id, `{"v":2,"writer":`+string(rune('0'+index))+`}`, "FPR", &u.PGPRevision)
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	won, lost := 0, 0
	for err := range results {
		if err == nil {
			won++
		} else if errors.Is(err, ErrPGPRevisionChanged) {
			lost++
		} else {
			t.Fatal(err)
		}
	}
	if won != 1 || lost != 1 {
		t.Fatalf("writers: %d won, %d conflicted", won, lost)
	}
}

func TestPGPRevisionZeroAndExhaustion(t *testing.T) {
	s, id := newClientProtectedUser(t)
	zero := uint64(0)
	if _, err := s.ClearPGPIdentity(id, &zero); !errors.Is(err, ErrPGPRevisionChanged) {
		t.Fatalf("zero skipped guard: %v", err)
	}
	_, err := s.mutate(id, func(u *User) error { u.PGPRevision = MaxPGPRevision; return nil })
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RewrapPGPPrivateKey(id, `{"v":2,"overflow":true}`, "FPR", nil); err == nil {
		t.Fatal("revision wrapped")
	}
	after, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("exhausted revision changed data")
	}
}

func TestPGPRevisionFailedWritePreservesCredentialAndEnvelope(t *testing.T) {
	s, id := newClientProtectedUser(t)
	path := s.path
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(t.TempDir(), "directory-not-file")
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	_, err = s.mutatePGP(id, nil, func(u *User) error {
		u.PasswordHash = "replacement-hash"
		u.PGPPrivateKeyWrapped = `{"v":2,"replacement":true}`
		// Fail the common sink after mutation, without changing the durable input.
		s.path = blocked
		return nil
	})
	s.path = path
	if err == nil {
		t.Fatal("write unexpectedly succeeded")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed write committed part of the credential/envelope pair")
	}
}
