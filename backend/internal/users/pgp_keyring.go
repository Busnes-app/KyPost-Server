package users

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/Busness-app/kypost-server/backend/internal/pgpmail"
	"github.com/ProtonMail/gopenpgp/v3/armor"
)

const MaxPGPKeyringKeys = 16
const MaxPGPKeyringFingerprints = 256

var ErrPGPKeyringUpgradeRequired = errors.New("this account requires a complete keyring update; upgrade the client")
var ErrInvalidPGPKeyring = errors.New("invalid or incomplete PGP keyring update")

// PGPKeyringState describes opaque sealed material. It is a consistency check,
// not proof that ciphertext contains these private packets; clients must verify it.
type PGPKeyringState struct {
	Version             int      `json:"version"`
	MaterialGeneration  uint64   `json:"materialGeneration"`
	PrimaryFingerprints []string `json:"primaryFingerprints"`
	KeyFingerprints     []string `json:"keyFingerprints"`
}

type PGPKeyringCredential struct {
	AuthSecret string
	LoginSalt  string
	Iterations int
}

type PGPKeyringUpdate struct {
	ExpectedRevision    uint64
	MaterialGeneration  uint64
	PrimaryFingerprints []string
	KeyFingerprints     []string
	PublicKey           string
	PasswordEnvelope    string
	RecoveryEnvelope    string
	Source              string
	Credential          *PGPKeyringCredential
}

func normalizeFingerprints(values []string, limit int) ([]string, error) {
	if len(values) == 0 || len(values) > limit {
		return nil, ErrInvalidPGPKeyring
	}
	normalized := make([]string, len(values))
	for i, value := range values {
		if len(value) != 40 && len(value) != 64 {
			return nil, ErrInvalidPGPKeyring
		}
		if _, err := hex.DecodeString(value); err != nil {
			return nil, ErrInvalidPGPKeyring
		}
		normalized[i] = strings.ToUpper(value)
	}
	slices.Sort(normalized)
	for i := 1; i < len(normalized); i++ {
		if normalized[i] == normalized[i-1] {
			return nil, ErrInvalidPGPKeyring
		}
	}
	return normalized, nil
}

func containsFingerprints(inventory, required []string) bool {
	for _, fingerprint := range required {
		if !slices.Contains(inventory, strings.ToUpper(fingerprint)) {
			return false
		}
	}
	return true
}

