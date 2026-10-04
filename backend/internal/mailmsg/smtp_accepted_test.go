package mailmsg

// Telling "the server never took the message" apart from "the server took the
// message and then the connection broke".
//
// Both come back from a send as a non-nil error. Blind retries can duplicate
// accepted mail, and the pickup path cannot treat
// them the same: it deletes its stored record when a send fails, to stop
// undeliverable records from holding a quota slot for seven days. Delete on the
// second case and the recipient gets the link email for a message the server
// has already thrown away, which is strictly worse than the leak being fixed.
//
// The window is real and narrow: net/smtp's writer.Close() returning nil means
// the server answered 250 to the DATA, and client.Quit() runs after that. A
// connection dropped between those two points is an accepted message reported
// as a failure.

import (
	"bufio"
	"errors"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

// smtpScript runs a minimal SMTP server that accepts one message. If
// dropAfterData is set it closes the connection immediately after answering
// 250 to the message body, without ever responding to QUIT.
func smtpScript(t *testing.T, dropAfterData bool, finalReply string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		r := bufio.NewReader(conn)
		w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }

		w("220 test ready")
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")

			if inData {
				if line == "." {
					inData = false
					if finalReply == "" {
						return // Body received, final acceptance answer lost.
					}
					w(finalReply)
					if dropAfterData {
						// Accepted, then vanish before QUIT is answered.
						return
					}
					continue
				}
				continue
			}

			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				// No extensions: no STARTTLS to negotiate, no AUTH to run.
				w("250 test")
			case strings.HasPrefix(line, "MAIL"), strings.HasPrefix(line, "RCPT"):
				w("250 ok")
			case strings.HasPrefix(line, "DATA"):
				inData = true
				w("354 go ahead")
			case strings.HasPrefix(line, "QUIT"):
				w("221 bye")
				return
			default:
				w("250 ok")
			}
		}
	}()
	return ln.Addr().String()
}

func sendTo(addr string) error {
	return SMTPSendWithTimeout(addr, nil, "a@example.com", []string{"b@example.com"},
		[]byte("Subject: hi\r\n\r\nbody"), 5*time.Second)
}

// The ordinary case still has to be a clean success, or the sentinel below
// would be indistinguishable from "this never works".
func TestSMTPSendSucceedsAgainstAWellBehavedServer(t *testing.T) {
	t.Setenv("ALLOW_INSECURE_SMTP", "1")
	AllowInsecureSMTP = true
	t.Cleanup(func() { AllowInsecureSMTP = false })

	if err := sendTo(smtpScript(t, false, "250 accepted")); err != nil {
		t.Fatalf("send: %v", err)
	}
}

// The message was accepted; only the goodbye failed. Callers that undo work on
// failure need to be able to see that.
func TestSMTPSendReportsAcceptanceWhenOnlyQuitFails(t *testing.T) {
	t.Setenv("ALLOW_INSECURE_SMTP", "1")
	AllowInsecureSMTP = true
	t.Cleanup(func() { AllowInsecureSMTP = false })

	err := sendTo(smtpScript(t, true, "250 accepted"))
	if err == nil {
		t.Fatal("server never answered QUIT; expected an accepted-then-teardown error")
	}
	if !errors.Is(err, ErrSMTPAcceptedThenFailed) {
		t.Fatalf("err = %v, want it to wrap ErrSMTPAcceptedThenFailed; a caller "+
			"would now delete a message the recipient was told about", err)
	}
}

func TestSMTPFinalResponseDistinguishesRefusalFromUncertainty(t *testing.T) {
	previous := AllowInsecureSMTP
	AllowInsecureSMTP = true
	t.Cleanup(func() { AllowInsecureSMTP = previous })
	for _, tc := range []struct {
		reply     string
		uncertain bool
	}{
		{"", true},
		{"relay echoed recipient@example.com pickup-bearer-secret", true},
		{"251 unexpected positive response", true},
		{"451 temporarily refused", false},
		{"550 permanently refused", false},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			err := sendTo(smtpScript(t, false, tc.reply))
			if err == nil || errors.Is(err, ErrSMTPAcceptedThenFailed) {
				t.Fatalf("lost/refused DATA must not claim acceptance: %v", err)
			}
			if errors.Is(err, ErrSMTPAcceptanceUncertain) != tc.uncertain {
				t.Fatalf("final response %q classified incorrectly: %v", tc.reply, err)
			}
			if tc.uncertain && err.Error() != ErrSMTPAcceptanceUncertain.Error() {
				t.Fatal("uncertain result disclosed an untrusted SMTP response")
			}
			if strings.HasPrefix(tc.reply, "relay echoed") {
				var protocolError textproto.ProtocolError
				if !errors.As(err, &protocolError) {
					t.Fatal("uncertainty classification lost its underlying protocol error")
				}
			}
		})
	}
}

// A failure before DATA is accepted must NOT claim acceptance, or the quota
// leak this distinction exists to fix comes straight back.
func TestSMTPSendDoesNotClaimAcceptanceWhenRefusedEarly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("554 no service here\r\n"))
	}()

	err = sendTo(ln.Addr().String())
	if err == nil {
		t.Fatal("expected a rejection")
	}
	if errors.Is(err, ErrSMTPAcceptedThenFailed) {
		t.Fatalf("a pre-acceptance rejection reported acceptance: %v", err)
	}
}
