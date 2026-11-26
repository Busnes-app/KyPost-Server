package pgpmail_test

// The shared OpenPGP v6 and RFC 9980 fixtures in testdata/openpgp-v6/, which
// the clients read too. buildPGPFixtures makes every byte from a fixed seed
// and a fixed time; TestPGPFixturesRegenerateByteIdentical requires the
// committed files to equal its output. Regenerate only on purpose:
//
//	go test -tags pgpfixturegen -run TestPGPFixturesRegenerateByteIdentical ./internal/pgpmail

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/cryptotest"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ProtonMail/gopenpgp/v3/crypto"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
)

const (
	fixtureDir  = "../../../testdata/openpgp-v6"
	fixtureSeed = 9980
	// fixtureText is what every fixture message and the detached signature carry.
	fixtureText = "KyPost OpenPGP v6 fixture. Do not use these keys for anything else.\n"
	// goldenMIME is the stored message the incoming-encryption golden encrypts.
	goldenMIME = "From: Sender <sender@example.org>\r\nTo: v4@example.org\r\nSubject: Golden\r\nDate: Wed, 25 Nov 2026 00:00:00 +0000\r\nMessage-ID: <golden@example.org>\r\n\r\nGolden body.\r\n"
)

var fixtureTime = time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)

func fixtureConfig(v6 bool, algo packet.PublicKeyAlgorithm) *packet.Config {
	c := &packet.Config{
		Time:          func() time.Time { return fixtureTime },
		DefaultCipher: packet.CipherAES256,
		Algorithm:     algo,
		V6Keys:        v6,
	}
	if algo == packet.PubKeyAlgoEdDSA {
		c.Curve = packet.Curve25519
	}
	if v6 {
		c.AEADConfig = &packet.AEADConfig{DefaultMode: packet.AEADModeOCB}
	}
	return c
}

func armorBytes(t *testing.T, blockType string, write func(io.Writer) error) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := armor.Encode(&b, blockType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := write(w); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b.WriteByte('\n')
	return b.Bytes()
}

func newFixtureKey(t *testing.T, name, email string, v6 bool, algo packet.PublicKeyAlgorithm) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity(name, "", email, fixtureConfig(v6, algo))
	if err != nil {
		t.Fatalf("generate %s: %v", name, err)
	}
	return e
}

func encryptFixture(t *testing.T, to *openpgp.Entity, config *packet.Config) []byte {
	t.Helper()
	return armorBytes(t, "PGP MESSAGE", func(w io.Writer) error {
		pt, err := openpgp.Encrypt(w, []*openpgp.Entity{to}, nil, &openpgp.FileHints{}, config)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(pt, fixtureText); err != nil {
			return err
		}
		return pt.Close()
	})
}

// buildPGPFixtures returns every fixture file by name. It must run in a test
// that owns the global random source.
func buildPGPFixtures(t *testing.T) map[string][]byte {
	t.Helper()
	cryptotest.SetGlobalRandom(t, fixtureSeed)
	keys := map[string]*openpgp.Entity{
		"v4":     newFixtureKey(t, "Fixture v4", "v4@example.org", false, packet.PubKeyAlgoEdDSA),
		"v6":     newFixtureKey(t, "Fixture v6", "v6@example.org", true, packet.PubKeyAlgoEd25519),
		"v6-pqc": newFixtureKey(t, "Fixture v6 PQC", "v6-pqc@example.org", true, packet.PubKeyAlgoMldsa65Ed25519),
		"v4-pqc": newFixtureKey(t, "Fixture v4 PQC subkey", "v4-pqc@example.org", false, packet.PubKeyAlgoEdDSA),
	}
	// RFC 9980 §3.5: ID 35 is the one PQ algorithm a v4 key may carry, as an
	// encryption subkey beside the classical one. A later creation time makes
	// it the subkey a sender selects.
	sub := fixtureConfig(false, packet.PubKeyAlgoMlkem768X25519)
	sub.Time = func() time.Time { return fixtureTime.Add(time.Minute) }
	if err := keys["v4-pqc"].AddEncryptionSubkey(sub); err != nil {
		t.Fatal(err)
	}

	files := map[string][]byte{"message.txt": []byte(fixtureText), "golden-incoming.eml": []byte(goldenMIME)}
	for _, name := range []string{"v4", "v6", "v6-pqc", "v4-pqc"} {
		e := keys[name]
		files[name+".pub.asc"] = armorBytes(t, openpgp.PublicKeyType, e.Serialize)
		files[name+".sec.asc"] = armorBytes(t, openpgp.PrivateKeyType, func(w io.Writer) error { return e.SerializePrivate(w, nil) })
	}
	// Messages and the signature are made an hour after the keys.
	later := func(v6 bool) *packet.Config {
		c := fixtureConfig(v6, 0)
		c.Time = func() time.Time { return fixtureTime.Add(time.Hour) }
		return c
	}
	files["v4.msg.asc"] = encryptFixture(t, keys["v4"], later(false))
	files["v4-pqc.msg.asc"] = encryptFixture(t, keys["v4-pqc"], later(false))
	files["v6-pqc.msg.asc"] = encryptFixture(t, keys["v6-pqc"], later(true))
	files["v6-pqc.sig.asc"] = armorBytes(t, openpgp.SignatureType, func(w io.Writer) error {
		return openpgp.DetachSign(w, keys["v6-pqc"], strings.NewReader(fixtureText), later(true))
	})
	// The golden starts from a fresh seed, as TestEncryptStoredMIMEV4Golden does.
	t.Run("golden", func(t *testing.T) {
		cryptotest.SetGlobalRandom(t, fixtureSeed)
		golden, err := pgpmail.EncryptStoredMIME([]byte(goldenMIME), string(files["v4.pub.asc"]))
		if err != nil {
			t.Fatal(err)
		}
		files["golden-incoming.v4.eml"] = golden
	})

	var sums strings.Builder
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		sum := sha256.Sum256(files[name])
		sums.WriteString(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	}
	files["SHA256SUMS"] = []byte(sums.String())
	return files
}

