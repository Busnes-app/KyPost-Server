package imap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	goimap "github.com/BrianLeishman/go-imap"

	"github.com/Busnes-app/kypost-server/backend/internal/netguard"
)

// hostileIMAPClient returns a client for a server that accepts LOGIN and
// answers every other command with send's bytes, then hangs up.
func hostileIMAPClient(t *testing.T, send func(io.Writer)) *APIClient {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
				_, _ = fmt.Fprint(conn, "* OK hostile\r\n")
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					tag, command, _ := strings.Cut(line, " ")
					if strings.HasPrefix(strings.ToUpper(command), "LOGIN") {
						_, _ = fmt.Fprintf(conn, "%s OK LOGIN completed\r\n", tag)
						continue
					}
					send(conn)
					return
				}
			}()
		}
	}()

	previous := goimap.TLSSkipVerify
	goimap.TLSSkipVerify = true
	t.Cleanup(func() { goimap.TLSSkipVerify = previous })
	allowLoopbackDial(t)
	quietRetries(t, 0)

	c := &APIClient{host: "127.0.0.1", port: ln.Addr().(*net.TCPAddr).Port, username: "u", password: "p", mailbox: "INBOX"}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// stream writes n bytes of pattern, stopping when the client hangs up.
func stream(w io.Writer, pattern string, n int) {
	chunk := bytes.Repeat([]byte(pattern), 64<<10/len(pattern))
	for sent := 0; sent < n; sent += len(chunk) {
		if _, err := w.Write(chunk); err != nil {
			return
		}
	}
}

func setLimit[T int | int64](t *testing.T, p *T, v T) {
	previous := *p
	*p = v
	t.Cleanup(func() { *p = previous })
}

// A hostile account server must fail the command, not size an allocation.
func TestIMAPHostileResponsesFailBounded(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit func(t *testing.T)
		send  func(io.Writer)
	}{
		{"declared 2 GiB literal", func(*testing.T) {}, func(w io.Writer) {
			_, _ = io.WriteString(w, "* 1 FETCH (BODY[] {2147483648}\r\n")
		}},
		{"endless line", func(t *testing.T) { setLimit(t, &goimap.MaxLineSize, 1<<20) }, func(w io.Writer) {
			stream(w, "a", 32<<20)
		}},
		{"endless response", func(t *testing.T) { setLimit(t, &goimap.MaxResponseSize, 1<<20) }, func(w io.Writer) {
			// Long lines: go-imap's per-line regexp garbage would swamp the count.
			stream(w, "* OK "+strings.Repeat("x", 4<<10)+"\r\n", 32<<20)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.limit(t)
			c := hostileIMAPClient(t, tc.send)

			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, err := c.ListOverviews(context.Background(), "INBOX", 1)
			runtime.ReadMemStats(&after)

			if !errors.Is(err, goimap.ErrResponseTooLarge) {
				t.Errorf("err = %v, want ErrResponseTooLarge", err)
			}
			// Bounded churn (two attempts at a 1 MiB limit, more under -race),
			// against at least 64 MiB if the 32 MiB sent were accepted.
			if grew := after.TotalAlloc - before.TotalAlloc; grew > 32<<20 {
				t.Errorf("allocated %d MiB answering a hostile server", grew>>20)
			}
		})
	}
}

// The limits above are only lowered in tests; production installs these.
func TestIMAPLimitsInstalled(t *testing.T) {
	if goimap.MaxLiteralSize != maxIMAPLiteral || goimap.MaxLineSize != maxIMAPLine || goimap.MaxResponseSize != maxIMAPResponse {
		t.Fatalf("go-imap limits = %d/%d/%d, want %d/%d/%d", goimap.MaxLiteralSize, goimap.MaxLineSize, goimap.MaxResponseSize,
			maxIMAPLiteral, maxIMAPLine, maxIMAPResponse)
	}
}

// Account hosts are user-supplied: internal destinations are refused at dial.
func TestIMAPAccountRefusesInternalHosts(t *testing.T) {
	quietRetries(t, 0)
	for _, host := range []string{"127.0.0.1", "169.254.169.254", "localhost"} {
		c := &APIClient{host: host, port: 993, username: "u", password: "p", mailbox: "INBOX"}
		_, err := c.ListOverviews(context.Background(), "INBOX", 1)
		if !errors.Is(err, netguard.ErrInternalAddress) {
			t.Errorf("host %s: err = %v, want ErrInternalAddress", host, err)
		}
	}
}
