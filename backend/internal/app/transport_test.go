package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// hostCert writes a self-signed certificate and key, as an operator provisions per host.
func hostCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "replica-a"}, DNSNames: []string{"replica-a"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// Table mode refuses each hop that can leave the host in clear text, one at a time; a
// fully encrypted setup (database on loopback here) starts.
func TestTableTransportRefusesEachCleartextHop(t *testing.T) {
	certFile, keyFile := hostCert(t)
	good := map[string]string{
		"TLS_CERT_FILE":  certFile,
		"TLS_KEY_FILE":   keyFile,
		"PUSH_RELAY_URL": "https://push.example.test",
		"APNS_RELAY_URL": "https://apns.example.test",
	}
	issuer := sso.SSOSettings{IssuerURL: "https://id.example.test"}
	with := func(k, v string) func(string) string {
		return func(name string) string {
			if name == k {
				return v
			}
			return good[name]
		}
	}
	ctx := context.Background()
	for _, c := range []struct{ name, key, value string }{
		{"plain listener", "TLS_CERT_FILE", ""},
		{"listener key missing", "TLS_KEY_FILE", ""},
		{"certificate that does not load", "TLS_KEY_FILE", certFile},
		{"cleartext SMTP allowed", "ALLOW_INSECURE_SMTP", "true"},
		{"Ollama on another host over http", "OLLAMA_BASE_URL", "http://10.0.0.5:11434"},
		{"classifier on another host over http", "CLASSIFIER_BASE_URL", "http://ollama:11434"},
		{"push relay over http", "PUSH_RELAY_URL", "http://push.example.test"},
		{"APNs relay over http", "APNS_RELAY_URL", "http://10.0.0.6"},
	} {
		if err := checkTableTransport(ctx, with(c.key, c.value), issuer); !errors.Is(err, errCleartext) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	for name, s := range map[string]sso.SSOSettings{
		"http issuer":         {IssuerURL: "http://id.example.test"},
		"allowInsecureIssuer": {IssuerURL: "https://id.example.test", AllowInsecureIssuer: true},
	} {
		if err := checkTableTransport(ctx, with("", ""), s); !errors.Is(err, errCleartext) {
			t.Errorf("%s: %v", name, err)
		}
	}
	dbbackend.RequirePostgres(t)
	if err := checkTableTransport(ctx, with("", ""), issuer); err != nil {
		t.Fatal("an encrypted setup:", err)
	}
	if err := checkTableTransport(ctx, with("OLLAMA_BASE_URL", "http://localhost:11434"), sso.SSOSettings{IssuerURL: "http://127.0.0.1:8080"}); err != nil {
		t.Fatal("loopback http:", err)
	}
}
