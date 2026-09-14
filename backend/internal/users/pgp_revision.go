package users

import (
	"errors"
	"strings"
)

// MaxPGPRevision keeps the JSON counter exactly representable in browser numbers.
const MaxPGPRevision uint64 = 1<<53 - 1

var ErrPGPRevisionChanged = errors.New("PGP state changed; reload before preparing the update again")
var ErrInvalidPGPRevision = errors.New("invalid expected PGP revision")

// Capture values, not the envelope slice: a mutation may edit that slice in place.
// Device deliveries are independent of the identity/recovery material and do not
// advance this counter. Credential changes (including admin reset) always do.
type pgpRevisionState struct {
	keyringVersion                                                                    int
	materialGeneration                                                                uint64
	keyInventory, primaryInventory                                                    string
	fingerprint, keyID, publicKey, privateKey, wrapped, protection, source, createdAt string
	passwordHash, authDerivation, loginSalt                                           string
	loginIterations                                                                   int
	recovery                                                                          WrappedEnvelope
}

func pgpState(u User) pgpRevisionState {
	state := pgpRevisionState{
		fingerprint: u.PGPFingerprint, keyID: u.PGPKeyID, publicKey: u.PGPPublicKey,
		privateKey: u.PGPPrivateKeyEnc, wrapped: u.PGPPrivateKeyWrapped,
		protection: u.PGPKeyProtection, source: u.PGPKeySource, createdAt: u.PGPKeyCreatedAt,
		passwordHash: u.PasswordHash, authDerivation: u.AuthDerivation,
		loginSalt: u.LoginSalt, loginIterations: u.LoginIterations,
	}
	if u.PGPKeyring != nil {
		state.keyringVersion = u.PGPKeyring.Version
		state.materialGeneration = u.PGPKeyring.MaterialGeneration
		state.keyInventory = strings.Join(u.PGPKeyring.KeyFingerprints, ",")
		state.primaryInventory = strings.Join(u.PGPKeyring.PrimaryFingerprints, ",")
	}
	for _, envelope := range u.PGPWrappedEnvelopes {
		if envelope.Slot == EnvelopeSlotRecovery {
			state.recovery = envelope
			break
		}
	}
	return state
}

// Optional only for compatibility with existing single-key callers. New callers
// supply the revision from the snapshot used to prepare their ciphertext. Zero
// is a real expectation for records predating revisions, not "skip the guard".
func (s *Store) mutatePGP(id string, expected *uint64, fn func(*User) error) (User, error) {
	guard := func(_ []User, u User) error {
		if u.PGPKeyring != nil && (u.PGPKeyring.Version != 1 || expected == nil) {
			return ErrPGPKeyringUpgradeRequired
		}
		if expected != nil {
			if *expected > MaxPGPRevision {
				return ErrInvalidPGPRevision
			}
			if *expected != u.PGPRevision {
				return ErrPGPRevisionChanged
			}
		}
		return nil
	}
	return s.mutateGuarded(id, guard, fn)
}
