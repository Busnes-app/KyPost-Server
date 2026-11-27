package pgpmail_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpprofile"
	"github.com/ProtonMail/gopenpgp/v3/armor"
	"github.com/ProtonMail/gopenpgp/v3/constants"
	"github.com/ProtonMail/gopenpgp/v3/crypto"
)

func TestP3SharedReadsRefuseTampering(t *testing.T) {
	for _, name := range []string{"v4", "v4-pqc", "v6", "v6-pqc"} {
		t.Run(name, func(t *testing.T) {
			info, err := pgpmail.InspectPublicKey(string(readFixture(t, name+".pub.asc")))
			if err != nil || len(info.Fingerprint) != map[bool]int{true: 64, false: 40}[strings.HasPrefix(name, "v6")] {
				t.Fatal(info, err)
			}
			identity, err := pgpmail.ParsePrivateKey(string(readFixture(t, name+".sec.asc")))
			if err != nil || !strings.EqualFold(identity.Fingerprint, info.Fingerprint) {
				t.Fatal("private/public disagree", err)
			}
			if name == "v6" {
				return
			} // classical v6 keys have no message fixture
			key, err := crypto.NewKey(readFixture(t, name+".sec.asc"))
			if err != nil {
				t.Fatal(err)
			}
			defer key.ClearPrivateParams()
			h, err := crypto.PGP().Decryption().DecryptionKey(key).New()
			if err != nil {
				t.Fatal(err)
			}
			defer h.ClearPrivateParams()
			result, err := h.Decrypt(readFixture(t, name+".msg.asc"), crypto.Armor)
			if err != nil || !bytes.Equal(result.Bytes(), readFixture(t, "message.txt")) {
				t.Fatal("fixture read", err)
			}
			binary, err := armor.UnarmorBytes(readFixture(t, name+".msg.asc"))
			if err != nil {
				t.Fatal(err)
			}
			binary[len(binary)-1] ^= 1
			if _, err := h.Decrypt(binary, crypto.Bytes); err == nil {
				t.Fatal("accepted tampered ciphertext")
			}
		})
	}
}

func TestP3ReadVerifyLevel5(t *testing.T) {
	key, err := pgpprofile.V6().KeyGeneration().AddUserId("Level 5", "level5@example.test").New().GenerateKeyWithSecurity(constants.HighSecurity)
	if err != nil {
		t.Fatal(err)
	}
	defer key.ClearPrivateParams()
	pub, err := key.GetArmoredPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	// The upstream reader handles level 5, but Spec A Q7 keeps server key
	// admission at category 3. Reader coverage must not widen that allowlist.
	if _, err := pgpmail.InspectPublicKey(pub); !errors.Is(err, pgpmail.ErrKeyPolicy) {
		t.Fatal("level-5 admission bypassed Spec A Q7", err)
	}
	signer, err := pgpprofile.V6().Sign().SigningKey(key).Detached().New()
	if err != nil {
		t.Fatal(err)
	}
	defer signer.ClearPrivateParams()
	sig, err := signer.Sign([]byte("level5"), crypto.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	verify, err := crypto.PGP().Verify().VerificationKey(key).New()
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"level5", "changed"} {
		result, err := verify.VerifyDetached([]byte(data), sig, crypto.Bytes)
		if err != nil || (result.SignatureError() == nil) != (data == "level5") {
			t.Fatalf("%s: %+v %v", data, result, err)
		}
	}
}
