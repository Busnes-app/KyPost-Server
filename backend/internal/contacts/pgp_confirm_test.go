package contacts

import (
	"errors"
	"testing"
)

// An Autocrypt key is confirmed only with its own pinned fingerprint, and
// removing it then pasting it back never promotes it without that step.
func TestConfirmPGPKeyAndRemovedAutocryptKey(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key, fp := privateTestKey(t, "bob@example.com")
	other, _ := privateTestKey(t, "bob@example.com")
	bob, err := store.Upsert(Contact{FormattedName: "Bob", Emails: []ContactValue{{Value: "bob@example.com"}}, PGPKey: key, PGPKeyFingerprint: fp, PGPKeySource: PGPSourceAutocrypt})
	if err != nil {
		t.Fatal(err)
	}

	// Clear, save, paste the same key back: still unconfirmed Autocrypt.
	cleared, err := store.Upsert(Contact{UID: bob.UID, FormattedName: "Bob", Emails: bob.Emails})
	if err != nil || cleared.PGPKeyRemovedAutocrypt != fp {
		t.Fatalf("clear: %v %+v", err, cleared)
	}
	pasted, err := store.Upsert(Contact{UID: bob.UID, FormattedName: "Bob", Emails: bob.Emails, PGPKey: key})
	if err != nil || pasted.PGPKeySource != PGPSourceAutocrypt || pasted.PGPKeyVerified || pasted.PGPKeyRemovedAutocrypt != "" {
		t.Fatalf("re-pasted key was promoted: %v %+v", err, pasted)
	}

	if _, found, err := store.ConfirmPGPKey(bob.UID, "DEADBEEF"); !found || !errors.Is(err, ErrPGPKeyMismatch) {
		t.Fatalf("wrong fingerprint: found %v err %v", found, err)
	}
	if _, found, err := store.ConfirmPGPKey("no-such-uid", fp); found || err != nil {
		t.Fatalf("missing contact: found %v err %v", found, err)
	}
	spaced := fp[:4] + " " + fp[4:]
	confirmed, found, err := store.ConfirmPGPKey(bob.UID, spaced)
	if err != nil || !found || !confirmed.PGPKeyVerified || confirmed.PGPKeySource != PGPSourceAutocrypt || confirmed.Rev <= pasted.Rev {
		t.Fatalf("confirm: found %v err %v %+v", found, err, confirmed)
	}

	// A confirmed key that is removed is forgotten; a different key is the user's own paste.
	if c, _ := store.Upsert(Contact{UID: bob.UID, FormattedName: "Bob", Emails: bob.Emails}); c.PGPKeyRemovedAutocrypt != "" {
		t.Fatalf("a confirmed key was remembered: %+v", c)
	}
	if c, _ := store.Upsert(Contact{UID: bob.UID, FormattedName: "Bob", Emails: bob.Emails, PGPKey: other}); c.PGPKeySource != "" {
		t.Fatalf("a different pasted key took the Autocrypt source: %+v", c)
	}

	// A rotated Autocrypt key keeps its old pin; removing and re-pasting the
	// new key must not trust it either.
	rotated, err := store.Upsert(Contact{FormattedName: "Carol", Emails: []ContactValue{{Value: "carol@example.com"}}, PGPKey: other, PGPKeyFingerprint: fp, PGPKeySource: PGPSourceAutocrypt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(Contact{UID: rotated.UID, FormattedName: "Carol", Emails: rotated.Emails}); err != nil {
		t.Fatal(err)
	}
	if c, _ := store.Upsert(Contact{UID: rotated.UID, FormattedName: "Carol", Emails: rotated.Emails, PGPKey: other}); c.PGPKeySource != PGPSourceAutocrypt || c.PGPKeyVerified {
		t.Fatalf("a re-pasted rotated Autocrypt key was promoted: %+v", c)
	}

	// A deleted contact keeps no remembered fingerprint.
	if _, err := store.Upsert(Contact{UID: rotated.UID, FormattedName: "Carol", Emails: rotated.Emails}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Delete(rotated.UID); err != nil {
		t.Fatal(err)
	}
	if c, _, _ := store.Get(rotated.UID); c.PGPKeyRemovedAutocrypt != "" {
		t.Fatalf("a tombstone kept the removed key's fingerprint: %+v", c)
	}
}
