package cryptutil_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"os"
	"testing"
	"testing/cryptotest"

	"github.com/Busnes-app/ky-primitives/xwing"
)

type xwingFixture struct {
	Description string       `json:"description"`
	Seed        string       `json:"seed"`
	PublicKey   string       `json:"publicKey"`
	DeviceID    string       `json:"deviceId"`
	Fingerprint string       `json:"fingerprint"`
	Bucket      uint64       `json:"bucket"`
	SAS         string       `json:"sas"`
	Plaintext   string       `json:"plaintext"`
	Envelope    hpkeEnvelope `json:"envelope"`
}

// Independent SAS v2 definition, shared only through public fixture bytes.
func xwingSAS(key []byte, id string, bucket uint64) string {
	preimage := []byte("kypost-device-sas/v2")
	preimage = binary.BigEndian.AppendUint16(preimage, uint16(len(key)))
	preimage = append(preimage, key...)
	preimage = binary.BigEndian.AppendUint16(preimage, uint16(len(id)))
	preimage = append(preimage, id...)
	preimage = binary.BigEndian.AppendUint64(preimage, bucket)
	digest := sha256.Sum256(preimage)
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	out := make([]byte, 14)
	for i := range out {
		var v byte
		for bit := i * 5; bit < (i+1)*5; bit++ {
			v = v<<1 | ((digest[bit/8] >> (7 - bit%8)) & 1)
		}
		out[i] = alphabet[v]
	}
	return string(out)
}

func TestXWingEnrollmentVectors(t *testing.T) {
	const path = "../../../testdata/device-envelope-xwing.json"
	if os.Getenv("KYPOST_WRITE_XWING_FIXTURE") == "1" {
		cryptotest.SetGlobalRandom(t, 0x647a)
		key, err := xwing.FromSeed(bytes.Repeat([]byte{0x65}, 32))
		if err != nil {
			t.Fatal(err)
		}
		f := xwingFixture{Description: "PUBLIC TEST SECRETS ONLY. Go-sealed X-Wing HPKE v6 (KEM 0x647A, HKDF-SHA256, AES-256-GCM), empty HPKE aad. seed is not a production key. Regenerate: KYPOST_WRITE_XWING_FIXTURE=1 go test ./internal/cryptutil -run TestXWingEnrollmentVectors", Seed: b64(key.Seed()), PublicKey: b64(key.Public().Bytes()), DeviceID: "test-device", Fingerprint: "0123456789ABCDEF0123456789ABCDEF01234567", Bucket: 14000000, Plaintext: "public test enrollment payload"}
		f.SAS = xwingSAS(key.Public().Bytes(), f.DeviceID, f.Bucket)
		sealed, err := xwing.Seal(key.Public(), hpkeInfo(6, f.DeviceID, f.Fingerprint), nil, []byte(f.Plaintext))
		if err != nil {
			t.Fatal(err)
		}
		f.Envelope = hpkeEnvelope{6, "HPKE-Base-XWing-SHA256-A256GCM", b64(sealed[1:1121]), b64(sealed[1121:])}
		data, err := json.MarshalIndent(f, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f xwingFixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	key, err := xwing.FromSeed(mustB64(t, f.Seed))
	if err != nil {
		t.Fatal(err)
	}
	if b64(key.Public().Bytes()) != f.PublicKey || xwingSAS(key.Public().Bytes(), f.DeviceID, f.Bucket) != f.SAS {
		t.Fatal("key/SAS vector mismatch")
	}
	for i := range xwing.PublicKeyBytes {
		changed := key.Public().Bytes()
		changed[i] ^= 1
		if xwingSAS(changed, f.DeviceID, f.Bucket) == f.SAS {
			t.Fatalf("SAS omitted key byte %d", i)
		}
	}
	check := func(e hpkeEnvelope) {
		t.Helper()
		if e.V != 6 || e.Alg != "HPKE-Base-XWing-SHA256-A256GCM" {
			t.Fatal("wrong suite")
		}
		enc, ct := mustB64(t, e.Enc), mustB64(t, e.CT)
		if len(enc) != 1120 {
			t.Fatal("wrong enc size")
		}
		sealed := append([]byte{xwing.Version}, enc...)
		sealed = append(sealed, ct...)
		info := hpkeInfo(6, f.DeviceID, f.Fingerprint)
		pt, err := xwing.Open(key, info, nil, sealed)
		if err != nil || string(pt) != f.Plaintext {
			t.Fatalf("open: %q %v", pt, err)
		}
		for _, binding := range [][]byte{hpkeInfo(6, "other-device", f.Fingerprint), hpkeInfo(6, f.DeviceID, "F"+f.Fingerprint[1:]), hpkeInfo(5, f.DeviceID, f.Fingerprint)} {
			if _, err := xwing.Open(key, binding, nil, sealed); err == nil {
				t.Fatal("opened under wrong binding")
			}
		}
	}
	check(f.Envelope)
	if os.Getenv("KYPOST_WRITE_XWING_FIXTURE") == "1" {
		return
	}
	data, err = os.ReadFile("../../../testdata/device-envelope-xwing-browser.json")
	if err != nil {
		t.Fatal(err)
	}
	var browser hpkeEnvelope
	if err := json.Unmarshal(data, &browser); err != nil {
		t.Fatal(err)
	}
	check(browser)
}
