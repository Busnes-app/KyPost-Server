package imap

import (
	"bufio"
	"context"
	"errors"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"
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
		_, err := OpenImportSource(context.Background(), client, "imap.example.com", true, nil, "user", []byte("secret"), 1<<20)
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

// A response over the line budget ends promptly with ErrImportProtocol wherever
// the budget runs out, without spinning on a byte it may not consume.
func TestImportSourceLineBudget(t *testing.T) {
	long := strings.Repeat("a", importLineMax+10)
	for name, input := range map[string]string{
		"atom":        "* " + long + "\r\n",
		"whitespace":  "* LIST" + strings.Repeat(" ", importLineMax+10) + "x\r\n",
		"quoted":      `* LIST "` + long + "\"\r\n",
		"nested list": "* LIST (a (" + strings.Repeat("b ", importLineMax/2+10) + "))\r\n",
		"tag":         long,
	} {
		s := &ImportSource{r: bufio.NewReaderSize(strings.NewReader(input), 4096), left: 1 << 30}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		done := make(chan error, 1)
		go func() { _, _, err := s.response(importMetaLit); done <- err }()
		select {
		case err := <-done:
			runtime.ReadMemStats(&after)
			if !errors.Is(err, ErrImportProtocol) {
				t.Errorf("%s: %v", name, err)
			}
			if grown := after.TotalAlloc - before.TotalAlloc; grown > 8<<20 {
				t.Errorf("%s: allocated %d bytes", name, grown)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: the parser did not stop at the line budget", name)
		}
	}
}
