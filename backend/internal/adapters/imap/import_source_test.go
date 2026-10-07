package imap

import (
	"bufio"
	"context"
	"errors"
	"net"
	"testing"
)

// A STARTTLS answer must not smuggle plaintext past the upgrade, and PREAUTH
// must not skip TLS: both refused before any credential is sent.
func TestImportSourceRefusesPlaintextTricks(t *testing.T) {
	for name, script := range map[string][]string{
		"injection after STARTTLS": {"* OK ready\r\n", "k1 OK begin\r\n* 1 EXISTS\r\n"},
		"PREAUTH":                  {"* PREAUTH logged in\r\n"},
	} {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			r := bufio.NewReader(server)
			for i, out := range script {
				if i > 0 {
					if line, err := r.ReadString('\n'); err != nil || line != "k1 STARTTLS\r\n" {
						return
					}
				}
				if _, err := server.Write([]byte(out)); err != nil {
					return
				}
			}
			_, _ = r.ReadString('\n') // anything further would be a credential
		}()
		_, err := OpenImportSource(context.Background(), client, "imap.example.com", true, nil, "user", []byte("secret"))
		if !errors.Is(err, ErrImportProtocol) && !errors.Is(err, ErrImportRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestDecodeMUTF7(t *testing.T) {
	for in, want := range map[string]string{"INBOX": "INBOX", "Caf&AOk-": "Café", "&-x": "&x", "&ZeVnLIqe-": "日本語"} {
		if got, ok := decodeMUTF7(in); !ok || got != want {
			t.Errorf("decodeMUTF7(%q) = %q %v", in, got, ok)
		}
	}
	for _, bad := range []string{"&AOk", "&!!-", "caf\xe9"} {
		if got, ok := decodeMUTF7(bad); ok {
			t.Errorf("decodeMUTF7(%q) accepted as %q", bad, got)
		}
	}
}
