package pgpmail_test

import (
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ProtonMail/gopenpgp/v3/crypto"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
)

func TestCheckKeyStatusUsableKey(t *testing.T) {
	id, err := pgpmailtest.GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	status, err := pgpmail.CheckKeyStatus(id.ArmoredPublicKey)
	if err != nil {
		t.Fatalf("CheckKeyStatus: %v", err)
	}
	if status.Revoked || status.Expired || !status.Usable() {
		t.Fatalf("expected a fresh key to be usable, got %+v", status)
	}
}

func TestCheckKeyStatusExpiredKey(t *testing.T) {
	past := time.Now().Add(-48 * time.Hour)
	key, err := crypto.PGP().KeyGeneration().
		GenerationTime(past.Unix()).
		Lifetime(3600).
		AddUserId("Expired", "expired@example.com").
		New().GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	armored, err := key.GetArmoredPublicKey()
	if err != nil {
		t.Fatalf("GetArmoredPublicKey: %v", err)
	}

	status, err := pgpmail.CheckKeyStatus(armored)
	if err != nil {
		t.Fatalf("CheckKeyStatus: %v", err)
	}
	if !status.Expired || status.Usable() {
		t.Fatalf("expected a key generated 48h ago with a 1h lifetime to be expired and unusable, got %+v", status)
	}
}

func TestCheckKeyStatusRevokedKey(t *testing.T) {
	key, err := crypto.PGP().KeyGeneration().AddUserId("Revoked", "revoked@example.com").New().GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if err := key.GetEntity().Revoke(packet.NoReason, "test revocation", &packet.Config{}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	armored, err := key.GetArmoredPublicKey()
	if err != nil {
		t.Fatalf("GetArmoredPublicKey: %v", err)
	}

	status, err := pgpmail.CheckKeyStatus(armored)
	if err != nil {
		t.Fatalf("CheckKeyStatus: %v", err)
	}
	if !status.Revoked || status.Usable() {
		t.Fatalf("expected a revoked key to be reported revoked and unusable, got %+v", status)
	}
}

func TestCheckKeyStatusInvalidArmor(t *testing.T) {
	if _, err := pgpmail.CheckKeyStatus("not a real armored key"); err == nil {
		t.Fatal("expected an error parsing invalid armored text")
	}
}

func TestIdentityStatusUsable(t *testing.T) {
	id, err := pgpmailtest.GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	if status := id.Status(); !status.Usable() {
		t.Fatalf("expected a fresh identity to be usable, got %+v", status)
	}
}