// CommitPGPKeyring commits complete password/recovery material, active public key,
// inventory and an optional derived credential under one revision/file lock.
// No HTTP route calls this yet: conversion requires recovery and native gates.
func (s *Store) CommitPGPKeyring(ctx context.Context, id string, update PGPKeyringUpdate) (User, error) {
	if update.MaterialGeneration == 0 || update.MaterialGeneration > MaxPGPRevision ||
		len(update.PublicKey) > MaxWrappedEnvelopeBytes ||
		(update.Source != "generated" && update.Source != "imported") {
		return User{}, ErrInvalidPGPKeyring
	}
	for _, envelope := range []string{update.PasswordEnvelope, update.RecoveryEnvelope} {
		if strings.TrimSpace(envelope) == "" {
			return User{}, ErrInvalidPGPKeyring
		}
		if err := ValidateWrappedEnvelope(envelope); err != nil {
			return User{}, err
		}
	}
	primaries, err := normalizeFingerprints(update.PrimaryFingerprints, MaxPGPKeyringKeys)
	if err != nil {
		return User{}, err
	}
	inventory, err := normalizeFingerprints(update.KeyFingerprints, MaxPGPKeyringFingerprints)
	if err != nil {
		return User{}, err
	}
	info, err := pgpmail.InspectPublicKey(update.PublicKey)
	if err != nil {
		return User{}, ErrInvalidPGPKeyring
	}
	active := strings.ToUpper(info.Fingerprint)
	if !slices.Contains(primaries, active) || !containsFingerprints(inventory, primaries) ||
		!containsFingerprints(inventory, info.KeyFingerprints) {
		return User{}, ErrInvalidPGPKeyring
	}
	// A declared primary cannot simultaneously be an active subkey.
	for _, fingerprint := range info.KeyFingerprints[1:] {
		if slices.Contains(primaries, fingerprint) {
			return User{}, ErrInvalidPGPKeyring
		}
	}
	status, err := pgpmail.CheckKeyStatus(info.ArmoredPublicKey)
	if err != nil {
		return User{}, ErrInvalidPGPKeyring
	}
	var credential PGPKeyringCredential
	var hash string
	if update.Credential != nil {
		credential = *update.Credential
		if err := ValidateAuthSecret(credential.AuthSecret); err != nil {
			return User{}, err
		}
		if err := validateLoginSalt(credential.LoginSalt); err != nil {
			return User{}, err
		}
		if err := validateLoginIterations(credential.Iterations); err != nil {
			return User{}, err
		}
		hash, err = HashPassword(ctx, credential.AuthSecret)
		if err != nil {
			return User{}, err
		}
	}
	return s.mutatePGP(id, &update.ExpectedRevision, func(u *User) error {
		if u.PGPProtection() != PGPProtectionClient || !u.UsesDerivedAuth() {
			return ErrPGPKeyringUpgradeRequired
		}
		previous, err := pgpmail.InspectPublicKey(u.PGPPublicKey)
		if err != nil || !strings.EqualFold(previous.Fingerprint, u.PGPFingerprint) {
			return ErrInvalidPGPKeyring
		}
		oldPrimaries := []string{strings.ToUpper(previous.Fingerprint)}
		oldInventory := previous.KeyFingerprints
		generation := uint64(1)
		conversion := u.PGPKeyring == nil
		if !conversion {
			oldPrimaries = u.PGPKeyring.PrimaryFingerprints
			oldInventory = u.PGPKeyring.KeyFingerprints
			generation = u.PGPKeyring.MaterialGeneration
			if generation == 0 || generation > MaxPGPRevision {
				return ErrInvalidPGPKeyring
			}
		}
		if !containsFingerprints(inventory, oldInventory) || !containsFingerprints(primaries, oldPrimaries) {
			return ErrInvalidPGPKeyring
		}
		changedActive := !strings.EqualFold(active, u.PGPFingerprint)
		// Conversion wraps the existing identity first; retirement is a later commit.
		if conversion && changedActive {
			return ErrInvalidPGPKeyring
		}
		if changedActive && (slices.Contains(oldInventory, active) || !status.Usable() || !info.CanEncrypt) {
			return ErrInvalidPGPKeyring
		}
		for _, primary := range primaries {
			if slices.Contains(oldInventory, primary) && !slices.Contains(oldPrimaries, primary) {
				return ErrInvalidPGPKeyring
			}
		}
		// UID/revocation/public-packet edits need their own preservation rules.
		publicKey := info.ArmoredPublicKey
		if !changedActive {
			submitted, err := armor.Unarmor(update.PublicKey)
			if err != nil {
				return ErrInvalidPGPKeyring
			}
			stored, err := armor.Unarmor(u.PGPPublicKey)
			if err != nil {
				return ErrInvalidPGPKeyring
			}
			if !bytes.Equal(submitted, stored) {
				return ErrPGPKeyringUpgradeRequired
			}
			// Serialization iterates UID maps; retain the exact stored packets.
			publicKey = u.PGPPublicKey
		}
		grew := len(inventory) != len(oldInventory) || len(primaries) != len(oldPrimaries)
		if !conversion && grew {
			if generation == MaxPGPRevision {
				return ErrInvalidPGPKeyring
			}
			generation++
		}
		if update.MaterialGeneration != generation {
			return ErrInvalidPGPKeyring
		}
		u.PGPKeyring = &PGPKeyringState{Version: 1, MaterialGeneration: generation, PrimaryFingerprints: primaries, KeyFingerprints: inventory}
		u.PGPFingerprint, u.PGPKeyID, u.PGPPublicKey = info.Fingerprint, info.KeyID, publicKey
		u.PGPPrivateKeyWrapped, u.PGPPrivateKeyEnc, u.PGPKeyProtection = update.PasswordEnvelope, "", PGPProtectionClient
		now := time.Now().UTC().Format(time.RFC3339)
		if changedActive {
			u.PGPKeySource, u.PGPKeyCreatedAt = update.Source, now
		}
		kept := make([]WrappedEnvelope, 0, len(u.PGPWrappedEnvelopes)+1)
		if !conversion && !grew && !changedActive {
			for _, envelope := range u.PGPWrappedEnvelopes {
				if envelope.Slot != EnvelopeSlotRecovery {
					kept = append(kept, envelope)
				}
			}
		}
		u.PGPWrappedEnvelopes = append(kept, WrappedEnvelope{Slot: EnvelopeSlotRecovery, Envelope: update.RecoveryEnvelope, AddedAt: now})
		if update.Credential != nil {
			u.PasswordHash, u.AuthDerivation = hash, AuthDerivationPBKDF2
			u.LoginSalt, u.LoginIterations, u.MustChangePassword = credential.LoginSalt, credential.Iterations, false
		}
		return nil
	})
}
