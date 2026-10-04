package mailmsg

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const relayProofMIME = "From: mailbox@example.test\r\nTo: visible@example.test\r\nSubject: relay proof\r\n\r\n.test MIME\r\n"

func TestDomainRelayStrictTLSRuntime(t *testing.T) {
	if addr := os.Getenv("KYPOST_RELAY_TLS_PROOF_ADDR"); addr != "" {
		host, portText, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatal(err)
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			t.Fatal(err)
		}
		c := DomainRelay{Version: 1, Generation: "12345678-1234-4234-8234-123456789abc", Domain: "example.test", Issuer: "https://idp.example", Host: host, Port: port, Username: "operator-relay-login", Password: "operator-relay-secret"}
		err = c.Deliver("mailbox@example.test", []string{"visible@example.test", "hidden@example.test"}, []byte(relayProofMIME))
		switch os.Getenv("KYPOST_RELAY_TLS_PROOF_CASE") {
		case "accepted":
			if err != nil {
				t.Fatal(err)
			}
		case "lost-ack":
			if !errors.Is(err, ErrSMTPAcceptanceUncertain) {
				t.Fatal("expected uncertainty", err)
			}
		case "no-auth":
			if err == nil || !strings.Contains(err.Error(), "AUTH") {
				t.Fatal("missing AUTH did not refuse", err)
			}
		case "auth-echo":
			var reply *textproto.Error
			if err == nil || strings.Contains(err.Error(), "operator-relay") || !errors.As(err, &reply) || reply.Code != 535 {
				t.Fatal("AUTH response leaked credentials or lost classification", err)
			}
		default:
			if err == nil {
				t.Fatal("unverified TLS/plaintext was accepted")
			}
		}
		return
	}
	// The child processes trust only this test certificate. Runtime TLS uses
	// its ordinary system verifier; no production InsecureSkipVerify/test hook.
	certServer := httptest.NewTLSServer(http.NotFoundHandler())
	cert := certServer.TLS.Certificates[0]
	certServer.Close()
	root := t.TempDir()
	caPath := filepath.Join(root, "test-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"accepted", "lost-ack", "no-auth", "auth-echo", "untrusted", "plaintext"} {
		t.Run(mode, func(t *testing.T) {
			var ln net.Listener
			var err error
			if mode == "plaintext" {
				ln, err = net.Listen("tcp", "127.0.0.1:0")
			} else {
				ln, err = tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			type observed struct {
				auth, from, raw string
				recipients      []string
			}
			done := make(chan observed, 1)
			go func() {
				var got observed
				defer func() { done <- got }()
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
				write := func(v string) { _, _ = conn.Write([]byte(v + "\r\n")) }
				write("220 relay proof")
				r := bufio.NewReader(conn)
				inData := false
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if inData {
						if line == ".\r\n" {
							if mode == "lost-ack" {
								return
							}
							inData = false
							write("250 accepted")
							continue
						}
						if strings.HasPrefix(line, "..") {
							line = line[1:]
						}
						got.raw += line
						continue
					}
					switch {
					case strings.HasPrefix(line, "EHLO"):
						if mode == "no-auth" {
							write("250 relay")
						} else {
							write("250-relay\r\n250 AUTH PLAIN")
						}
					case strings.HasPrefix(line, "AUTH PLAIN "):
						b, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(line, "AUTH PLAIN ")))
						got.auth = string(b)
						if mode == "auth-echo" {
							write("535 echoed operator-relay-login operator-relay-secret")
						} else {
							write("235 authenticated")
						}
					case strings.HasPrefix(line, "MAIL FROM:"):
						got.from = line
						write("250 sender")
					case strings.HasPrefix(line, "RCPT TO:"):
						got.recipients = append(got.recipients, line)
						write("250 recipient")
					case line == "DATA\r\n":
						inData = true
						write("354 data")
					case line == "QUIT\r\n":
						write("221 bye")
						return
					default:
						return
					}
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDomainRelayStrictTLSRuntime$", "-test.count=1")
			trust := caPath
			if mode == "untrusted" {
				trust = filepath.Join(root, "missing-ca.pem")
			}
			cmd.Env = append(os.Environ(), "KYPOST_RELAY_TLS_PROOF_ADDR="+ln.Addr().String(), "KYPOST_RELAY_TLS_PROOF_CASE="+mode, "SSL_CERT_FILE="+trust, "SSL_CERT_DIR="+t.TempDir(), "ALLOW_INSECURE_SMTP=true")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("strict relay child: %v\n%s", err, out)
			}
			select {
			case got := <-done:
				if mode == "accepted" || mode == "lost-ack" {
					if got.auth != "\x00operator-relay-login\x00operator-relay-secret" || !strings.Contains(got.from, "mailbox@example.test") || len(got.recipients) != 2 || !strings.Contains(got.recipients[1], "hidden@example.test") || got.raw != relayProofMIME {
						t.Fatalf("relay changed authentication/envelope/MIME: %+v", got)
					}
				} else if mode == "auth-echo" {
					if got.auth != "\x00operator-relay-login\x00operator-relay-secret" || got.from != "" || got.raw != "" {
						t.Fatal("rejected AUTH reached mail submission")
					}
				} else if got.auth != "" || got.from != "" || got.raw != "" {
					t.Fatal("unsafe relay received credentials or mail")
				}
			case <-ctx.Done():
				t.Fatal("relay proof did not terminate")
			}
		})
	}
}
