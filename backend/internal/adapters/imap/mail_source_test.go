package imap

import "testing"

// Embedded Client is never called: these are pure wire identity boundaries.
type referenceTestClient struct {
	Client
	generation string
}

func (c referenceTestClient) MailSourceIdentity() string         { return "native:synthetic" }
func (c referenceTestClient) MessageReferenceGeneration() string { return c.generation }

type unknownReferenceTestClient struct{ Client }

func (c unknownReferenceTestClient) MailSourceIdentity() string { return "native:unknown" }

func TestMessageReferencesRequireNativeGeneration(t *testing.T) {
	c := referenceTestClient{generation: "11111111-1111-4111-8111-111111111111"}
	valid := "n1:" + c.generation + ":42"
	if MessageReference(c, "42") != valid {
		t.Fatal("wire encoding")
	}
	if id, err := ResolveMessageReference(c, valid); err != nil || id != "42" {
		t.Fatal(id, err)
	}
	for _, reference := range []string{"42", "n1:foreign:42", "n1:" + c.generation + ":0", "n1:" + c.generation + ":+42", "n1:" + c.generation + ":042", valid + ":1", valid + " ", "n1:" + c.generation + ":99999999999999999999"} {
		if _, err := ResolveMessageReference(c, reference); err == nil {
			t.Fatal("invalid native reference accepted", reference)
		}
	}
	for _, client := range []Client{unknownReferenceTestClient{}, referenceTestClient{}} {
		if MessageReference(client, "42") != "" {
			t.Fatal("unknown native capability fell back")
		}
		if _, err := ResolveMessageReference(client, "42"); err == nil {
			t.Fatal("unknown native capability accepted")
		}
	}
	if MessageReference(nil, "42") != "42" {
		t.Fatal("legacy formatter changed")
	}
	if id, err := ResolveMessageReference(nil, "42"); err != nil || id != "42" {
		t.Fatal("legacy resolver changed", id, err)
	}
}
