package users

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/ProtonMail/gopenpgp/v3/crypto"
)

func keyringCandidate(t *testing.T) (*Store, string, PGPKeyringUpdate) {
	t.Helper()
	s, id := newClientProtectedUser(t)
	identity, err := pgpmail.GenerateIdentity("Keyring test", "ring@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	info, err := pgpmail.InspectPublicKey(identity.ArmoredPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPGPIdentityClientProtected(id, info.Fingerprint, info.KeyID, info.ArmoredPublicKey, `{"v":2,"password":"old"}`, "generated", "now", nil); err != nil {
		t.Fatal(err)
	}
	u, err := s.SetDerivedAuth(context.Background(), id, strings.Repeat("a", 64), base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")), 600000, false)
	if err != nil {
		t.Fatal(err)
	}
	return s, id, PGPKeyringUpdate{
		ExpectedRevision: u.PGPRevision, MaterialGeneration: 1,
		PrimaryFingerprints: []string{info.Fingerprint}, KeyFingerprints: info.KeyFingerprints,
		PublicKey: info.ArmoredPublicKey, PasswordEnvelope: `{"v":2,"password":"ring"}`,
		RecoveryEnvelope: `{"v":2,"recovery":"ring"}`, Source: "generated",
	}
}

func TestPGPKeyringAtomicConversionRetirementAndPassword(t *testing.T) {
	s, id, update := keyringCandidate(t)
	if _, err := s.SetPGPWrappedEnvelope(id, "device:old", `{"v":2}`, "now", "", &update.ExpectedRevision); err != nil {
		t.Fatal(err)
	}
	converted, err := s.CommitPGPKeyring(context.Background(), id, update)
	if err != nil {
		t.Fatal(err)
	}
	if converted.PGPRevision != update.ExpectedRevision+1 || converted.PGPKeyring.MaterialGeneration != 1 || len(converted.PGPWrappedEnvelopes) != 1 || converted.PGPWrappedEnvelopes[0].Envelope != update.RecoveryEnvelope {
		t.Fatal("conversion was not complete/atomic or retained device delivery")
	}
	// Cache reads must not expose aliases to either inventory slice.
	cached, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	cached.PGPKeyring.KeyFingerprints[0] = "CORRUPT"
	cached.PGPKeyring.PrimaryFingerprints[0] = "CORRUPT"
	fresh, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(fresh.PGPKeyring.KeyFingerprints, "CORRUPT") || slices.Contains(fresh.PGPKeyring.PrimaryFingerprints, "CORRUPT") {
		t.Fatal("inventory aliases cache")
	}
	oldPublic := update.PublicKey
	oldInventory := slices.Clone(converted.PGPKeyring.KeyFingerprints)
	next, err := pgpmail.GenerateIdentity("Replacement", "ring@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	info, err := pgpmail.InspectPublicKey(next.ArmoredPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	update.ExpectedRevision = converted.PGPRevision
	update.MaterialGeneration = 2
	update.PublicKey = info.ArmoredPublicKey
	update.PrimaryFingerprints = append(slices.Clone(converted.PGPKeyring.PrimaryFingerprints), info.Fingerprint)
	update.KeyFingerprints = append(slices.Clone(oldInventory), info.KeyFingerprints...)
	update.PasswordEnvelope = `{"v":2,"password":"retired-ring"}`
	update.RecoveryEnvelope = `{"v":2,"recovery":"retired-ring"}`
	retired, err := s.CommitPGPKeyring(context.Background(), id, update)
	if err != nil {
		t.Fatal(err)
	}
	if retired.PGPKeyring.MaterialGeneration != 2 || !containsFingerprints(retired.PGPKeyring.KeyFingerprints, oldInventory) || retired.PGPPrivateKeyWrapped != update.PasswordEnvelope || retired.PGPWrappedEnvelopes[0].Envelope != update.RecoveryEnvelope {
		t.Fatal("retirement lost history or a sealing")
	}
	update.ExpectedRevision = retired.PGPRevision
	update.PasswordEnvelope = `{"v":2,"password":"new-password"}`
	update.Credential = &PGPKeyringCredential{AuthSecret: strings.Repeat("b", 64), LoginSalt: base64.StdEncoding.EncodeToString([]byte("fedcba9876543210")), Iterations: 600000}
	changed, err := s.CommitPGPKeyring(context.Background(), id, update)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := VerifyAuthSecret(context.Background(), changed, update.Credential.AuthSecret)
	if err != nil || !valid || changed.PGPRevision != retired.PGPRevision+1 || changed.PGPKeyring.MaterialGeneration != 2 || changed.PGPPrivateKeyWrapped != update.PasswordEnvelope {
		t.Fatal("credential/ring did not commit together without changing material generation")
	}
	update.ExpectedRevision = changed.PGPRevision
	original, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	bad := update
	bad.PublicKey = oldPublic
	if _, err := s.CommitPGPKeyring(context.Background(), id, bad); !errors.Is(err, ErrInvalidPGPKeyring) {
		t.Fatalf("historical reactivation: %v", err)
	}
	bad = update
	bad.KeyFingerprints = info.KeyFingerprints
	bad.PrimaryFingerprints = []string{info.Fingerprint}
	if _, err := s.CommitPGPKeyring(context.Background(), id, bad); !errors.Is(err, ErrInvalidPGPKeyring) {
		t.Fatalf("history loss: %v", err)
	}
	bad = update
	bad.MaterialGeneration++
	if _, err := s.CommitPGPKeyring(context.Background(), id, bad); !errors.Is(err, ErrInvalidPGPKeyring) {
		t.Fatalf("rewrap changed material generation: %v", err)
	}
	after, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("rejected update changed credentials or ciphertext")
	}
}

func TestPGPKeyringRejectsLegacyWritersAndPreservesReset(t *testing.T) {
	s, id, update := keyringCandidate(t)
	u, err := s.CommitPGPKeyring(context.Background(), id, update)
	if err != nil {
		t.Fatal(err)
	}
	rev := u.PGPRevision
	original, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		run  func() (User, error)
	}{
		{"client-identity", func() (User, error) {
			return s.SetPGPIdentityClientProtected(id, u.PGPFingerprint, u.PGPKeyID, u.PGPPublicKey, `{"v":2}`, "imported", "now", &rev)
		}},
		{"rewrap", func() (User, error) { return s.RewrapPGPPrivateKey(id, `{"v":2}`, u.PGPFingerprint, &rev) }},
		{"recovery", func() (User, error) {
			return s.SetPGPWrappedEnvelope(id, EnvelopeSlotRecovery, `{"v":2}`, "now", u.PGPFingerprint, &rev)
		}},
		{"device-v2", func() (User, error) {
			return s.SetPGPWrappedEnvelope(id, "device:old", `{"v":2}`, "now", u.PGPFingerprint, &rev)
		}},
		{"no-revision-delete", func() (User, error) { return s.ClearPGPIdentity(id, nil) }},
		{"no-revision-reset", func() (User, error) { return s.SetPassword(context.Background(), id, "reset-test-password", true, nil) }},
		{"normal-password-only", func() (User, error) {
			return s.SetPassword(context.Background(), id, "reset-test-password", false, &rev)
		}},
		{"legacy-credential-wrapper", func() (User, error) {
			return s.SetDerivedAuth(context.Background(), id, strings.Repeat("c", 64), base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")), 600000, false)
		}},
		{"credential-only", func() (User, error) {
			return s.SetDerivedAuthAndRewrapPGP(context.Background(), id, strings.Repeat("c", 64), base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")), 600000, false, "", &rev)
		}},
		{"credential-single-key", func() (User, error) {
			return s.SetDerivedAuthAndRewrapPGP(context.Background(), id, strings.Repeat("c", 64), base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")), 600000, false, `{"v":2}`, &rev)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.run(); !errors.Is(err, ErrPGPKeyringUpgradeRequired) {
				t.Fatalf("got %v", err)
			}
		})
	}
	after, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("legacy writer changed data")
	}
	reset, err := s.SetPassword(context.Background(), id, "reset-test-password", true, &rev)
	if err != nil {
		t.Fatal(err)
	}
	if reset.PGPPrivateKeyWrapped != u.PGPPrivateKeyWrapped || reset.PGPWrappedEnvelopes[0].Envelope != u.PGPWrappedEnvelopes[0].Envelope || reset.PGPKeyring.MaterialGeneration != 1 {
		t.Fatal("reset destroyed ring")
	}
	if _, err := s.CommitPGPKeyring(context.Background(), id, update); !errors.Is(err, ErrPGPRevisionChanged) {
		t.Fatalf("reset failed to invalidate prepared write: %v", err)
	}
	resetBytes, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPassword(context.Background(), id, "another-test-password", false, &reset.PGPRevision); !errors.Is(err, ErrPGPKeyringUpgradeRequired) {
		t.Fatalf("legacy forced completion stranded keyring: %v", err)
	}
	unchanged, err := os.ReadFile(s.path)
	if err != nil || !bytes.Equal(resetBytes, unchanged) {
		t.Fatal("rejected forced completion changed material or credential")
	}
	forced, err := s.SetDerivedAuthAndRewrapPGP(context.Background(), id, strings.Repeat("d", 64), base64.StdEncoding.EncodeToString([]byte("fedcba9876543210")), 600000, false, "", &reset.PGPRevision)
	if err != nil {
		t.Fatal(err)
	}
	if forced.MustChangePassword || forced.PGPPrivateKeyWrapped != u.PGPPrivateKeyWrapped || forced.PGPKeyring.MaterialGeneration != 1 {
		t.Fatal("forced change lost ring")
	}
	deleted, err := s.ClearPGPIdentity(id, &forced.PGPRevision)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.PGPKeyring != nil || deleted.PGPPrivateKeyWrapped != "" || len(deleted.PGPWrappedEnvelopes) != 0 || deleted.PGPRevision != forced.PGPRevision+1 {
		t.Fatal("explicit deletion retained stale metadata")
	}
}

func TestPGPKeyringCompetingStoresHaveOneWinner(t *testing.T) {
	s, id, update := keyringCandidate(t)
	other := newStore(s.path)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, store := range []*Store{s, other} {
		wg.Go(func() { <-start; _, err := store.CommitPGPKeyring(context.Background(), id, update); results <- err })
	}
	close(start)
	wg.Wait()
	close(results)
	success, stale := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrPGPRevisionChanged):
			stale++
		default:
			t.Fatal(err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("success=%d stale=%d", success, stale)
	}
}

