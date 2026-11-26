package api

import (
	"fmt"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/contacts"
	"github.com/ProtonMail/gopenpgp/v3/crypto"
	"github.com/ProtonMail/gopenpgp/v3/profile"
)

// A contact with a v4 and a v6 key ships both, each its own entry with its
// own provenance.
func TestSignerKeysShipBothSlots(t *testing.T) {
	store := must1(contacts.New(t.TempDir()))
	k4, k6 := testV4Key(t, "bob@example.com"), testV6Key(t, "bob@example.com", true)
	c := must1(store.Upsert(contacts.Contact{FormattedName: "Bob", Emails: []contacts.ContactValue{{Value: "bob@example.com"}}, PGPKey: k4, PGPKeySource: "qr", PGPKeyVerified: true}))
	c.PGPKey, c.PGPKeySource = k6, contacts.PGPSourceWKD
	must1(store.Upsert(c))

	got := must1(boundSignerKeysForSender(store, "bob@example.com"))
	if len(got) != 2 || got[0].PublicKey != k4 || !got[0].Verified || got[1].PublicKey == "" || got[1].Source != contacts.PGPSourceWKD || got[1].Verified {
		t.Fatalf("signer keys = %+v", got)
	}
}

// The parse cache is keyed by the key's bytes, so a contact change that keeps
// the old pin on new key material (an Autocrypt rotation) is a conflict, not a
// cached match.
func TestSignerKeyCacheFollowsKeyBytesNotPin(t *testing.T) {
	store := must1(contacts.New(t.TempDir()))
	k1 := testV4Key(t, "bob@example.com")
	c := must1(store.Upsert(contacts.Contact{FormattedName: "Bob", Emails: []contacts.ContactValue{{Value: "bob@example.com"}}, PGPKey: k1}))
	if got := must1(boundSignerKeys(store)); len(got) != 1 || got[0].Conflict {
		t.Fatalf("first read = %+v", got)
	}
	c.PGPKey = testV4Key(t, "bob@example.com") // new bytes, old pin kept
	c.PGPKeySource = contacts.PGPSourceAutocrypt
	must1(store.Upsert(c))
	if got := must1(boundSignerKeys(store)); len(got) != 1 || !got[0].Conflict || got[0].PublicKey != "" {
		t.Fatalf("rotated key under the old pin = %+v, want a conflict", got)
	}
}

// BenchmarkSignerKeys1000 measures the signer binding over 1,000 contacts with
// v6 PQC keys: bootstrap (every contact) and pgp-payload (one sender).
func BenchmarkSignerKeys1000(b *testing.B) {
	store, err := contacts.New(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	pqc := crypto.PGPWithProfile(profile.PQC())
	ops := make([]contacts.BatchOp, 0, 1000)
	for i := range 1000 {
		addr := fmt.Sprintf("c%d@example.com", i)
		key, err := pqc.KeyGeneration().AddUserId("C", addr).New().GenerateKey()
		if err != nil {
			b.Fatal(err)
		}
		pub, err := key.GetArmoredPublicKey()
		if err != nil {
			b.Fatal(err)
		}
		ops = append(ops, contacts.BatchOp{Contact: contacts.Contact{FormattedName: addr, Emails: []contacts.ContactValue{{Value: addr}}, PGPKey: pub}})
	}
	if err := store.ApplyBatch(ops); err != nil {
		b.Fatal(err)
	}
	b.Run("bootstrap", func(b *testing.B) {
		for b.Loop() {
			if keys, err := boundSignerKeys(store); err != nil || len(keys) != 1000 {
				b.Fatal(err, len(keys))
			}
		}
	})
	b.Run("pgp-payload", func(b *testing.B) {
		for b.Loop() {
			if keys, err := boundSignerKeysForSender(store, "c500@example.com"); err != nil || len(keys) != 1 {
				b.Fatal(err, len(keys))
			}
		}
	})
}
