package pgpmail_test

import (
	"path/filepath"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
)

func TestSealOpenPrivateKeyRoundTrip(t *testing.T) {
	id, err := pgpmailtest.GenerateIdentity("Bob", "bob@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "pgp-private-key.key")

	sealed, err := id.SealPrivateKey(keyPath)
	if err != nil {
		t.Fatalf("SealPrivateKey: %v", err)
	}
	if sealed == "" {
		t.Fatal("expected non-empty sealed envelope")
	}

	opened, err := pgpmail.OpenPrivateKey(sealed, keyPath)
	if err != nil {
		t.Fatalf("OpenPrivateKey: %v", err)
	}
	if opened.Fingerprint != id.Fingerprint {
		t.Fatalf("fingerprint mismatch: got %s want %s", opened.Fingerprint, id.Fingerprint)
	}
}
