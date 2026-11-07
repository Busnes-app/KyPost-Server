package mailmsg

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

func TestDomainRelayDirectProtectedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path, keyPath := filepath.Join(dir, "relay"), filepath.Join(dir, "key")
	profile, err := SaveDomainRelay(context.Background(), path, keyPath, directTestProfile())
	if err != nil {
		t.Fatal(err)
	}
	saved, exists, err := ReadDomainRelay(path, keyPath)
	if err != nil || !exists || !saved.Equal(profile) {
		t.Fatal("direct profile lost", err)
	}
	domains, err := SetDomainRelayDomains(context.Background(), path, keyPath, []string{"example.test", "second.test"})
	if err != nil || domains.Mode != "direct" || domains.Generation != saved.Generation {
		t.Fatal("domain edit changed transport", err)
	}
	if err = saved.Check(context.Background()); err == nil {
		t.Fatal("direct profile reported SMTP AUTH ready")
	}
	for _, mutate := range []func(*DomainRelay){func(c *DomainRelay) { c.Password = "credential" }, func(c *DomainRelay) { c.Port = 465 }, func(c *DomainRelay) { c.Mode = "unknown" }, func(c *DomainRelay) { c.Host = "127.0.0.1" }} {
		bad := saved
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatal("mixed direct/relay profile accepted")
		}
	}
	key := bytes.Repeat([]byte{1}, 32)
	sealed, err := SealDomainRelay(saved, key)
	if err != nil {
		t.Fatal(err)
	}
	restored, version, err := DecodeDomainRelay(sealed, key)
	if err != nil || version != 2 || !restored.Equal(saved) {
		t.Fatal("backup profile lost direct mode", err)
	}
}
