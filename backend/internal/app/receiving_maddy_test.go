//go:build linux

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"golang.org/x/net/dns/dnsmessage"
)

// The child changes DNS only in a test binary, then dispatches the production
// command unchanged. There is no production lookup bypass or synthetic route.
func TestNativeReceivingCommandHelper(t *testing.T) {
	address := os.Getenv("KYPOST_TEST_DNS")
	if address == "" {
		return
	}
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	logger, err := logging.NewWithOutput(os.Stderr)
	if err != nil {
		os.Exit(1)
	}
	slog.SetDefault(slog.New(logger.Handler()))
	for i, arg := range os.Args {
		if arg == "--" {
			err = Run(os.Args[i+1:])
			if err == nil {
				os.Exit(0)
			}
			fmt.Fprintln(os.Stderr, err)
			var status interface{ ExitCode() int }
			if errors.As(err, &status) {
				os.Exit(status.ExitCode())
			}
			os.Exit(1)
		}
	}
	os.Exit(1)
}

func TestNativeReceivingMaddyRuntime(t *testing.T) {
	binary := os.Getenv("MADDY_PROOF_BINARY")
	if binary == "" {
		t.Skip("set MADDY_PROOF_BINARY to pinned Maddy 0.9.5")
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) != "6ea4b951f15b91fd81d98957e4d4bad7a0cec6d6e1d66b011c765cc9a14e05db" {
		t.Fatal("unqualified receiving binary")
	}
	r, created := receivingFixture(t)
	domain, err := r.domains.Read()
	if err != nil {
		t.Fatal(err)
	}
	dns, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dns.Close() })
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, peer, err := dns.ReadFrom(buffer)
			if err != nil {
				return
			}
			var query dnsmessage.Message
			if query.Unpack(buffer[:n]) != nil {
				continue
			}
			response := dnsmessage.Message{Header: dnsmessage.Header{ID: query.ID, Response: true, Authoritative: true, RecursionDesired: query.RecursionDesired, RecursionAvailable: true}, Questions: query.Questions}
			for _, question := range query.Questions {
				if question.Type == dnsmessage.TypeTXT && question.Name.String() == domain.RecordName()+"." {
					response.Answers = append(response.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET, TTL: 1}, Body: &dnsmessage.TXTResource{TXT: []string{domain.RecordValue()}}})
				}
			}
			packet, err := response.Pack()
			if err == nil {
				_, _ = dns.WriteTo(packet, peer)
			}
		}
	}()
	t.Setenv("KYPOST_TEST_DNS", dns.LocalAddr().String())
	t.Setenv("CONFIG_DIR", r.configDir)
	t.Setenv("STATE_DIR", r.stateDir)
	t.Setenv("KYPOST_NATIVE_MAIL", "true")
	t.Setenv("KYPOST_NATIVE_RECEIVING", "true")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	preflight := exec.Command(os.Args[0], "-test.run=^TestNativeReceivingCommandHelper$", "--", "receiving", "accept", "not-an-accepted-id", "")
	preflight.Stdin = bytes.NewReader([]byte("x"))
	output, err := preflight.CombinedOutput()
	if err == nil || !bytes.Contains(output, []byte("no rows in result set")) {
		t.Fatalf("actual command failed before bounded DATA check: %v %s", err, output)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	root := t.TempDir()
	config := fmt.Sprintf(`hostname receiver.example.test
state_dir %s/state
runtime_dir %s/run
tls off
log stderr
smtp tcp://%s {
    max_message_size 4M
    check {
        command %s -test.run=^TestNativeReceivingCommandHelper$ -- receiving bind "{msg_id}" "{sender}" "{address}" {
            run_on rcpt
            code 1 reject 451 4.3.0 "Receiving storage unavailable"
            code 3 reject 550 5.1.1 "Recipient unavailable"
        }
        command %s -test.run=^TestNativeReceivingCommandHelper$ -- receiving accept "{msg_id}" "{sender}" {
            run_on body
            code 1 reject 451 4.3.0 "Receiving storage unavailable"
            code 3 reject 451 4.3.0 "Routing unavailable"
        }
    }
    destination example.test { deliver_to dummy }
    default_destination { reject }
}
`, root, root, address, strconv.Quote(os.Args[0]), strconv.Quote(os.Args[0]))
	configPath := filepath.Join(root, "maddy.conf")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "gateway.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	cmd := exec.Command(binary, "--config", configPath, "run")
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	until := time.Now().Add(10 * time.Second)
	var client *smtp.Client
	for time.Now().Before(until) {
		client, err = smtp.Dial(address)
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil {
		output, _ := os.ReadFile(logPath)
		t.Fatalf("receiver startup: %v %s", err, output)
	}
	defer client.Close()
	if err := client.Mail(""); err != nil {
		t.Fatal(err)
	}
	var rejection *textproto.Error
	if err := client.Rcpt("unknown@example.test"); !errors.As(err, &rejection) || rejection.Code != 550 {
		t.Fatalf("unknown recipient must be permanently refused: %v", err)
	}
	for _, recipient := range []string{"one@example.test", "two@example.test"} {
		if err := client.Rcpt(recipient); err != nil {
			output, _ := os.ReadFile(logPath)
			t.Fatalf("native RCPT: %v %s", err, output)
		}
	}
	wire := []byte("From: sender@outside.test\r\nTo: one@example.test\r\nMessage-ID: <forged-receipt>\r\nX-KyPost-Owner: attacker\r\nSubject: actual runtime\r\n\r\n.dot\r\n")
	w, err := client.Data()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(wire); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		output, _ := os.ReadFile(logPath)
		t.Fatalf("native DATA: %v %s", err, output)
	}
	ctx := context.Background()
	rows, err := r.holding.List(ctx, receivingGateway, 0, 100)
	if err != nil || len(rows) != 1 || rows[0].State != "pending" {
		t.Fatalf("SMTP success without one durable obligation: %+v %v", rows, err)
	}
	delivery, err := r.holding.Get(ctx, receivingGateway, rows[0].ID)
	if err != nil || len(delivery.Bindings) != 2 || !bytes.HasSuffix(delivery.Raw, wire) {
		t.Fatalf("envelope/raw mismatch after actual helper: bindings=%d error=%v", len(delivery.Bindings), err)
	}
	// Test-only physical pressure: extend the already-open shared-memory file
	// sparsely, preserving SQLite's live index and every accepted payload. This
	// does not fill the host disk or alter production limits/configuration.
	pressureDB, err := sql.Open("sqlite", "file:"+filepath.Join(r.stateDir, "receiving", "ingress.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pressureDB.Close()
	if _, err := pressureDB.Exec("UPDATE routes SET valid_until=0"); err != nil {
		t.Fatal(err)
	}
	shm := filepath.Join(r.stateDir, "receiving", "ingress.db-shm")
	if err := os.Truncate(shm, 1<<30); err != nil {
		t.Fatal(err)
	}
	if err := client.Mail(""); err != nil {
		t.Fatal(err)
	}
	if err := client.Rcpt("one@example.test"); !errors.As(err, &rejection) || rejection.Code != 451 {
		t.Fatalf("physical pressure must temporarily refuse new RCPT: %v", err)
	}
	logger, err := logging.NewWithOutput(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, stop := context.WithCancel(ctx)
	defer stop()
	done, err := startReceivingImport(workerCtx, runDeps{nativeReceiving: true, logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("receiving importer did not drain on cancellation")
		}
	})
	until = time.Now().Add(5 * time.Second)
	for {
		stored, err := r.holding.Get(ctx, receivingGateway, delivery.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.State == "imported" {
			break
		}
		if time.Now().After(until) {
			t.Fatalf("actual daemon importer did not acknowledge delivery: %s", stored.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := client.Reset(); err != nil {
		t.Fatal(err)
	}
	if err := client.Mail(""); err != nil {
		t.Fatal(err)
	}
	if err := client.Rcpt("one@example.test"); !errors.As(err, &rejection) || rejection.Code != 451 {
		t.Fatalf("import must not reopen new admission under continuing pressure: %v", err)
	}
	for _, u := range created {
		a, _, err := r.life.NativeAssignment(u.NativeMailboxIssuer, u.SSOSub)
		if err != nil {
			t.Fatal(err)
		}
		store, err := mailbox.OpenExisting(filepath.Join(r.stateDir, "users", u.ID, "mailbox"), a.Owner, a.Limits, a.Source)
		if err != nil {
			t.Fatal(err)
		}
		got, err := store.Raw(ctx, "INBOX", 1)
		_ = store.Close()
		if err != nil || !bytes.Equal(got, delivery.Raw) {
			t.Fatalf("native owner did not receive exact accepted MIME: %v", err)
		}
	}
}
