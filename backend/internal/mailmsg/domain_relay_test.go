package mailmsg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
)

func TestDomainRelayEncryptedRotationAndMissingKeyRefusal(t *testing.T) {
	dir := t.TempDir()
	path, keyPath := filepath.Join(dir, "native-relay.json"), filepath.Join(dir, "native-relay.key")
	c := DomainRelay{Domain: "example.test", Issuer: "https://idp.example", Host: "smtp.example.test", Port: 465, Username: "operator-relay-login", Password: "operator-test-secret"}
	first, err := SaveDomainRelay(context.Background(), path, keyPath, c)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{path, keyPath} {
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private file mode: %v %v", info, err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(raw), c.Username) || strings.Contains(string(raw), c.Password) {
		t.Fatal("relay credentials appeared in stored plaintext", err)
	}
	got, exists, err := ReadDomainRelay(path, keyPath)
	if err != nil || !exists || got != first {
		t.Fatal("encrypted round trip", got, exists, err)
	}
	second, err := SaveDomainRelay(context.Background(), path, keyPath, c)
	if err != nil || second.Generation == first.Generation {
		t.Fatal("rotation reused configuration generation", err)
	}
	c.Issuer = "https://other.example"
	if _, err := SaveDomainRelay(context.Background(), path, keyPath, c); !errors.Is(err, ErrDomainRelay) {
		t.Fatal("relay silently adopted another issuer", err)
	}
	c.Issuer = first.Issuer
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadDomainRelay(path, keyPath); !errors.Is(err, ErrDomainRelay) {
		t.Fatal("missing decryption key was not refused", err)
	}
	if _, err := SaveDomainRelay(context.Background(), path, keyPath, c); !errors.Is(err, ErrDomainRelay) {
		t.Fatal("rotation replaced a lost key", err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatal("lost key was regenerated", err)
	}
}

func TestDomainRelayRejectsInvalidTransportAndStoredPlaintext(t *testing.T) {
	dir := t.TempDir()
	path, keyPath := filepath.Join(dir, "relay"), filepath.Join(dir, "key")
	c := DomainRelay{Domain: "example.test", Issuer: "https://idp.example", Host: "smtp.example.test", Port: 465, Username: "login", Password: "secret"}
	for _, mutate := range []func(*DomainRelay){
		func(c *DomainRelay) { c.Host = "smtp.example.test:465" },
		func(c *DomainRelay) { c.Host = "https://smtp.example.test" },
		func(c *DomainRelay) { c.Username = "injected\x00user" },
		func(c *DomainRelay) { c.Password = "" },
		func(c *DomainRelay) { c.Port = -1 },
		func(c *DomainRelay) { c.Port = 65536 },
	} {
		bad := c
		mutate(&bad)
		if _, err := SaveDomainRelay(context.Background(), path, keyPath, bad); !errors.Is(err, ErrDomainRelay) {
			t.Fatal("invalid relay accepted", err)
		}
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatal("invalid input created a key", err)
	}
	key, err := cryptutil.LoadOrCreateKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeDomainRelay([]byte(`{"version":1,"smtpPassword":"plaintext"}`), key); !errors.Is(err, ErrDomainRelay) {
		t.Fatal("plaintext relay accepted", err)
	}
}
