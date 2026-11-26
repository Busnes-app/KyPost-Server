package pgpprofile

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ProtonMail/gopenpgp/v3/crypto"
)

// V6 must make RFC 9980 keys and encrypt with v6 PKESKs and SEIPDv2 OCB,
// not gopenpgp's PQC() default of GCM.
func TestV6ProfileIsPQWithOCB(t *testing.T) {
	key, err := V6().KeyGeneration().AddUserId("T", "t@example.test").New().GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	e := key.GetEntity()
	if key.GetVersion() != 6 || e.PrimaryKey.PubKeyAlgo != packet.PubKeyAlgoMldsa65Ed25519 ||
		len(e.Subkeys) != 1 || e.Subkeys[0].PublicKey.PubKeyAlgo != packet.PubKeyAlgoMlkem768X25519 {
		t.Fatalf("v%d %v", key.GetVersion(), e.PrimaryKey.PubKeyAlgo)
	}
	pub, err := key.ToPublic()
	if err != nil {
		t.Fatal(err)
	}
	h, err := V6().Encryption().Recipient(pub).New()
	if err != nil {
		t.Fatal(err)
	}
	msg, err := h.Encrypt([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	armored, err := msg.Armor()
	if err != nil {
		t.Fatal(err)
	}
	block, err := armor.Decode(strings.NewReader(armored))
	if err != nil {
		t.Fatal(err)
	}
	r := packet.NewReader(block.Body)
	for {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			t.Fatal("no encrypted data packet")
		}
		if err != nil {
			t.Fatal(err)
		}
		switch p := p.(type) {
		case *packet.EncryptedKey:
			if p.Version != 6 {
				t.Fatalf("PKESK v%d", p.Version)
			}
		case *packet.SymmetricallyEncrypted:
			if p.Version != 2 || p.Mode != packet.AEADModeOCB || p.Cipher != packet.CipherAES256 {
				t.Fatalf("SEIPD v%d mode %v cipher %v", p.Version, p.Mode, p.Cipher)
			}
			dh, err := crypto.PGP().Decryption().DecryptionKey(key).New()
			if err != nil {
				t.Fatal(err)
			}
			res, err := dh.Decrypt([]byte(armored), crypto.Armor)
			if err != nil || string(res.Bytes()) != "x" {
				t.Fatalf("decrypt: %v", err)
			}
			return
		}
	}
}
