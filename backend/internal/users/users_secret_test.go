package users

import (
	"context"
	"testing"
)

func TestDeviceSecretRoundTrips(t *testing.T) {
	const secret = "9f8e7d6c5b4a39281706f5e4d3c2b1a0ffeeddccbbaa9988"
	stored := HashDeviceSecret(secret)
	if stored == secret {
		t.Fatal("HashDeviceSecret returned the secret unchanged")
	}
	if ok, _ := VerifyDeviceSecret(context.Background(), stored, secret); !ok {
		t.Fatal("VerifyDeviceSecret rejected the secret it just hashed")
	}
	if ok, _ := VerifyDeviceSecret(context.Background(), stored, secret+"x"); ok {
		t.Fatal("VerifyDeviceSecret accepted a wrong secret")
	}
	if ok, _ := VerifyDeviceSecret(context.Background(), stored, ""); ok {
		t.Fatal("VerifyDeviceSecret accepted an empty secret")
	}
}

func TestVerifyDeviceSecretRejectsUntaggedAndMalformedHashes(t *testing.T) {
	for _, stored := range []string{"sha256:", "sha256:zzzz", "sha256:abcd", retiredScryptHash, ""} {
		if ok, err := VerifyDeviceSecret(context.Background(), stored, "anything"); ok || err != nil {
			t.Errorf("VerifyDeviceSecret(%q): ok=%v err=%v, want false,nil", stored, ok, err)
		}
	}
}