func TestPGPKeyringInvalidUpdatesPreserveFile(t *testing.T) {
	s, id, update := keyringCandidate(t)
	original, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*PGPKeyringUpdate)
	}{
		{"missing-password", func(u *PGPKeyringUpdate) { u.PasswordEnvelope = "" }},
		{"missing-recovery", func(u *PGPKeyringUpdate) { u.RecoveryEnvelope = "" }},
		{"oversize", func(u *PGPKeyringUpdate) { u.RecoveryEnvelope = strings.Repeat("x", MaxWrappedEnvelopeBytes+1) }},
		{"duplicate-primary", func(u *PGPKeyringUpdate) {
			u.PrimaryFingerprints = append(slices.Clone(u.PrimaryFingerprints), strings.ToUpper(u.PrimaryFingerprints[0]))
		}},
		{"duplicate-packet", func(u *PGPKeyringUpdate) {
			u.KeyFingerprints = append(slices.Clone(u.KeyFingerprints), strings.ToLower(u.KeyFingerprints[0]))
		}},
		{"missing-subkey", func(u *PGPKeyringUpdate) { u.KeyFingerprints = slices.Clone(u.PrimaryFingerprints) }},
		{"subkey-as-primary", func(u *PGPKeyringUpdate) {
			u.PrimaryFingerprints = append(slices.Clone(u.PrimaryFingerprints), u.KeyFingerprints[1])
		}},
		{"zero-generation", func(u *PGPKeyringUpdate) { u.MaterialGeneration = 0 }},
		{"wrong-initial-generation", func(u *PGPKeyringUpdate) { u.MaterialGeneration = 2 }},
		{"invalid-fingerprint", func(u *PGPKeyringUpdate) { u.KeyFingerprints = []string{"not a fingerprint"} }},
		{"too-many-packets", func(u *PGPKeyringUpdate) { u.KeyFingerprints = make([]string, MaxPGPKeyringFingerprints+1) }},
		{"too-many-primaries", func(u *PGPKeyringUpdate) { u.PrimaryFingerprints = make([]string, MaxPGPKeyringKeys+1) }},
		{"unsafe-revision", func(u *PGPKeyringUpdate) { u.ExpectedRevision = MaxPGPRevision + 1 }},
		{"invalid-credential", func(u *PGPKeyringUpdate) { u.Credential = &PGPKeyringCredential{AuthSecret: "invalid"} }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			bad := update
			test.change(&bad)
			if _, err := s.CommitPGPKeyring(context.Background(), id, bad); err == nil {
				t.Fatal("accepted invalid update")
			}
		})
	}
	after, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("invalid update changed persisted data")
	}
}

