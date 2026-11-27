package users

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
)

func TestPQCConversionPreservesV4AndHistory(t *testing.T) {
	s, id, update := keyringCandidate(t)
	before, _ := s.Get(id)
	public, err := pgpmailtest.GenerateV6PublicKey("Ring", "ring@example.invalid", true)
	if err != nil {
		t.Fatal(err)
	}
	info, err := pgpmail.InspectPQCKey(public)
	if err != nil {
		t.Fatal(err)
	}
	update.PublicKeyV6 = public
	update.PrimaryFingerprints = append(update.PrimaryFingerprints, info.Fingerprint)
	update.KeyFingerprints = append(update.KeyFingerprints, info.KeyFingerprints...)
	bad := update
	bad.RecoveryEnvelope = ""
	if _, err := s.CommitPGPKeyring(context.Background(), id, bad); err == nil {
		t.Fatal("missing recovery committed")
	}
	u, err := s.CommitPGPKeyring(context.Background(), id, update)
	if err != nil {
		t.Fatal(err)
	}
	if u.PGPPublicKey != before.PGPPublicKey || u.PGPFingerprint != before.PGPFingerprint || u.PGPKeyID != before.PGPKeyID ||
		u.PasswordHash != before.PasswordHash || u.PGPKeyring.Version != 2 || u.PGPFingerprintV6 != info.Fingerprint ||
		u.PGPKeyring.MaterialGeneration != 1 || u.PGPRevision != before.PGPRevision+1 || u.PGPKeyCreatedAtV6 == "" {
		t.Fatal("conversion lost identity, credential or generation")
	}
	if u.PGPPrivateKeyWrapped != update.PasswordEnvelope || u.PGPWrappedEnvelopes[0].Envelope != update.RecoveryEnvelope {
		t.Fatal("partial envelope commit")
	}
	if _, err := s.CommitPGPKeyring(context.Background(), id, update); !errors.Is(err, ErrPGPRevisionChanged) {
		t.Fatal("stale conversion", err)
	}
	update.ExpectedRevision = u.PGPRevision
	if _, err := s.CommitPGPKeyring(context.Background(), id, update); !errors.Is(err, ErrInvalidPGPKeyring) {
		t.Fatal("overwrote v6", err)
	}
	update.PublicKeyV6 = ""
	update.KeyFingerprints = update.KeyFingerprints[:2]
	if _, err := s.CommitPGPKeyring(context.Background(), id, update); !errors.Is(err, ErrInvalidPGPKeyring) {
		t.Fatal("lost history", err)
	}
	if _, err := s.SetPGPIdentityClientProtected(id, before.PGPFingerprint, before.PGPKeyID, before.PGPPublicKey, "single", "generated", "now", &u.PGPRevision); !errors.Is(err, ErrPGPKeyringUpgradeRequired) {
		t.Fatal("legacy writer", err)
	}
	after, _ := s.Get(id)
	if !reflect.DeepEqual(after, u) {
		t.Fatal("rejected writes changed data")
	}
	deleted, err := s.ClearPGPIdentity(id, &u.PGPRevision)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.PGPPublicKeyV6 != "" || deleted.PGPFingerprintV6 != "" {
		t.Fatal("delete retained public PQ key")
	}
}

func TestPQCConversionRequiresProfileAndCompleteInventory(t *testing.T) {
	for _, pq := range []bool{false, true} {
		s, id, update := keyringCandidate(t)
		public, err := pgpmailtest.GenerateV6PublicKey("Ring", "ring@example.invalid", pq)
		if err != nil {
			t.Fatal(err)
		}
		update.PublicKeyV6 = public
		if _, err := s.CommitPGPKeyring(context.Background(), id, update); !errors.Is(err, ErrInvalidPGPKeyring) {
			t.Fatal("accepted classical v6 or incomplete inventory", err)
		}
	}
	s, id, update := keyringCandidate(t)
	update.PasswordEnvelope = strings.Repeat("x", MaxWrappedEnvelopeBytes+1)
	if _, err := s.CommitPGPKeyring(context.Background(), id, update); err == nil {
		t.Fatal("capacity")
	}
}
