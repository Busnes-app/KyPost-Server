package pgpmail_test

// ML-DSA-65+Ed25519 sign/verify and ML-KEM-768+X25519 encapsulate/decapsulate
// (a v6 PKESK made and opened) on gopenpgp's profile.PQC(), with the fixture
// key. Run: go test -run '^$' -bench PQC ./internal/pgpmail

import (
	"testing"

	"github.com/ProtonMail/gopenpgp/v3/constants"
	"github.com/ProtonMail/gopenpgp/v3/crypto"
	"github.com/ProtonMail/gopenpgp/v3/profile"
)

func pqcBenchKey(b *testing.B) (*crypto.PGPHandle, *crypto.Key, *crypto.Key) {
	b.Helper()
	sec, err := crypto.NewKeyFromArmored(string(readFixture(b, "v6-pqc.sec.asc")))
	if err != nil {
		b.Fatal(err)
	}
	pub, err := sec.ToPublic()
	if err != nil {
		b.Fatal(err)
	}
	return crypto.PGPWithProfile(profile.PQC()), sec, pub
}

func BenchmarkPQCMLDSA65Sign(b *testing.B) {
	pgp, sec, _ := pqcBenchKey(b)
	signer, err := pgp.Sign().SigningKey(sec).Detached().New()
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := signer.Sign([]byte(fixtureText), crypto.Bytes); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPQCMLDSA65Verify(b *testing.B) {
	pgp, sec, pub := pqcBenchKey(b)
	signer, err := pgp.Sign().SigningKey(sec).Detached().New()
	if err != nil {
		b.Fatal(err)
	}
	sig, err := signer.Sign([]byte(fixtureText), crypto.Bytes)
	if err != nil {
		b.Fatal(err)
	}
	verifier, err := pgp.Verify().VerificationKey(pub).New()
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		res, err := verifier.VerifyDetached([]byte(fixtureText), sig, crypto.Bytes)
		if err != nil || res.SignatureError() != nil {
			b.Fatal(err, res.SignatureError())
		}
	}
}

func pqcSessionKey(b *testing.B) *crypto.SessionKey {
	sk, err := crypto.GenerateSessionKeyAlgo(constants.AES256)
	if err != nil {
		b.Fatal(err)
	}
	return sk
}

func BenchmarkPQCMLKEM768Encapsulate(b *testing.B) {
	pgp, _, pub := pqcBenchKey(b)
	enc, err := pgp.Encryption().Recipient(pub).New()
	if err != nil {
		b.Fatal(err)
	}
	sk := pqcSessionKey(b)
	for b.Loop() {
		if _, err := enc.EncryptSessionKey(sk); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPQCMLKEM768Decapsulate(b *testing.B) {
	pgp, sec, pub := pqcBenchKey(b)
	enc, err := pgp.Encryption().Recipient(pub).New()
	if err != nil {
		b.Fatal(err)
	}
	pkesk, err := enc.EncryptSessionKey(pqcSessionKey(b))
	if err != nil {
		b.Fatal(err)
	}
	dec, err := pgp.Decryption().DecryptionKey(sec).New()
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := dec.DecryptSessionKey(pkesk); err != nil {
			b.Fatal(err)
		}
	}
}
