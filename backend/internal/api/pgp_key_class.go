package api

import (
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/contacts"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	openpgp "github.com/ProtonMail/go-crypto/openpgp/v2"
	"github.com/ProtonMail/gopenpgp/v3/crypto"
)

// keyClass is what one certificate offers a sender.
type keyClass struct {
	V6 bool // an OpenPGP v6 (RFC 9580) certificate
	PQ bool // its encryption key is ML-KEM-768+X25519 (RFC 9980, ID 35)
}

// rank orders certificates: v6 PQC > v4 with an ID 35 subkey > v6 > v4.
func (k keyClass) rank() int {
	r := 1
	if k.V6 {
		r = 2
	}
	if k.PQ {
		r += 2
	}
	return r
}

// capability is the per-recipient answer resolve and check give: "pq", "v6"
// or "v4".
func (k keyClass) capability() string {
	switch {
	case k.PQ:
		return "pq"
	case k.V6:
		return "v6"
	}
	return "v4"
}

// classOfEntity reads e's class from the encryption key go-crypto would pick
// now, which prefers a PQ subkey over an equally new classical one.
func classOfEntity(e *openpgp.Entity) keyClass {
	k, _ := usableClassOfEntity(e)
	return k
}

// usableClassOfEntity is classOfEntity plus whether e can be encrypted to now.
func usableClassOfEntity(e *openpgp.Entity) (keyClass, bool) {
	ek, ok := e.EncryptionKey(time.Now(), nil)
	return keyClass{
		V6: e.PrimaryKey.Version == 6,
		PQ: ok && ek.PublicKey.PubKeyAlgo == packet.PubKeyAlgoMlkem768X25519,
	}, ok
}

// contactCapability is the best class among a contact's keys that still match
// their pins and can be encrypted to now, or "" when it holds none.
func contactCapability(c contacts.Contact) string {
	best := keyClass{}
	found := false
	for _, slot := range []struct{ key, pin string }{{c.PGPKey, c.PGPKeyFingerprint}, {c.PGPKeyV6, c.PGPKeyV6Fingerprint}} {
		if slot.key == "" || !slotMatchesPin(slot.key, slot.pin) {
			continue
		}
		key, err := crypto.NewKeyFromArmored(slot.key)
		if err != nil || key.GetEntity() == nil {
			continue
		}
		k, usable := usableClassOfEntity(key.GetEntity())
		if !usable {
			continue
		}
		if !found || k.rank() > best.rank() {
			best, found = k, true
		}
	}
	if !found {
		return ""
	}
	return best.capability()
}

// armoredCapability is the class of one usable armored certificate ("pq",
// "v6", "v4"), or "" if it cannot be encrypted to now.
func armoredCapability(armored string) string {
	key, err := crypto.NewKeyFromArmored(armored)
	if err != nil || key.GetEntity() == nil {
		return ""
	}
	k, usable := usableClassOfEntity(key.GetEntity())
	if !usable {
		return ""
	}
	return k.capability()
}

// keyVersionOf is an armored key's OpenPGP version, 0 when it does not parse.
func keyVersionOf(armored string) int {
	key, err := crypto.NewKeyFromArmored(strings.TrimSpace(armored))
	if err != nil || key.GetEntity() == nil {
		return 0
	}
	return key.GetEntity().PrimaryKey.Version
}
