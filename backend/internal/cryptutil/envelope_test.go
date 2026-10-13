package cryptutil

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSealOpenRoundTripAndRefusals(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "master.key")
	errNotEnv := errors.New("not an envelope")

	enc, err := SealString("secret", keyPath)
	if err != nil {
		t.Fatalf("SealString: %v", err)
	}
	if got, err := OpenString(enc, keyPath, errNotEnv); err != nil || got != "secret" {
		t.Fatalf("OpenString = %q, %v", got, err)
	}
	if got, err := OpenBytes([]byte(enc), keyPath); err != nil || string(got) != "secret" {
		t.Fatalf("OpenBytes = %q, %v", got, err)
	}

	env, ok := ParseEnvelope([]byte(enc))
	if !ok {
		t.Fatal("ParseEnvelope rejected a sealed envelope")
	}
	ct, _ := base64.StdEncoding.DecodeString(env.Ciphertext)
	ct[0] ^= 1
	env.Ciphertext = base64.StdEncoding.EncodeToString(ct)
	tampered, _ := json.Marshal(env)
	if _, err := OpenString(string(tampered), keyPath, errNotEnv); err == nil {
		t.Fatal("tampered ciphertext opened")
	}

	otherKey := filepath.Join(dir, "other.key")
	if _, err := LoadOrCreateKey(otherKey); err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	if _, err := OpenBytes([]byte(enc), otherKey); err == nil {
		t.Fatal("opened under the wrong key")
	}

	// A decrypt path must never mint a key.
	missing := filepath.Join(dir, "missing.key")
	if _, err := OpenString(enc, missing, errNotEnv); err == nil {
		t.Fatal("opened with no key on disk")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenString created a key file: %v", err)
	}

	if _, err := OpenString("plain", keyPath, errNotEnv); err != errNotEnv {
		t.Fatalf("OpenString(non-envelope) = %v, want caller's error", err)
	}
	if _, err := OpenBytes([]byte("plain"), keyPath); !errors.Is(err, ErrNotEncrypted) {
		t.Fatalf("OpenBytes(non-envelope) = %v, want ErrNotEncrypted", err)
	}
}

func TestParseEnvelopeRejectsMalformed(t *testing.T) {
	for _, raw := range []string{
		``,
		`not json`,
		`{"version":2,"nonce":"bg==","ciphertext":"Yw=="}`,
		`{"version":1,"nonce":" ","ciphertext":"Yw=="}`,
		`{"version":1,"nonce":"bg==","ciphertext":""}`,
	} {
		if _, ok := ParseEnvelope([]byte(raw)); ok {
			t.Errorf("ParseEnvelope(%q) accepted", raw)
		}
	}
}
