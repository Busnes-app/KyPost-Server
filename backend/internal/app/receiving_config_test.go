//go:build linux

package app

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNativeReceivingConfigRefusesUnsafeInputs(t *testing.T) {
	r, _ := receivingFixture(t)
	t.Setenv("CONFIG_DIR", r.configDir)
	t.Setenv("STATE_DIR", r.stateDir)
	t.Setenv("KYPOST_NATIVE_MAIL", "true")
	t.Setenv("KYPOST_NATIVE_RECEIVING", "true")
	root := t.TempDir()
	cert, key, leaf := receivingTestCertificate(t, root)
	args := []string{"127.0.0.1:2525", leaf.DNSNames[0], cert, key}
	badCert := filepath.Join(root, "bad.pem")
	if err := os.WriteFile(badCert, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "cert-link")
	if err := os.Symlink(cert, link); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		index int
		value string
	}{
		{"no port", 0, "127.0.0.1"}, {"zero port", 0, "127.0.0.1:0"},
		{"hostname bind", 0, "localhost:25"}, {"zone", 0, "[fe80::1%eth0]:25"},
		{"hostname injection", 1, "mail.example.test\ninclude attacker"},
		{"hostname wildcard", 1, "*.example.test"}, {"wrong certificate name", 1, "wrong.example.test"},
		{"relative path", 2, "cert.pem"}, {"quote", 2, "/tmp/a\"b"},
		{"expansion", 2, "/tmp/{env:SECRET}"}, {"dollar", 2, "/tmp/$SECRET"},
		{"backslash", 2, "/tmp/a\\b"}, {"control", 2, "/tmp/a\x00b"},
		{"malformed certificate", 2, badCert}, {"FIFO", 2, fifo}, {"symlink", 2, link},
		{"missing key", 3, filepath.Join(root, "missing")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := append([]string(nil), args...)
			input[tc.index] = tc.value
			var output bytes.Buffer
			if err := runReceivingConfig(input, &output); err == nil || output.Len() != 0 {
				t.Fatal("unsafe input produced configuration", err, output.String())
			}
		})
	}
	if err := os.Chmod(key, 0644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runReceivingConfig(args, &output); err == nil || output.Len() != 0 {
		t.Fatal("publicly readable private key accepted", err)
	}
	if err := os.Chmod(key, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.stateDir, "native-restore-hold.json"), []byte("held"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runReceivingConfig(args, &output); err == nil || output.Len() != 0 {
		t.Fatal("restore hold produced configuration", err)
	}
	if err := os.Remove(filepath.Join(r.stateDir, "native-restore-hold.json")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KYPOST_NATIVE_RECEIVING", "false")
	if err := runReceivingConfig(args, &output); err == nil || output.Len() != 0 {
		t.Fatal("disabled reception produced configuration", err)
	}
	t.Setenv("KYPOST_NATIVE_RECEIVING", "true")
	if err := r.holding.Close(); err != nil {
		t.Fatal(err)
	}
	spool := filepath.Join(r.stateDir, "receiving")
	if err := os.Rename(spool, spool+"-retained"); err != nil {
		t.Fatal(err)
	}
	if err := runReceivingConfig(args, &output); err == nil || output.Len() != 0 {
		t.Fatal("missing spool produced configuration", err)
	}
	if _, err := os.Lstat(spool); !os.IsNotExist(err) {
		t.Fatal("configuration recreated missing spool", err)
	}
}

func TestNativeReceivingConfigRejectsCertificatePurpose(t *testing.T) {
	r, _ := receivingFixture(t)
	t.Setenv("CONFIG_DIR", r.configDir)
	t.Setenv("STATE_DIR", r.stateDir)
	t.Setenv("KYPOST_NATIVE_MAIL", "true")
	t.Setenv("KYPOST_NATIVE_RECEIVING", "true")
	cert, key, leaf := receivingTestCertificate(t, t.TempDir())
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, known := range []bool{false, true} {
		template := *leaf
		template.ExtKeyUsage = nil
		template.UnknownExtKeyUsage = []asn1.ObjectIdentifier{{1, 2, 3, 4}}
		if known {
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
			template.UnknownExtKeyUsage = nil
		}
		der, err := x509.CreateCertificate(rand.Reader, &template, &template, pair.PrivateKey.(crypto.Signer).Public(), pair.PrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		err = runReceivingConfig([]string{"127.0.0.1:2525", leaf.DNSNames[0], cert, key}, &output)
		if err == nil || !strings.Contains(err.Error(), "server authentication") || output.Len() != 0 {
			t.Fatalf("known=%v certificate purpose reached DNS or output: %v %s", known, err, output.String())
		}
	}
}
