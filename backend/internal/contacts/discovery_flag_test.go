package contacts

import "testing"

// A CardDAV PUT or mobile-sync write-back never sends DiscoveryCreated. If
// the store dropped it, a contact harvested from a stranger's Autocrypt header
// would become a known sender whose mail skips classification.
func TestUpsertKeepsDiscoveryCreatedWhenWriterOmitsIt(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, err := store.Upsert(Contact{FormattedName: "x@evil.example", Emails: []ContactValue{{Value: "x@evil.example"}}, DiscoveryCreated: true})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	c.DiscoveryCreated = false
	c.FormattedName = "Renamed by phone"
	after, err := store.Upsert(c)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if !after.DiscoveryCreated {
		t.Fatal("DiscoveryCreated dropped by an update that omitted it")
	}
}
