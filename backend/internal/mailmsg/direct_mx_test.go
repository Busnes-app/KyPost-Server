package mailmsg

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

func directTestProfile() DomainRelay {
	return DomainRelay{Mode: "direct", Generation: "12345678-1234-4234-8234-123456789abc", Domains: []string{"example.test"}, Issuer: "https://identity.example.test", Host: "mail.example.test", DKIM: []DKIMKey{testDKIMKey("example.test")}}
}
func preserveDirectSeams(t *testing.T) {
	t.Helper()
	mx, ip, dial, roots := directLookupMX, directLookupIP, directDial, directRoots
	t.Cleanup(func() { directLookupMX, directLookupIP, directDial, directRoots = mx, ip, dial, roots })
}

func TestDirectMXSMTPRecipientEvidence(t *testing.T) {
	certificate := httptest.NewTLSServer(http.NotFoundHandler())
	pair := certificate.TLS.Certificates[0]
	certificate.Close()
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"accepted", "lost-ack", "451", "550", "accepted-quit-lost", "plaintext", "untrusted", "oversized-banner"} {
		t.Run(mode, func(t *testing.T) {
			preserveDirectSeams(t)
			directRoots = x509.NewCertPool()
			if mode != "untrusted" {
				directRoots.AddCert(leaf)
			}
			directLookupMX = func(context.Context, string) ([]*net.MX, error) {
				return []*net.MX{{Host: "unavailable.test.", Pref: 5}, {Host: "example.com.", Pref: 10}}, nil
			}
			directLookupIP = func(_ context.Context, _ string, host string) ([]net.IP, error) {
				if host == "unavailable.test." {
					return []net.IP{net.ParseIP("8.8.4.4")}, nil
				}
				return []net.IP{net.ParseIP("8.8.8.8")}, nil
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			attempts := 0
			directDial = func(ctx context.Context, network, address string) (net.Conn, error) {
				attempts++
				switch address {
				case "8.8.4.4:25":
					return nil, errors.New("controlled connection refusal")
				case "8.8.8.8:25":
					return (&net.Dialer{}).DialContext(ctx, network, ln.Addr().String())
				default:
					t.Errorf("unchecked destination %s", address)
					return nil, errors.New("unexpected target")
				}
			}
			type observed struct {
				raw, hello string
				recipients int
				auth       bool
				err        error
			}
			done := make(chan observed, 1)
			go func() {
				var got observed
				defer func() { done <- got }()
				c, e := ln.Accept()
				if e != nil {
					got.err = e
					return
				}
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				reader, writer := bufio.NewReader(c), bufio.NewWriter(c)
				reply := func(s string) { _, _ = writer.WriteString(s + "\r\n"); _ = writer.Flush() }
				if mode == "oversized-banner" {
					reply("220 " + strings.Repeat("X", 2<<20))
					return
				}
				reply("220 controlled recipient")
				secured := false
				for {
					line, e := reader.ReadString('\n')
					if e != nil {
						return
					}
					switch {
					case strings.HasPrefix(line, "EHLO "):
						got.hello = strings.TrimSpace(strings.TrimPrefix(line, "EHLO "))
						if !secured && mode != "plaintext" {
							reply("250-recipient\r\n250 STARTTLS")
						} else {
							reply("250 recipient")
						}
					case line == "STARTTLS\r\n":
						reply("220 TLS")
						connection := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
						if connection.Handshake() != nil {
							return
						}
						c = connection
						reader, writer = bufio.NewReader(c), bufio.NewWriter(c)
						secured = true
					case strings.HasPrefix(line, "AUTH "):
						got.auth = true
						reply("535 no AUTH")
					case strings.HasPrefix(line, "MAIL FROM:"):
						reply("250 sender")
					case strings.HasPrefix(line, "RCPT TO:"):
						got.recipients++
						reply("250 recipient")
					case line == "DATA\r\n":
						reply("354 data")
						raw, e := textproto.NewReader(reader).ReadDotBytes()
						if e != nil {
							got.err = e
							return
						}
						got.raw = string(raw)
						switch mode {
						case "lost-ack":
							return
						case "451":
							reply("451 controlled refusal")
						case "550":
							reply("550 controlled refusal")
						case "accepted-quit-lost":
							reply("250 accepted")
							return
						default:
							reply("250 accepted")
						}
					case line == "QUIT\r\n":
						reply("221 bye")
						return
					default:
						got.err = errors.New("unexpected SMTP command")
						return
					}
				}
			}()
			err = directTestProfile().Deliver("mailbox@example.test", []string{"visible@Example.Test"}, []byte(relayProofMIME))
			var code *textproto.Error
			switch mode {
			case "accepted":
				if err != nil {
					t.Fatal(err)
				}
			case "lost-ack":
				if !errors.Is(err, ErrSMTPAcceptanceUncertain) {
					t.Fatal("uncertainty lost", err)
				}
			case "accepted-quit-lost":
				if !errors.Is(err, ErrSMTPAcceptedThenFailed) {
					t.Fatal("acceptance lost", err)
				}
			case "451", "550":
				if !errors.As(err, &code) || (mode == "451" && code.Code != 451) || (mode == "550" && code.Code != 550) {
					t.Fatal("definite refusal lost", err)
				}
			default:
				if err == nil {
					t.Fatal("unsafe TLS accepted")
				}
			}
			got := <-done
			if got.err != nil || got.auth || (mode != "oversized-banner" && got.hello != "mail.example.test") {
				t.Fatal("invalid recipient conversation", got)
			}
			if mode == "plaintext" || mode == "untrusted" || mode == "oversized-banner" {
				if got.raw != "" || got.recipients != 0 {
					t.Fatal("unsafe peer received mail")
				}
			} else {
				// One verifying DKIM-Signature, then the exact stored MIME.
				normalized, e := NormalizeSMTPMessage([]byte(got.raw))
				if e != nil || verifyWire(t, directTestProfile(), string(normalized)) != relayProofMIME || got.recipients != 1 {
					t.Fatal("recipient MIME/envelope mismatch", got)
				}
			}
			if attempts != 2 {
				t.Fatal("retried another endpoint after DATA or skipped MX fallback", attempts)
			}
		})
	}
}

