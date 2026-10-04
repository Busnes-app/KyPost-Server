package cryptutil

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestOpenRefusesMalformedNonceWithoutPanic(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	envelope, err := Seal([]byte("private data"), key)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 1, 11, 13, 24} {
		corrupt := envelope
		corrupt.Nonce = base64.StdEncoding.EncodeToString(make([]byte, size))
		if _, err := Open(corrupt, key); err == nil {
			t.Fatalf("accepted nonce size %d", size)
		}
	}
	plain, err := Open(envelope, key)
	if err != nil || string(plain) != "private data" {
		t.Fatal("valid envelope failed", err)
	}
}
