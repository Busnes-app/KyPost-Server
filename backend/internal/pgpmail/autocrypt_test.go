package pgpmail_test

import (
	"bytes"
	"crypto"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
)

// TestAutocryptKeydataRefusesRevokedUserID: a minimal key would carry the
// User ID without its revocation, so a revoked From User ID is not advertised.
func TestAutocryptKeydataRefusesRevokedUserID(t *testing.T) {
	created := time.Now().Add(-time.Hour)
	config := &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519, Time: func() time.Time { return created }}
	e, err := openpgp.NewEntity("Bob", "", "bob@example.com", config)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AddUserId("Bob", "", "old@example.com", config); err != nil {
		t.Fatal(err)
	}
	old := e.Identities["Bob <old@example.com>"]
	rev := &packet.Signature{
		Version: 4, SigType: packet.SigTypeCertificationRevocation, PubKeyAlgo: e.PrimaryKey.PubKeyAlgo,
		Hash: crypto.SHA256, CreationTime: time.Now().Add(-time.Minute), IssuerKeyId: &e.PrimaryKey.KeyId,
	}
	if err := rev.SignUserId(old.UserId.Id, e.PrimaryKey, e.PrivateKey, nil); err != nil {
		t.Fatal(err)
	}
	old.Signatures = append(old.Signatures, rev)
	var b bytes.Buffer
	w, _ := armor.Encode(&b, openpgp.PublicKeyType, nil)
	if err := e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	if _, err := pgpmail.AutocryptKeydata(b.String(), "bob@example.com"); err != nil {
		t.Fatalf("live User ID refused: %v", err)
	}
	if _, err := pgpmail.AutocryptKeydata(b.String(), "old@example.com"); err == nil {
		t.Fatal("revoked User ID advertised without its revocation")
	}
}
