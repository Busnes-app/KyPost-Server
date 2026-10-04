//go:build linux

package app

import (
	"bufio"
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

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

func TestNativeOutboundWorkerTLSProcess(t *testing.T) {
	root := os.Getenv("KYPOST_OUTBOUND_WORKER_TEST_ROOT")
	if root == "" {
		return
	}
	port, err := strconv.Atoi(os.Getenv("KYPOST_OUTBOUND_WORKER_TEST_PORT"))
	if err != nil {
		t.Fatal(err)
	}
	r, created := receivingFixture(t)
	u := created[0]
	secretDir := t.TempDir()
	ctx := context.Background()
	sender := sso.NativeOutbound{ConfigDir: r.configDir, StateRoot: r.stateDir, SecretDir: secretDir, Accounts: r.accounts, Domains: r.domains, Settings: sso.NewStore(r.configDir)}
	_, err = mailmsg.SaveDomainRelay(ctx, filepath.Join(r.configDir, "native-relay.json"), filepath.Join(secretDir, "native-relay.key"), mailmsg.DomainRelay{Domain: "example.test", Issuer: u.NativeMailboxIssuer, Host: "127.0.0.1", Port: port, Username: "operator", Password: "test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	raw := mailmsg.Message{From: "one@example.test", To: []string{"recipient@outside.test"}, Subject: "worker", Body: "worker body"}.Build()
	job := mailbox.OutboundJob{From: "one@example.test", NativeSendEpoch: u.NativeSendEpoch, PGPRevision: u.PGPRevision, Deliveries: []mailbox.OutboundDelivery{{Recipients: []string{"recipient@outside.test"}, Raw: raw}}, Sent: raw}
	ids := []string{}
	for range 2 {
		id, err := fsutil.NewUUIDv4()
		if err != nil {
			t.Fatal(err)
		}
		if err = sender.Queue(ctx, u.ID, id, job); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	// Oldest-first tie-break is UUID order, so identify the actual claimed job below.
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := startNativeOutboundRuntime(workerCtx, runDeps{users: r.accounts, logger: newTestLogger(t)}, sender)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "data-received")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker never reached SMTP DATA")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
		t.Fatal("worker returned before recording active SMTP outcome")
	case <-time.After(50 * time.Millisecond):
	}
	if err = os.WriteFile(filepath.Join(root, "release-ack"), []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not join completed SMTP")
	}
	accepted, queued := 0, 0
	for _, id := range ids {
		statuses, saved, err := sender.Status(ctx, u.ID, id)
		if err != nil || len(statuses) != 1 {
			t.Fatal(statuses, err)
		}
		switch statuses[0].State {
		case "accepted":
			accepted++
			if statuses[0].Attempts != 1 || !saved {
				t.Fatal("active send did not finalize after cancellation", statuses, saved)
			}
		case "queued":
			queued++
			if statuses[0].Attempts != 0 {
				t.Fatal("worker claimed fresh work after cancellation", statuses)
			}
		default:
			t.Fatal("unexpected shutdown evidence", statuses)
		}
	}
	if accepted != 1 || queued != 1 {
		t.Fatal("shutdown resent or lost queued intent", accepted, queued)
	}
}

func TestNativeOutboundWorkerJoinsActiveTLSSubmission(t *testing.T) {
	certServer := httptest.NewTLSServer(nil)
	certificate := certServer.TLS.Certificates[0]
	certServer.Close()
	root := t.TempDir()
	ca := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	defer tlsListener.Close()
	completed := make(chan error, 1)
	go func() {
		conn, err := tlsListener.Accept()
		if err != nil {
			completed <- err
			return
		}
		defer conn.Close()
		if err = conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
			completed <- err
			return
		}
		reader := bufio.NewReader(conn)
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
			case line == "MAIL FROM:<one@example.test>\r\n", line == "RCPT TO:<recipient@outside.test>\r\n":
				err = write("250 okay\r\n")
			case line == "DATA\r\n":
				if err = write("354 data\r\n"); err != nil {
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
				}
				if err != nil {
					break
				}
				if err = os.WriteFile(filepath.Join(root, "data-received"), []byte("received"), 0600); err != nil {
					break
				}
				deadline := time.Now().Add(10 * time.Second)
				for {
					if _, statErr := os.Stat(filepath.Join(root, "release-ack")); statErr == nil {
						break
					}
					if time.Now().After(deadline) {
						err = fmt.Errorf("worker failed to release SMTP fixture")
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if err == nil {
					err = write("250 accepted\r\n")
				}
			case line == "QUIT\r\n":
				err = write("221 bye\r\n")
				completed <- err
				return
			default:
				err = fmt.Errorf("unexpected worker SMTP command")
			}
		}
		completed <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeOutboundWorkerTLSProcess$")
	child.Env = append(os.Environ(), "KYPOST_OUTBOUND_WORKER_TEST_ROOT="+root, "KYPOST_OUTBOUND_WORKER_TEST_PORT="+strconv.Itoa(listener.Addr().(*net.TCPAddr).Port), "SSL_CERT_FILE="+ca, "SSL_CERT_DIR="+t.TempDir())
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("active outbox worker: %v\n%s", err, output)
	}
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("worker SMTP fixture did not finish")
	}
}
