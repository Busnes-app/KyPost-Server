package pgpmail

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp/packet"
	openpgp "github.com/ProtonMail/go-crypto/openpgp/v2"
	"github.com/ProtonMail/gopenpgp/v3/crypto"
)

func rsaEntity(t *testing.T, primaryBits, subkeyBits int) *openpgp.Entity {
	t.Helper()
	entity, err := openpgp.NewEntity("Floor", "", "floor@example.test", &packet.Config{Algorithm: packet.PubKeyAlgoRSA, RSABits: primaryBits})
	if err != nil {
		t.Fatal(err)
	}
	if subkeyBits != 0 {
		if err := entity.AddEncryptionSubkey(&packet.Config{Algorithm: packet.PubKeyAlgoRSA, RSABits: subkeyBits}); err != nil {
			t.Fatal(err)
		}
		if err := entity.AddSigningSubkey(&packet.Config{Algorithm: packet.PubKeyAlgoRSA, RSABits: subkeyBits}); err != nil {
			t.Fatal(err)
		}
		entity.Subkeys = entity.Subkeys[1:] // drop the strong default subkey
	}
	return entity
}

func armoredPublic(t *testing.T, entity *openpgp.Entity) string {
	t.Helper()
	var buf bytes.Buffer
	if err := entity.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	key, err := crypto.NewKey(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	armored, err := key.GetArmoredPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	return armored
}

func signerFor(t *testing.T, entity *openpgp.Entity) *Identity {
	t.Helper()
	var buf bytes.Buffer
	if err := entity.SerializePrivate(&buf, nil); err != nil {
		t.Fatal(err)
	}
	key, err := crypto.NewKey(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	armored, err := key.Armor()
	if err != nil {
		t.Fatal(err)
	}
	id, err := ParsePrivateKey(armored)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

const floorMessage = "From: a@example.test\r\nTo: b@example.test\r\nSubject: floor\r\nContent-Type: text/plain\r\n\r\nbody\r\n"

func TestEncryptMIMERefusesRSABelow2048(t *testing.T) {
	strong := rsaEntity(t, 2048, 0)
	strongKey := armoredPublic(t, strong)
	cases := map[string]*openpgp.Entity{
		"RSA-1024":                       rsaEntity(t, 1024, 0),
		"RSA-2047":                       rsaEntity(t, 2047, 0),
		"RSA-2048 with RSA-2047 subkeys": rsaEntity(t, 2048, 2047),
	}
	for name, weak := range cases {
		t.Run(name, func(t *testing.T) {
			weakKey := armoredPublic(t, weak)
			for _, recipients := range [][]string{{weakKey}, {strongKey, weakKey}} {
				if _, err := EncryptMIME([]byte(floorMessage), recipients, nil); !errors.Is(err, ErrWeakRSAKey) {
					t.Fatalf("encrypt to %s: got %v, want ErrWeakRSAKey", name, err)
				}
			}
			if _, err := EncryptStoredMIME([]byte(floorMessage), weakKey); !errors.Is(err, ErrWeakRSAKey) {
				t.Fatalf("incoming encryption to %s: got %v, want ErrWeakRSAKey", name, err)
			}
			if _, err := EncryptMIME([]byte(floorMessage), []string{strongKey}, signerFor(t, weak)); !errors.Is(err, ErrWeakRSAKey) {
				t.Fatalf("sign with %s: got %v, want ErrWeakRSAKey", name, err)
			}
			// The key itself stays importable; only new use is refused.
			if _, err := InspectPublicKey(weakKey); err != nil {
				t.Fatalf("inspect %s: %v", name, err)
			}
		})
	}
	if _, err := EncryptMIME([]byte(floorMessage), []string{strongKey}, signerFor(t, strong)); err != nil {
		t.Fatalf("RSA-2048 encrypt and sign: %v", err)
	}
}
