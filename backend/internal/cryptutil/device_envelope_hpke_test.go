package cryptutil_test

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hpke"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"testing/cryptotest"
)

// HPKE device envelopes (v4/v5): RFC 9180 mode_base, DHKEM(P-256,
// HKDF-SHA256), HKDF-SHA256, AES-256-GCM. Native openers check their ports
// against testdata/device-envelope-hpke.json; all private scalars there are
// public test data.

const (
	hpkeFixturePath = "../../../testdata/device-envelope-hpke.json"
	hpkeEnvelopeAlg = "HPKE-Base-P256-SHA256-A256GCM"
	hpkeFixtureSeed = 4
)

type hpkeEnvelope struct {
	V   int    `json:"v"`
	Alg string `json:"alg"`
	Enc string `json:"enc"`
	CT  string `json:"ct"`
}

type hpkeVector struct {
	Name        string       `json:"name"`
	DeviceID    string       `json:"deviceId"`
	Fingerprint string       `json:"fingerprint"`
	Plaintext   string       `json:"plaintext,omitempty"`
	Info        string       `json:"info"`
	Envelope    hpkeEnvelope `json:"envelope"`
	// RejectedBy is "framing" (server/opener envelope checks) or "open"
	// (HPKE decapsulation or AEAD); empty for positives.
	RejectedBy string `json:"rejectedBy,omitempty"`
}

type hpkeFixture struct {
	Description      string       `json:"description"`
	Generator        string       `json:"generator"`
	Suite            string       `json:"suite"`
	DevicePrivateKey string       `json:"devicePrivateKey"`
	DevicePublicKey  string       `json:"devicePublicKey"`
	Vectors          []hpkeVector `json:"vectors"`
	Negative         []hpkeVector `json:"negative"`
}