func TestPGPFixturesRegenerateByteIdentical(t *testing.T) {
	files := buildPGPFixtures(t)
	if regeneratePGPFixtures {
		if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, data := range files {
			if err := os.WriteFile(filepath.Join(fixtureDir, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(fixtureDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the generator's output; regenerate on purpose with -tags pgpfixturegen", name)
		}
	}
}

func readFixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readFixtureKey(t *testing.T, name string) *openpgp.Entity {
	t.Helper()
	list, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(readFixture(t, name)))
	if err != nil || len(list) != 1 {
		t.Fatalf("%s: %v (%d keys)", name, err, len(list))
	}
	return list[0]
}

func keyAlgos(e *openpgp.Entity) []packet.PublicKeyAlgorithm {
	algos := []packet.PublicKeyAlgorithm{e.PrimaryKey.PubKeyAlgo}
	for _, s := range e.Subkeys {
		algos = append(algos, s.PublicKey.PubKeyAlgo)
	}
	return algos
}

// TestPGPFixtureKeys pins each fixture key's version and algorithms, and that
// the allowlist accepts all four.
func TestPGPFixtureKeys(t *testing.T) {
	for name, want := range map[string]struct {
		version int
		algos   []packet.PublicKeyAlgorithm
	}{
		"v4":     {4, []packet.PublicKeyAlgorithm{packet.PubKeyAlgoEdDSA, packet.PubKeyAlgoECDH}},
		"v6":     {6, []packet.PublicKeyAlgorithm{packet.PubKeyAlgoEd25519, packet.PubKeyAlgoX25519}},
		"v6-pqc": {6, []packet.PublicKeyAlgorithm{packet.PubKeyAlgoMldsa65Ed25519, packet.PubKeyAlgoMlkem768X25519}},
		"v4-pqc": {4, []packet.PublicKeyAlgorithm{packet.PubKeyAlgoEdDSA, packet.PubKeyAlgoECDH, packet.PubKeyAlgoMlkem768X25519}},
	} {
		pub := readFixtureKey(t, name+".pub.asc")
		if pub.PrimaryKey.Version != want.version || !slices.Equal(keyAlgos(pub), want.algos) {
			t.Errorf("%s: v%d %v, want v%d %v", name, pub.PrimaryKey.Version, keyAlgos(pub), want.version, want.algos)
		}
		if _, err := pgpmail.InspectPublicKey(string(readFixture(t, name+".pub.asc"))); err != nil {
			t.Errorf("%s refused by the allowlist: %v", name, err)
		}
	}
}

// TestPGPFixtureMessages decrypts each fixture message with its key and pins
// the packet shape: PKESK version and target, SEIPD version, cipher and mode.
func TestPGPFixtureMessages(t *testing.T) {
	for _, tc := range []struct {
		key          string
		pkesk, seipd int
		subkeyAlgo   packet.PublicKeyAlgorithm
		cipher       packet.CipherFunction
		mode         packet.AEADMode
	}{
		{"v4", 3, 1, packet.PubKeyAlgoECDH, packet.CipherAES256, 0},
		{"v4-pqc", 3, 1, packet.PubKeyAlgoMlkem768X25519, packet.CipherAES256, 0},
		{"v6-pqc", 6, 2, packet.PubKeyAlgoMlkem768X25519, packet.CipherAES256, packet.AEADModeOCB},
	} {
		t.Run(tc.key, func(t *testing.T) {
			sec := readFixtureKey(t, tc.key+".sec.asc")
			block, err := armor.Decode(bytes.NewReader(readFixture(t, tc.key+".msg.asc")))
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(block.Body)
			pkesks, seipd := messagePackets(t, raw)
			if len(pkesks) != 1 || pkesks[0].Version != tc.pkesk || pkesks[0].Algo != tc.subkeyAlgo {
				t.Fatalf("PKESK %+v, want one v%d to algorithm %d", pkesks, tc.pkesk, tc.subkeyAlgo)
			}
			var target *openpgp.Subkey
			for i, s := range sec.Subkeys {
				if s.PublicKey.PubKeyAlgo == tc.subkeyAlgo {
					target = &sec.Subkeys[i]
				}
			}
			if target == nil || pkesks[0].KeyId != target.PublicKey.KeyId {
				t.Fatalf("PKESK names key %X, want the algorithm-%d subkey", pkesks[0].KeyId, tc.subkeyAlgo)
			}
			if tc.pkesk == 3 {
				if err := pkesks[0].Decrypt(target.PrivateKey, nil); err != nil || pkesks[0].CipherFunc != tc.cipher {
					t.Fatalf("v3 PKESK cipher %v (%v), want %v", pkesks[0].CipherFunc, err, tc.cipher)
				}
			}
			if seipd.Version != tc.seipd || (tc.seipd == 2 && (seipd.Cipher != tc.cipher || seipd.Mode != tc.mode)) {
				t.Fatalf("SEIPD v%d %v/%v, want v%d %v/%v", seipd.Version, seipd.Cipher, seipd.Mode, tc.seipd, tc.cipher, tc.mode)
			}
			md, err := openpgp.ReadMessage(bytes.NewReader(raw), openpgp.EntityList{sec}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(md.UnverifiedBody)
			if err != nil || string(body) != fixtureText {
				t.Fatalf("plaintext %q, %v", body, err)
			}
		})
	}
}

func messagePackets(t *testing.T, raw []byte) ([]*packet.EncryptedKey, *packet.SymmetricallyEncrypted) {
	t.Helper()
	var pkesks []*packet.EncryptedKey
	r := packet.NewReader(bytes.NewReader(raw))
	for {
		p, err := r.Next()
		if err != nil {
			t.Fatalf("read message packets: %v", err)
		}
		switch p := p.(type) {
		case *packet.EncryptedKey:
			pkesks = append(pkesks, p)
		case *packet.SymmetricallyEncrypted:
			return pkesks, p
		default:
			t.Fatalf("unexpected packet %T", p)
		}
	}
}

// TestPGPFixtureDetachedSignature verifies the v6 ML-DSA-65+Ed25519 detached
// signature and refuses it over altered text.
func TestPGPFixtureDetachedSignature(t *testing.T) {
	pub := readFixtureKey(t, "v6-pqc.pub.asc")
	sig := readFixture(t, "v6-pqc.sig.asc")
	config := &packet.Config{Time: func() time.Time { return fixtureTime.Add(time.Hour) }}
	got, err := openpgp.CheckArmoredDetachedSignature(openpgp.EntityList{pub}, strings.NewReader(fixtureText), bytes.NewReader(sig), config)
	if err != nil || got.PrimaryKey.Fingerprint == nil {
		t.Fatalf("verify: %v", err)
	}
	block, _ := armor.Decode(bytes.NewReader(sig))
	p, err := packet.Read(block.Body)
	if err != nil {
		t.Fatal(err)
	}
	if s := p.(*packet.Signature); s.Version != 6 || s.PubKeyAlgo != packet.PubKeyAlgoMldsa65Ed25519 {
		t.Fatalf("signature v%d algorithm %d, want v6 algorithm 30", s.Version, s.PubKeyAlgo)
	}
	if _, err := openpgp.CheckArmoredDetachedSignature(openpgp.EntityList{pub}, strings.NewReader(fixtureText+"x"), bytes.NewReader(sig), config); err == nil {
		t.Fatal("signature verified over altered text")
	}
}

// TestEncryptStoredMIMEV4Golden pins incoming encryption to a v4 key byte for
// byte: same seed, same key, same message, same output as before the PQC
// work. It also decrypts the golden with the fixture key.
func TestEncryptStoredMIMEV4Golden(t *testing.T) {
	cryptotest.SetGlobalRandom(t, fixtureSeed)
	got, err := pgpmail.EncryptStoredMIME([]byte(goldenMIME), string(readFixture(t, "v4.pub.asc")))
	if err != nil {
		t.Fatal(err)
	}
	want := readFixture(t, "golden-incoming.v4.eml")
	if !bytes.Equal(got, want) {
		t.Fatalf("incoming encryption to a v4 key changed:\n got %q\nwant %q", got, want)
	}
	sec, err := crypto.NewKeyFromArmored(string(readFixture(t, "v4.sec.asc")))
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(got, []byte("-----BEGIN PGP MESSAGE-----"))
	end := bytes.Index(got, []byte("-----END PGP MESSAGE-----"))
	if start < 0 || end < 0 {
		t.Fatal("golden has no armored message")
	}
	dec, err := crypto.PGP().Decryption().DecryptionKey(sec).New()
	if err != nil {
		t.Fatal(err)
	}
	plain, err := dec.Decrypt(got[start:end+len("-----END PGP MESSAGE-----")], crypto.Armor)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plain.Bytes(), []byte("Golden body.")) {
		t.Fatalf("golden plaintext %q", plain.Bytes())
	}
}
