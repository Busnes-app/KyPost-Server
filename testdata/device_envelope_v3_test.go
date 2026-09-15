package testdata

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// Independent stdlib implementation for native consumers to check their ports.
// All private scalars in the fixture are public test data.
func TestDeviceEnvelopeVectors(t *testing.T) {
	data, err := os.ReadFile("device-envelope-v3.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		DevicePrivateKey, DevicePublicKey, EphemeralPrivateKey string
		Vectors                                                []struct {
			DeviceID, Fingerprint, Plaintext, SharedSecret, AESKey, AAD string
			Envelope                                                    struct {
				V                int
				Alg, Epk, IV, CT string
			}
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	decode := func(s string) []byte {
		t.Helper()
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	device, err := ecdh.P256().NewPrivateKey(decode(fixture.DevicePrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	ephemeral, err := ecdh.P256().NewPrivateKey(decode(fixture.EphemeralPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(device.PublicKey().Bytes(), decode(fixture.DevicePublicKey)) {
		t.Fatal("device public point differs")
	}
	if len(fixture.Vectors) != 2 {
		t.Fatal("need v2 and v3 vectors")
	}
	for i, v := range fixture.Vectors {
		t.Run(fmt.Sprintf("v%d", v.Envelope.V), func(t *testing.T) {
			if v.Envelope.V != i+2 || v.Envelope.Alg != "ECDH-P256+HKDF-SHA256+A256GCM" {
				t.Fatal("unsupported format")
			}
			if !bytes.Equal(ephemeral.PublicKey().Bytes(), decode(v.Envelope.Epk)) {
				t.Fatal("ephemeral point differs")
			}
			shared, err := device.ECDH(ephemeral.PublicKey())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(shared, decode(v.SharedSecret)) {
				t.Fatal("ECDH differs")
			}
			domain := fmt.Sprintf("kypost-device-envelope/v%d", v.Envelope.V)
			key, err := hkdf.Key(sha256.New, shared, device.PublicKey().Bytes(), domain, 32)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(key, decode(v.AESKey)) {
				t.Fatal("HKDF differs")
			}
			aad := []byte(domain)
			for _, field := range []string{v.DeviceID, v.Fingerprint} {
				if len(field) > 65535 {
					t.Fatal("field too long")
				}
				aad = binary.BigEndian.AppendUint16(aad, uint16(len(field)))
				aad = append(aad, []byte(field)...)
			}
			if !bytes.Equal(aad, decode(v.AAD)) {
				t.Fatal("AAD differs")
			}
			block, err := aes.NewCipher(key)
			if err != nil {
				t.Fatal(err)
			}
			gcm, err := cipher.NewGCM(block)
			if err != nil {
				t.Fatal(err)
			}
			iv, ct := decode(v.Envelope.IV), decode(v.Envelope.CT)
			if !bytes.Equal(gcm.Seal(nil, iv, []byte(v.Plaintext), aad), ct) {
				t.Fatal("ciphertext differs")
			}
			opened, err := gcm.Open(nil, iv, ct, aad)
			if err != nil || string(opened) != v.Plaintext {
				t.Fatal("plaintext differs", err)
			}
			aad[len(aad)-1] ^= 1
			if _, err := gcm.Open(nil, iv, ct, aad); err == nil {
				t.Fatal("accepted changed fingerprint")
			}
		})
	}
}
