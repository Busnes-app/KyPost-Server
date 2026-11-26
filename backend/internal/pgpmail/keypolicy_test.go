package pgpmail_test

import (
	"errors"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
)

func TestKeyPolicyAllowlist(t *testing.T) {
	keys, err := pgpmailtest.PolicyKeys("policy@example.org")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		t.Run(k.Name, func(t *testing.T) {
			err := pgpmail.CheckArmoredKeyPolicy(k.Armored)
			if k.Allowed != (err == nil) || (err != nil && !errors.Is(err, pgpmail.ErrKeyPolicy)) {
				t.Fatalf("allowed=%v, got %v", k.Allowed, err)
			}
			_, err = pgpmail.InspectPublicKey(k.Armored)
			if k.Allowed != (err == nil) {
				t.Fatalf("InspectPublicKey: allowed=%v, got %v", k.Allowed, err)
			}
		})
	}
}