// hpkeInfo is envelopeAad: "kypost-device-envelope/v<v>" || u16be-prefixed
// device ID || u16be-prefixed fingerprint.
func hpkeInfo(v int, deviceID, fingerprint string) []byte {
	info := fmt.Appendf(nil, "kypost-device-envelope/v%d", v)
	for _, f := range []string{deviceID, fingerprint} {
		info = binary.BigEndian.AppendUint16(info, uint16(len(f)))
		info = append(info, f...)
	}
	return info
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// buildHPKEFixture derives the HPKE fixture from the v2/v3 fixture's device
// key and plaintexts under seeded randomness, so it is reproducible on the
// pinned toolchain.
func buildHPKEFixture(t *testing.T) []byte {
	data, err := os.ReadFile("../../../testdata/device-envelope-v3.json")
	if err != nil {
		t.Fatal(err)
	}
	var legacy struct {
		DevicePrivateKey, DevicePublicKey string
		Vectors                           []struct{ DeviceID, Fingerprint, Plaintext string }
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	priv, err := hpke.DHKEM(ecdh.P256()).NewPrivateKey(mustB64(t, legacy.DevicePrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	cryptotest.SetGlobalRandom(t, hpkeFixtureSeed)
	f := hpkeFixture{
		Description:      "PUBLIC TEST SECRETS ONLY. v4/v5 HPKE device envelopes for interoperability tests; never use for real enrollment. Open with the recipient's own deviceId/fingerprint and the delivery version, not values read from the envelope.",
		Generator:        "go test -tags hpkegen -run TestGenerateHPKEFixture ./internal/cryptutil/ (backend/internal/cryptutil/device_envelope_hpke_test.go)",
		Suite:            "RFC 9180 mode_base, KEM 0x0010 DHKEM(P-256, HKDF-SHA256), KDF 0x0001 HKDF-SHA256, AEAD 0x0002 AES-256-GCM; info = envelope AAD, HPKE aad empty",
		DevicePrivateKey: legacy.DevicePrivateKey,
		DevicePublicKey:  legacy.DevicePublicKey,
	}
	for i, l := range legacy.Vectors {
		v := i + 4
		info := hpkeInfo(v, l.DeviceID, l.Fingerprint)
		sealed, err := hpke.Seal(priv.PublicKey(), hpke.HKDFSHA256(), hpke.AES256GCM(), info, []byte(l.Plaintext))
		if err != nil {
			t.Fatal(err)
		}
		f.Vectors = append(f.Vectors, hpkeVector{
			Name: fmt.Sprintf("v%d", v), DeviceID: l.DeviceID, Fingerprint: l.Fingerprint,
			Plaintext: l.Plaintext, Info: b64(info),
			Envelope: hpkeEnvelope{V: v, Alg: hpkeEnvelopeAlg, Enc: b64(sealed[:65]), CT: b64(sealed[65:])},
		})
	}
	v4 := f.Vectors[0]
	negative := func(name, deviceID, fingerprint, rejectedBy string, env hpkeEnvelope) {
		f.Negative = append(f.Negative, hpkeVector{
			Name: name, DeviceID: deviceID, Fingerprint: fingerprint,
			Info: b64(hpkeInfo(env.V, deviceID, fingerprint)), Envelope: env, RejectedBy: rejectedBy,
		})
	}
	negative("wrong device ID", "dev-2", v4.Fingerprint, "open", v4.Envelope)
	negative("wrong fingerprint", v4.DeviceID, "0000000000000000000000000000000000000000", "open", v4.Envelope)
	relabelled := v4.Envelope
	relabelled.V = 5
	negative("v4 relabelled v5", v4.DeviceID, v4.Fingerprint, "open", relabelled)
	wrongAlg := v4.Envelope
	wrongAlg.Alg = "ECDH-P256+HKDF-SHA256+A256GCM"
	negative("wrong alg", v4.DeviceID, v4.Fingerprint, "framing", wrongAlg)
	offCurve := v4.Envelope
	// (1, 1) is not on P-256: right length and prefix, refused by the KEM.
	point := make([]byte, 65)
	point[0], point[32], point[64] = 0x04, 1, 1
	offCurve.Enc = b64(point)
	negative("enc not on curve", v4.DeviceID, v4.Fingerprint, "open", offCurve)

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(f); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func loadHPKEFixture(t *testing.T) (hpkeFixture, hpke.PrivateKey) {
	t.Helper()
	data, err := os.ReadFile(hpkeFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var f hpkeFixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	priv, err := hpke.DHKEM(ecdh.P256()).NewPrivateKey(mustB64(t, f.DevicePrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(priv.PublicKey().Bytes(), mustB64(t, f.DevicePublicKey)) {
		t.Fatal("device public point differs")
	}
	return f, priv
}

// openHPKEEnvelope is a reference opener: framing first, then HPKE with info
// built from the recipient's own identity and the delivery version.
func openHPKEEnvelope(t *testing.T, priv hpke.PrivateKey, version int, deviceID, fingerprint string, e hpkeEnvelope) (string, []byte, error) {
	t.Helper()
	enc, ct := mustB64(t, e.Enc), mustB64(t, e.CT)
	if e.V != version || (e.V != 4 && e.V != 5) || e.Alg != hpkeEnvelopeAlg || len(enc) != 65 || enc[0] != 0x04 || len(ct) < 16 {
		return "framing", nil, fmt.Errorf("bad framing")
	}
	pt, err := hpke.Open(priv, hpke.HKDFSHA256(), hpke.AES256GCM(), hpkeInfo(version, deviceID, fingerprint), append(enc, ct...))
	return "open", pt, err
}

func TestHPKECFRGVector(t *testing.T) {
	data, err := os.ReadFile("../../../testdata/hpke-cfrg-p256-aes256.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Vector struct {
			Info, SkRm, PkRm, Enc string
			Encryptions           []struct{ AAD, CT, PT string }
		}
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	unhex := func(s string) []byte {
		t.Helper()
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	v := f.Vector
	priv, err := hpke.DHKEM(ecdh.P256()).NewPrivateKey(unhex(v.SkRm))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(priv.PublicKey().Bytes(), unhex(v.PkRm)) {
		t.Fatal("pkRm differs")
	}
	r, err := hpke.NewRecipient(unhex(v.Enc), priv, hpke.HKDFSHA256(), hpke.AES256GCM(), unhex(v.Info))
	if err != nil {
		t.Fatal(err)
	}
	// Sequence numbers advance per Open, so every encryption is opened in order.
	for i, e := range v.Encryptions {
		pt, err := r.Open(unhex(e.AAD), unhex(e.CT))
		if err != nil || !bytes.Equal(pt, unhex(e.PT)) {
			t.Fatalf("encryption %d: %v", i, err)
		}
	}
}

func TestDeviceEnvelopeHPKEVectors(t *testing.T) {
	f, priv := loadHPKEFixture(t)
	if len(f.Vectors) != 2 || f.Vectors[0].Envelope.V != 4 || f.Vectors[1].Envelope.V != 5 {
		t.Fatal("need v4 and v5 vectors")
	}
	for _, v := range f.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			info := hpkeInfo(v.Envelope.V, v.DeviceID, v.Fingerprint)
			if !bytes.Equal(info, mustB64(t, v.Info)) {
				t.Fatal("info differs")
			}
			_, pt, err := openHPKEEnvelope(t, priv, v.Envelope.V, v.DeviceID, v.Fingerprint, v.Envelope)
			if err != nil || string(pt) != v.Plaintext {
				t.Fatal("open failed", err)
			}
		})
	}
	if len(f.Negative) != 5 {
		t.Fatal("negative cases missing")
	}
	for _, n := range f.Negative {
		t.Run(n.Name, func(t *testing.T) {
			stage, _, err := openHPKEEnvelope(t, priv, n.Envelope.V, n.DeviceID, n.Fingerprint, n.Envelope)
			if err == nil || stage != n.RejectedBy {
				t.Fatalf("stage %q err %v, want rejection by %q", stage, err, n.RejectedBy)
			}
			if !bytes.Equal(hpkeInfo(n.Envelope.V, n.DeviceID, n.Fingerprint), mustB64(t, n.Info)) {
				t.Fatal("info differs")
			}
		})
	}
}

func TestDeviceEnvelopeHPKEFixtureReproducible(t *testing.T) {
	want, err := os.ReadFile(hpkeFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buildHPKEFixture(t), want) {
		t.Fatal("generator output differs from testdata/device-envelope-hpke.json; regenerate only for a deliberate fixture change")
	}
}