func TestDirectMXDNSAndDestinationRefusals(t *testing.T) {
	for _, mode := range []string{"null", "mixed-null", "temporary", "private", "mixed-addresses", "implicit", "too-many", "invalid-host"} {
		t.Run(mode, func(t *testing.T) {
			preserveDirectSeams(t)
			directLookupMX = func(_ context.Context, name string) ([]*net.MX, error) {
				if name != "example.test." {
					t.Fatal("noncanonical domain", name)
				}
				switch mode {
				case "null":
					return []*net.MX{{Host: "."}}, nil
				case "mixed-null":
					return []*net.MX{{Host: "."}, {Host: "example.com."}}, nil
				case "temporary":
					return nil, &net.DNSError{IsTemporary: true}
				case "implicit":
					return nil, &net.DNSError{IsNotFound: true}
				case "too-many":
					m := make([]*net.MX, 21)
					for i := range m {
						m[i] = &net.MX{Host: "example.com."}
					}
					return m, nil
				case "invalid-host":
					return []*net.MX{{Host: "127.0.0.1."}}, nil
				}
				return []*net.MX{{Host: "example.com."}}, nil
			}
			directLookupIP = func(_ context.Context, _ string, host string) ([]net.IP, error) {
				if mode == "implicit" && host != "example.test." {
					t.Fatal("implicit MX changed host", host)
				}
				if mode == "mixed-addresses" {
					return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("169.254.169.254")}, nil
				}
				return []net.IP{net.ParseIP("127.0.0.1")}, nil
			}
			directDial = func(context.Context, string, string) (net.Conn, error) {
				t.Error("refused destination dialed")
				return nil, errors.New("unexpected dial")
			}
			err := directTestProfile().Deliver("mailbox@example.test", []string{"visible@example.test"}, []byte(relayProofMIME))
			var reply *textproto.Error
			if !errors.As(err, &reply) {
				t.Fatal("missing safe refusal", err)
			}
			want := 451
			if mode == "null" {
				want = 550
			}
			if reply.Code != want {
				t.Fatal("incorrect DNS classification", reply.Code, want)
			}
		})
	}
}