func TestPGPKeyringPersistenceFailureCommitsNothing(t *testing.T) {
	s, id, update := keyringCandidate(t)
	original, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	// The target and .lock fit NAME_MAX, but AtomicWriteFile's temporary name
	// does not. This fails persistence after the real transaction, even as root.
	s.path = filepath.Join(t.TempDir(), strings.Repeat("u", 246))
	if err := os.WriteFile(s.path, original, 0600); err != nil {
		t.Fatal(err)
	}
	update.Credential = &PGPKeyringCredential{AuthSecret: strings.Repeat("b", 64), LoginSalt: base64.StdEncoding.EncodeToString([]byte("fedcba9876543210")), Iterations: 600000}
	if _, err := s.CommitPGPKeyring(context.Background(), id, update); err == nil || !strings.Contains(err.Error(), "file name too long") {
		t.Fatalf("expected temporary-file failure: %v", err)
	}
	after, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("failed persistence committed credential, keyring, or envelope")
	}
}

func TestPGPKeyringPreservesMultipleUIDPackets(t *testing.T) {
	s, id, update := keyringCandidate(t)
	identity, err := pgpmail.GenerateIdentity("Many UIDs", "one@example.invalid", "two@example.invalid", "three@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	info, err := pgpmail.InspectPublicKey(identity.ArmoredPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.SetPGPIdentityClientProtected(id, info.Fingerprint, info.KeyID, info.ArmoredPublicKey, update.PasswordEnvelope, "generated", "now", &update.ExpectedRevision)
	if err != nil {
		t.Fatal(err)
	}
	update.ExpectedRevision, update.PublicKey = u.PGPRevision, u.PGPPublicKey
	update.PrimaryFingerprints, update.KeyFingerprints = []string{info.Fingerprint}, info.KeyFingerprints
	for range 100 {
		u, err = s.CommitPGPKeyring(context.Background(), id, update)
		if err != nil {
			t.Fatal(err)
		}
		if u.PGPPublicKey != update.PublicKey {
			t.Fatal("same-active packets changed")
		}
		update.ExpectedRevision = u.PGPRevision
	}
}

func TestPGPKeyringRejectsSigningOnlyRetirement(t *testing.T) {
	s, id, update := keyringCandidate(t)
	u, err := s.CommitPGPKeyring(context.Background(), id, update)
	if err != nil {
		t.Fatal(err)
	}
	key, err := crypto.PGP().KeyGeneration().AddUserId("Signing only", "sign@example.invalid").New().GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	key.GetEntity().Subkeys = nil
	public, err := key.GetArmoredPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	info, err := pgpmail.InspectPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	status, err := pgpmail.CheckKeyStatus(public)
	if err != nil || !status.Usable() || info.CanEncrypt {
		t.Fatalf("invalid regression candidate: %+v %v", status, err)
	}
	update.ExpectedRevision, update.MaterialGeneration, update.PublicKey = u.PGPRevision, 2, public
	update.PrimaryFingerprints = append(update.PrimaryFingerprints, info.Fingerprint)
	update.KeyFingerprints = append(update.KeyFingerprints, info.KeyFingerprints...)
	before, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitPGPKeyring(context.Background(), id, update); !errors.Is(err, ErrInvalidPGPKeyring) {
		t.Fatalf("signing-only retirement: %v", err)
	}
	after, err := os.ReadFile(s.path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected retirement changed disk")
	}
}

func TestChangeKeyringPasswordPreservesMaterial(t *testing.T) {
	for _, recovery := range []bool{true, false} {
		t.Run(map[bool]string{true: "with recovery", false: "without recovery"}[recovery], func(t *testing.T) {
			s, id, update := keyringCandidate(t)
			u, err := s.CommitPGPKeyring(context.Background(), id, update)
			if err != nil {
				t.Fatal(err)
			}
			if !recovery {
				u, err = s.DeletePGPWrappedEnvelope(id, EnvelopeSlotRecovery, &u.PGPRevision)
				if err != nil {
					t.Fatal(err)
				}
			}
			u, err = s.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			credential := PGPKeyringCredential{AuthSecret: strings.Repeat("b", 64), LoginSalt: u.LoginSalt, Iterations: 600000}
			for _, revision := range []*uint64{nil, &update.ExpectedRevision} {
				if _, err := s.ChangeKeyringPassword(context.Background(), id, credential, `{"v":2,"new":true}`, revision); err == nil {
					t.Fatal("unguarded/stale change accepted")
				}
			}
			changed, err := s.ChangeKeyringPassword(context.Background(), id, credential, `{"v":2,"new":true}`, &u.PGPRevision)
			if err != nil {
				t.Fatal(err)
			}
			if changed.PGPRevision != u.PGPRevision+1 || changed.PGPPrivateKeyWrapped != `{"v":2,"new":true}` {
				t.Fatal("missing password envelope/revision")
			}
			if ok, err := VerifyAuthSecret(context.Background(), changed, credential.AuthSecret); err != nil || !ok {
				t.Fatal("credential not committed")
			}
			changed.UpdatedAt = u.UpdatedAt // Every store mutation stamps its ordinary audit time.
			changed.PasswordHash, changed.LoginSalt, changed.LoginIterations = u.PasswordHash, u.LoginSalt, u.LoginIterations
			changed.PGPRevision, changed.PGPPrivateKeyWrapped = u.PGPRevision, u.PGPPrivateKeyWrapped
			if !reflect.DeepEqual(changed, u) {
				t.Fatal("password change altered retained material or metadata")
			}
		})
	}
}

func TestChangeKeyringPasswordRefusesWrongState(t *testing.T) {
	s, id, update := keyringCandidate(t)
	credential := PGPKeyringCredential{AuthSecret: strings.Repeat("b", 64), LoginSalt: base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")), Iterations: 600000}
	if _, err := s.ChangeKeyringPassword(context.Background(), id, credential, `{"v":2}`, &update.ExpectedRevision); !errors.Is(err, ErrPGPKeyringUpgradeRequired) {
		t.Fatalf("legacy: %v", err)
	}
	u, err := s.CommitPGPKeyring(context.Background(), id, update)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangeKeyringPassword(context.Background(), id, credential, "", &u.PGPRevision); !errors.Is(err, ErrPGPKeyringUpgradeRequired) {
		t.Fatalf("empty: %v", err)
	}
	before := u
	u, err = s.SetPassword(context.Background(), id, "temporary-ring-password", true, &u.PGPRevision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangeKeyringPassword(context.Background(), id, credential, `{"v":2}`, &u.PGPRevision); !errors.Is(err, ErrPGPKeyringUpgradeRequired) {
		t.Fatalf("forced: %v", err)
	}
	after, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.PGPRevision != u.PGPRevision || after.PasswordHash != u.PasswordHash || after.PGPPrivateKeyWrapped != before.PGPPrivateKeyWrapped {
		t.Fatal("refused change wrote data")
	}
}

func TestRewrapPGPKeyringPreservesCurrentState(t *testing.T) {
	s, id, update := keyringCandidate(t)
	if _, err := s.RewrapPGPKeyring(id, `{"v":2}`, update.PrimaryFingerprints[0], &update.ExpectedRevision); !errors.Is(err, ErrPGPKeyringUpgradeRequired) {
		t.Fatalf("legacy opt-in: %v", err)
	}
	u, err := s.CommitPGPKeyring(context.Background(), id, update)
	if err != nil {
		t.Fatal(err)
	}
	for _, revision := range []*uint64{nil, &update.ExpectedRevision} {
		if _, err := s.RewrapPGPKeyring(id, `{"v":2}`, u.PGPFingerprint, revision); err == nil {
			t.Fatal("unguarded/stale restoration")
		}
	}
	if _, err := s.RewrapPGPKeyring(id, `{"v":2}`, "", &u.PGPRevision); err == nil {
		t.Fatal("missing fingerprint")
	}
	if _, err := s.RewrapPGPKeyring(id, `{"v":2}`, strings.Repeat("F", 40), &u.PGPRevision); !errors.Is(err, ErrPGPIdentityChanged) {
		t.Fatalf("wrong fingerprint: %v", err)
	}
	changed, err := s.RewrapPGPKeyring(id, `{"v":2,"restored":true}`, u.PGPFingerprint, &u.PGPRevision)
	if err != nil {
		t.Fatal(err)
	}
	if changed.PGPRevision != u.PGPRevision+1 || changed.PGPPrivateKeyWrapped != `{"v":2,"restored":true}` {
		t.Fatal("missing atomic rewrap")
	}
	normalized := changed
	normalized.PGPPrivateKeyWrapped, normalized.PGPRevision, normalized.UpdatedAt = u.PGPPrivateKeyWrapped, u.PGPRevision, u.UpdatedAt
	if !reflect.DeepEqual(normalized, u) {
		t.Fatal("restoration changed credential or retained material")
	}
	reset, err := s.SetPassword(context.Background(), id, "temporary-ring-password", true, &changed.PGPRevision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RewrapPGPKeyring(id, `{"v":2}`, reset.PGPFingerprint, &reset.PGPRevision); !errors.Is(err, ErrPGPKeyringUpgradeRequired) {
		t.Fatalf("forced reset: %v", err)
	}
}

func TestSetPGPKeyringRecoveryPreservesOtherState(t *testing.T) {
	s, id, update := keyringCandidate(t)
	if _, err := s.SetPGPKeyringRecovery(id, `{"v":2}`, "now", update.PrimaryFingerprints[0], &update.ExpectedRevision); !errors.Is(err, ErrPGPKeyringUpgradeRequired) {
		t.Fatalf("legacy: %v", err)
	}
	u, err := s.CommitPGPKeyring(context.Background(), id, update)
	if err != nil {
		t.Fatal(err)
	}
	for _, revision := range []*uint64{nil, &update.ExpectedRevision} {
		if _, err := s.SetPGPKeyringRecovery(id, `{"v":2}`, "now", u.PGPFingerprint, revision); err == nil {
			t.Fatal("unguarded/stale write")
		}
	}
	if _, err := s.SetPGPKeyringRecovery(id, `{"v":2}`, "now", "", &u.PGPRevision); err == nil {
		t.Fatal("missing fingerprint")
	}
	changed, err := s.SetPGPKeyringRecovery(id, `{"v":2,"fresh":true}`, "new-time", u.PGPFingerprint, &u.PGPRevision)
	if err != nil {
		t.Fatal(err)
	}
	if changed.PGPRevision != u.PGPRevision+1 || len(changed.PGPWrappedEnvelopes) != 1 || changed.PGPWrappedEnvelopes[0].Envelope != `{"v":2,"fresh":true}` || changed.PGPWrappedEnvelopes[0].AddedAt != "new-time" {
		t.Fatal("recovery not committed")
	}
	normalized := changed
	normalized.PGPWrappedEnvelopes, normalized.PGPRevision, normalized.UpdatedAt = u.PGPWrappedEnvelopes, u.PGPRevision, u.UpdatedAt
	if !reflect.DeepEqual(normalized, u) {
		t.Fatal("recovery write altered credential/password/public/keyring data")
	}
	deleted, err := s.DeletePGPWrappedEnvelope(id, EnvelopeSlotRecovery, &changed.PGPRevision)
	if err != nil {
		t.Fatal(err)
	}
	added, err := s.SetPGPKeyringRecovery(id, `{"v":2,"fresh":true}`, "new-time", deleted.PGPFingerprint, &deleted.PGPRevision)
	if err != nil {
		t.Fatal(err)
	}
	if len(added.PGPWrappedEnvelopes) != 1 {
		t.Fatal("did not add absent slot")
	}
	reset, err := s.SetPassword(context.Background(), id, "temporary-ring-password", true, &added.PGPRevision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPGPKeyringRecovery(id, `{"v":2}`, "now", reset.PGPFingerprint, &reset.PGPRevision); !errors.Is(err, ErrPGPKeyringUpgradeRequired) {
		t.Fatalf("forced: %v", err)
	}
}
