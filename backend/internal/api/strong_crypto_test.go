package api

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/fips140"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func rsaKey(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// issue signs a certificate for pub with parentKey; a nil parent self-signs.
func issue(t *testing.T, pub crypto.PublicKey, parentKey crypto.Signer, parent *x509.Certificate, ca bool) *x509.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "kypost.test"},
		DNSNames:              []string{"kypost.test"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  ca,
		BasicConstraintsValid: true,
	}
	if parent == nil {
		parent = tmpl
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func writeRSAKeypair(t *testing.T, certPath, keyPath string, key *rsa.PrivateKey) {
	t.Helper()
	cert := issue(t, &key.PublicKey, key, nil, true)
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Go serves an RSA-1024 certificate even with GODEBUG=fips140=on, so the
// inbound listener refuses one itself, at startup and on reload.
func TestInboundTLSRefusesRSACertificateBelow2048(t *testing.T) {
	strong := rsaKey(t, 2048)
	for _, bits := range []int{1024, 2047} {
		dir := t.TempDir()
		certPath, keyPath := filepath.Join(dir, "c.pem"), filepath.Join(dir, "c.key")
		writeRSAKeypair(t, certPath, keyPath, rsaKey(t, bits))
		if _, err := newTLSConfig(certPath, keyPath); err == nil || !strings.Contains(err.Error(), "at least 2048 bits") {
			t.Fatalf("RSA-%d certificate: err = %v, want refusal", bits, err)
		}

		writeRSAKeypair(t, certPath, keyPath, strong)
		rc, err := newReloadingCertificate(certPath, keyPath)
		if err != nil {
			t.Fatalf("RSA-2048 certificate refused: %v", err)
		}
		time.Sleep(1100 * time.Millisecond) // the reload check compares modtimes
		writeRSAKeypair(t, certPath, keyPath, rsaKey(t, bits))
		got, err := rc.get(nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.PrivateKey.(*rsa.PrivateKey).N.BitLen() != 2048 {
			t.Fatalf("reload switched to an RSA-%d certificate", bits)
		}
	}
}

// The GODEBUG=fips140=on profile reaches KyPost's TLS clients: a chain under a
// weak RSA root is refused there and accepted otherwise. The pin probe stands
// in for every client that uses crypto/tls with verification.
func TestTLSClientsRefuseWeakChainsInTheStrongProfile(t *testing.T) {
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, bits := range []int{1024, 2047, 2048} {
		rootKey := rsaKey(t, bits)
		root := issue(t, &rootKey.PublicKey, rootKey, nil, true)
		leaf := issue(t, &leafKey.PublicKey, rootKey, root, false)
		srv := httptest.NewUnstartedServer(http.NotFoundHandler())
		srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: leafKey}}}
		srv.StartTLS()
		roots := x509.NewCertPool()
		roots.AddCert(root)
		_, err := probeSPKIPin(context.Background(), srv.URL, roots)
		srv.Close()
		if refused, want := err != nil, fips140.Enabled() && bits < 2048; refused != want {
			t.Fatalf("RSA-%d root, fips140=%v: err = %v", bits, fips140.Enabled(), err)
		}
	}
}
