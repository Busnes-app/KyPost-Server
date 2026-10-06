package mailbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

func TestNativeOutboxSMTPProcess(t *testing.T) {
	dir := os.Getenv("KYPOST_OUTBOX_SMTP_DIR")
	if dir == "" {
		return
	}
	port, err := strconv.Atoi(os.Getenv("KYPOST_OUTBOX_SMTP_PORT"))
	must(t, err)
	relay := mailmsg.DomainRelay{Generation: outboxGeneration, Domain: "example.test", Issuer: testOwner.Issuer, Host: "127.0.0.1", Port: port, Username: "operator-login", Password: "operator-secret"}
	ctx := context.Background()
	s := openTest(t, dir, testOwner, testLimits)
	job := outboxTestJob()
	must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, job))
	delivery, claim, err := s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, relay.Generation)
	must(t, err)
	submission := relay.Deliver(job.From, delivery.Recipients, delivery.Raw)
	must(t, s.CompleteOutbound(ctx, outboxTestID, 0, claim, submission))
	_, statuses, _, err := s.ReadOutbound(ctx, outboxMaster, outboxTestID)
	must(t, err)
	if os.Getenv("KYPOST_OUTBOX_SMTP_MODE") == "lost-ack" {
		if statuses[0].State != "uncertain" {
			t.Fatal("lost acknowledgment not retained", statuses)
		}
		if _, _, err = s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, relay.Generation); err == nil {
			t.Fatal("uncertain job resent")
		}
		if _, err = s.FileOutboundSent(ctx, outboxMaster, outboxTestID); err == nil {
			t.Fatal("uncertain job asserted acceptance")
		}
	} else {
		if submission != nil || statuses[0].State != "accepted" {
			t.Fatal("relay not accepted", submission, statuses)
		}
		id, err := s.FileOutboundSent(ctx, outboxMaster, outboxTestID)
		must(t, err)
		again, err := s.FileOutboundSent(ctx, outboxMaster, outboxTestID)
		must(t, err)
		if id != again {
			t.Fatal("Sent duplicated")
		}
	}
}

func TestNativeOutboxActualTLSAcceptanceAndLostAcknowledgment(t *testing.T) {
	certificateServer := httptest.NewTLSServer(nil)
	certificate := certificateServer.TLS.Certificates[0]
	certificateServer.Close()
	for _, mode := range []string{"accepted", "lost-ack"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			ca := filepath.Join(root, "ca.pem")
			must(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0600))
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			must(t, err)
			tlsListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
			defer tlsListener.Close()
			type result struct {
				raw        []byte
				recipients int
				err        error
			}
			completed := make(chan result, 1)
			go func() {
				conn, err := tlsListener.Accept()
				if err != nil {
					completed <- result{err: err}
					return
				}
				defer conn.Close()
				if err = conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
					completed <- result{err: err}
					return
				}
				reader := bufio.NewReader(conn)
				raw := bytes.Buffer{}
				recipients := 0
				write := func(reply string) error { _, err := fmt.Fprint(conn, reply); return err }
				err = write("220 loopback\r\n")
				for err == nil {
					line, readErr := reader.ReadString('\n')
					if readErr != nil {
						err = readErr
						break
					}
					switch {
					case strings.HasPrefix(line, "EHLO "):
						err = write("250-loopback\r\n250 AUTH PLAIN\r\n")
					case strings.HasPrefix(line, "AUTH PLAIN "):
						err = write("235 authenticated\r\n")
					case line == "MAIL FROM:<alice@example.test>\r\n":
						err = write("250 sender\r\n")
					case strings.HasPrefix(line, "RCPT TO:<"):
						recipients++
						err = write("250 recipient\r\n")
					case line == "DATA\r\n":
						err = write("354 data\r\n")
						if err != nil {
							break
						}
						for {
							part, readErr := reader.ReadString('\n')
							if readErr != nil {
								err = readErr
								break
							}
							if part == ".\r\n" {
								break
							}
							if strings.HasPrefix(part, "..") {
								part = part[1:]
							}
							raw.WriteString(part)
						}
						if err != nil {
							break
						}
						if mode == "lost-ack" {
							completed <- result{raw: raw.Bytes(), recipients: recipients}
							return
						}
						err = write("250 accepted\r\n")
					case line == "QUIT\r\n":
						err = write("221 bye\r\n")
						completed <- result{raw: raw.Bytes(), recipients: recipients, err: err}
						return
					default:
						err = fmt.Errorf("unexpected synthetic SMTP command")
					}
				}
				completed <- result{err: err}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeOutboxSMTPProcess$")
			child.Env = append(os.Environ(), "KYPOST_OUTBOX_SMTP_DIR="+filepath.Join(root, "mailbox"), "KYPOST_OUTBOX_SMTP_PORT="+strconv.Itoa(listener.Addr().(*net.TCPAddr).Port), "KYPOST_OUTBOX_SMTP_MODE="+mode, "SSL_CERT_FILE="+ca, "SSL_CERT_DIR="+t.TempDir(), "ALLOW_INSECURE_SMTP=true")
			output, err := child.CombinedOutput()
			if err != nil {
				t.Fatalf("outbox/transport process: %v\n%s", err, output)
			}
			select {
			case received := <-completed:
				if received.err != nil || received.recipients != 2 || !bytes.Equal(received.raw, outboxTestJob().Deliveries[0].Raw) || bytes.Contains(received.raw, []byte("hidden@outside.test")) {
					t.Fatal("wire/envelope isolation failed", received.err, received.recipients)
				}
			case <-ctx.Done():
				t.Fatal("synthetic server did not finish")
			}
		})
	}
}
