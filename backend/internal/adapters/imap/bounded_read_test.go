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
// answers every other command through send, hanging up when it returns false.
func hostileIMAPClient(t *testing.T, send func(w io.Writer, tag, command string) bool) *APIClient {
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
					if !send(conn, tag, strings.ToUpper(command)) {
						return
					}
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

// once adapts a one-shot hostile answer: send it, then hang up.
func once(send func(io.Writer)) func(io.Writer, string, string) bool {
	return func(w io.Writer, _, _ string) bool { send(w); return false }
}

// A hostile account server must fail the command, not size an allocation.
func TestIMAPHostileResponsesFailBounded(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit func(t *testing.T)
		send  func(io.Writer, string, string) bool
	}{
		{"declared 2 GiB literal", func(*testing.T) {}, once(func(w io.Writer) {
			_, _ = io.WriteString(w, "* 1 FETCH (BODY[] {2147483648}\r\n")
		})},
		{"endless line", func(t *testing.T) { setLimit(t, &goimap.MaxLineSize, 1<<20) }, once(func(w io.Writer) {
			stream(w, "a", 32<<20)
		})},
		{"endless response", func(t *testing.T) { setLimit(t, &goimap.MaxResponseSize, 1<<20) }, once(func(w io.Writer) {
			// Long lines: go-imap's per-line regexp garbage would swamp the count.
			stream(w, "* OK "+strings.Repeat("x", 4<<10)+"\r\n", 32<<20)
		})},
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

// A response under the limit that then fails to parse must not be copied into
// the error (and its log) whole.
func TestIMAPMalformedFetchErrorIsBounded(t *testing.T) {
	const limit = 4 << 20
	setLimit(t, &goimap.MaxResponseSize, limit)
	// A malformed literal size, just under the limit; built before measuring.
	fetch := []byte("* 1 FETCH (UID 1 BODY[] {5X" + strings.Repeat("a", limit-64<<10) + ")\r\n")
	c := hostileIMAPClient(t, func(w io.Writer, tag, command string) bool {
		switch {
		case strings.HasPrefix(command, "SELECT"):
			_, _ = fmt.Fprintf(w, "* 1 EXISTS\r\n%s OK SELECT completed\r\n", tag)
		case strings.HasPrefix(command, "UID SEARCH"):
			_, _ = fmt.Fprintf(w, "* SEARCH 1\r\n%s OK SEARCH completed\r\n", tag)
		default:
			_, _ = w.Write(fetch)
			_, _ = fmt.Fprintf(w, "%s OK FETCH completed\r\n", tag)
		}
		return true
	})

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := c.ListOverviews(context.Background(), "INBOX", 1)
	runtime.ReadMemStats(&after)

	if err == nil || len(err.Error()) > 4<<10 {
		t.Fatalf("err = %d bytes, want a parse error under 4 KiB", len(fmt.Sprint(err)))
	}
	// Reading costs ~3x (line assembly 2x, the response 1x); each copy of the
	// response in an error or log line would add at least 1x more.
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 3*limit+limit/2 {
		t.Errorf("allocated %d MiB for a %d MiB response", grew>>20, limit>>20)
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
