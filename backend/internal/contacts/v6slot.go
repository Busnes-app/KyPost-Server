package contacts

import (
	"errors"
	"strings"

	"github.com/ProtonMail/gopenpgp/v3/crypto"
)

// ErrV6Slot refuses a write whose v6 slot is not a v6 key beside a non-v6 key.
var ErrV6Slot = errors.New("pgpKeyV6 must hold an OpenPGP v6 public key next to a non-v6 pgpKey")

// keyVersion is an armored key's OpenPGP version, 0 when it does not parse.
func keyVersion(armored string) int {
	key, err := crypto.NewKeyFromArmored(armored)
	if err != nil || key.GetEntity() == nil {
		return 0
	}
	return key.GetEntity().PrimaryKey.Version
}

// splitV6Slot puts a write's keys in their slots before the sink pins them.
//
//   - A v6 key written to PGPKey while the stored PGPKey is a v4 key is the
//     correspondent ADDING a v6 certificate, not changing keys: it moves to the
//     v6 slot and the v4 key keeps its slot and pin. Every client that predates
//     the slot sends one pgpKey, and this is how it adds a v6 key without
//     raising key_changed. Its source moves with it; its pin and confirmation only
//     when they name the v6 key, so an echoed v4 pin stays the v4 key's.
//     Not for Autocrypt, which carries no v6 certificate (it cannot fit the
//     10 KiB header): a harvested v6 key over a v4 one keeps the old pin and
//     stays the key change the harvest means it to be.
//   - A write that keeps a key but carries no v6 slot keeps the stored one, so
//     those clients cannot drop it by omission. A write with no key removes
//     both: removing "the key" must not leave the other one encrypting.
//   - A v4 key written over a v6 key in PGPKey is the reverse addition.
//   - A v6 slot alone moves into PGPKey: one key, one slot.
func splitV6Slot(c *Contact, existing Contact, exists bool) error {
	if exists && c.PGPKey != "" && c.PGPKeySource != PGPSourceAutocrypt && keyVersion(c.PGPKey) == 6 && keyVersion(existing.PGPKey) == 4 {
		// A read-modify-write echoes the v6 slot it read; the v6 key in PGPKey
		// replaces it. The same v6 key keeps its stored pin (carried below).
		fp := pgpKeyFingerprint(c.PGPKey)
		c.PGPKeyV6, c.PGPKeyV6Source, c.PGPKeyV6Fingerprint, c.PGPKeyV6Verified = c.PGPKey, c.PGPKeySource, "", false
		if strings.EqualFold(fp, pgpKeyFingerprint(existing.PGPKeyV6)) {
			c.PGPKeyV6Source = ""
		} else if strings.EqualFold(c.PGPKeyFingerprint, fp) {
			c.PGPKeyV6Fingerprint, c.PGPKeyV6Verified = c.PGPKeyFingerprint, c.PGPKeyVerified
		}
		c.PGPKey, c.PGPKeySource, c.PGPKeyFingerprint, c.PGPKeyVerified, c.PGPKeyAutocryptDate =
			existing.PGPKey, existing.PGPKeySource, existing.PGPKeyFingerprint, existing.PGPKeyVerified, existing.PGPKeyAutocryptDate
	}
	// The reverse: a v4 key written over a v6 key held in the original slot
	// joins it. The v6 key keeps its pin and confirmation in the v6 slot, and
	// an echoed v6 pin is not carried onto the v4 key.
	if exists && c.PGPKeyV6 == "" && existing.PGPKeyV6 == "" && c.PGPKeySource != PGPSourceAutocrypt && keyVersion(existing.PGPKey) == 6 {
		if v := keyVersion(c.PGPKey); v != 0 && v != 6 {
			c.PGPKeyV6, c.PGPKeyV6Source, c.PGPKeyV6Fingerprint, c.PGPKeyV6Verified =
				existing.PGPKey, existing.PGPKeySource, existing.PGPKeyFingerprint, existing.PGPKeyVerified
			if existing.PGPKeyFingerprint != "" && strings.EqualFold(c.PGPKeyFingerprint, existing.PGPKeyFingerprint) {
				c.PGPKeyFingerprint, c.PGPKeySource, c.PGPKeyVerified, c.PGPKeyAutocryptDate = "", "", false, ""
			}
		}
	}
	if c.PGPKey == "" {
		c.PGPKey, c.PGPKeySource, c.PGPKeyFingerprint, c.PGPKeyVerified = c.PGPKeyV6, c.PGPKeyV6Source, c.PGPKeyV6Fingerprint, c.PGPKeyV6Verified
		c.PGPKeyV6, c.PGPKeyV6Source, c.PGPKeyV6Fingerprint, c.PGPKeyV6Verified = "", "", "", false
		return nil
	}
	if c.PGPKeyV6 == "" && exists {
		c.PGPKeyV6, c.PGPKeyV6Source, c.PGPKeyV6Fingerprint, c.PGPKeyV6Verified =
			existing.PGPKeyV6, existing.PGPKeyV6Source, existing.PGPKeyV6Fingerprint, existing.PGPKeyV6Verified
	}
	if c.PGPKeyV6 == "" {
		c.PGPKeyV6Source, c.PGPKeyV6Fingerprint, c.PGPKeyV6Verified = "", "", false
		return nil
	}
	if keyVersion(c.PGPKeyV6) != 6 || keyVersion(c.PGPKey) == 6 {
		return ErrV6Slot
	}
	// Same v6 key resent without provenance keeps its pin (carryPGPProvenance).
	if exists && c.PGPKeyV6Fingerprint == "" && c.PGPKeyV6Source == "" && !c.PGPKeyV6Verified && existing.PGPKeyV6 != "" &&
		strings.EqualFold(pgpKeyFingerprint(c.PGPKeyV6), pgpKeyFingerprint(existing.PGPKeyV6)) {
		c.PGPKeyV6Source, c.PGPKeyV6Fingerprint, c.PGPKeyV6Verified = existing.PGPKeyV6Source, existing.PGPKeyV6Fingerprint, existing.PGPKeyV6Verified
	}
	if c.PGPKeyV6Fingerprint == "" {
		c.PGPKeyV6Fingerprint = pgpKeyFingerprint(c.PGPKeyV6)
	}
	return nil
}
