package ingress

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/smtp"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMaddyHoldingBoundary(t *testing.T) {
	binary := os.Getenv("MADDY_PROOF_BINARY")
	if binary == "" {
		t.Skip("set MADDY_PROOF_BINARY to the pinned Maddy 0.9.5 binary")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) != "6ea4b951f15b91fd81d98957e4d4bad7a0cec6d6e1d66b011c765cc9a14e05db" {
		t.Fatal("unqualified receiving binary")
	}
	root := t.TempDir()
	ctx := context.Background()
	s, err := Open(filepath.Join(root, "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := proofRoute("alias@example.test", "alice", 1)
	if err := s.SetRoute(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRoute(ctx, proofRoute("hidden@example.test", "bob", 1)); err != nil {
		t.Fatal(err)
	}
	// Even a valid route outside the served domain must be rejected before
	// binding, or a refused RCPT would become a phantom intended delivery.
	if err := s.SetRoute(ctx, proofRoute("relay@outside.test", "dave", 1)); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`hostname receiver.example.test
state_dir %s/state
runtime_dir %s/run
tls off
log stderr
smtp tcp://%s {
    max_message_size 1M
    check {
        command %s -test.run=^TestMaddyHelper$ -- %s bind "{msg_id}" "{sender}" "{address}" {
            run_on rcpt
            code 1 reject 451 4.3.0 "Receiving storage unavailable"
            code 3 reject 550 5.1.1 "Recipient unavailable"
        }
        command %s -test.run=^TestMaddyHelper$ -- %s accept "{msg_id}" "{sender}" {
            run_on body
            code 1 reject 451 4.3.0 "Receiving storage unavailable"
            code 3 reject 451 4.3.0 "Routing unavailable"
        }
    }
    destination example.test {
        deliver_to dummy
    }
    default_destination { reject }
}
`, root, root, addr, strconv.Quote(os.Args[0]), strconv.Quote(root), strconv.Quote(os.Args[0]), strconv.Quote(root))
	configPath := filepath.Join(root, "maddy.conf")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(root, "gateway.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	start := func() *exec.Cmd {
		cmd := exec.Command(binary, "--config", configPath, "run")
		cmd.Stdout, cmd.Stderr = log, log
		cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		until := time.Now().Add(10 * time.Second)
		for time.Now().Before(until) {
			conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return cmd
			}
			time.Sleep(50 * time.Millisecond)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		logs, _ := os.ReadFile(filepath.Join(root, "gateway.log"))
		t.Fatalf("gateway did not start: %s", logs)
		return nil
	}
	cmd := start()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	client, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Mail("sender@outside.test"); err != nil {
		t.Fatal(err)
	}
	if err := client.Rcpt("unknown@example.test"); err == nil {
		t.Fatal("unknown accepted")
	}
	if err := client.Rcpt("relay@outside.test"); err == nil {
		t.Fatal("external relay accepted")
	}
	if err := client.Rcpt(r.Address); err != nil {
		t.Fatal(err)
	}
	if err := client.Rcpt("hidden@example.test"); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := s.db.QueryRow("SELECT id FROM deliveries WHERE gateway='maddy-proof'").Scan(&id); err != nil {
		t.Fatal(err)
	}
	// Reassign between RCPT acceptance and DATA: old bindings must survive.
	if err := s.SetRoute(ctx, proofRoute(r.Address, "carol", 2)); err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: sender@outside.test\r\nTo: alias@example.test\r\nMessage-ID: <forged-delivery-id>\r\nX-KyPost-Owner: mallory\r\nSubject: unchanged\r\n\tfolded subject\r\n\r\noriginal body\r\n.dot\r\n")
	w, err := client.Data()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := s.Get(ctx, "maddy-proof", id)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID == "forged-delivery-id" || len(d.Bindings) != 2 || d.Bindings[0].Mailbox != "alice" || d.Bindings[1].Mailbox != "bob" || !bytes.HasSuffix(d.Raw, raw) {
		t.Fatalf("envelope/MIME/binding mismatch: %+v", d)
	}
	if bytes.Contains(d.Raw, []byte("hidden@example.test")) {
		t.Fatal("Bcc leaked into MIME")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	cmd = start()
	d, err = s.Get(ctx, "maddy-proof", id)
	if err != nil || !bytes.HasSuffix(d.Raw, raw) {
		t.Fatalf("receiver restart lost accepted mail: %v", err)
	}
	d, err = s.Claim(ctx, "maddy-proof", id, time.Minute)
	if err != ErrRoute || d.State != "quarantined" || d.Bindings[0].Mailbox != "alice" {
		t.Fatalf("redirected old mail: %+v %v", d, err)
	}
	// A fresh transaction on the same connection must mint a new receiver ID
	// and bind the reassigned owner, even with a repeated sender Message-ID.
	client, err = smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	send := func(raw []byte) error {
		if err := client.Mail("sender@outside.test"); err != nil {
			return err
		}
		if err := client.Rcpt(r.Address); err != nil {
			return err
		}
		w, err := client.Data()
		if err != nil {
			return err
		}
		if _, err := w.Write(raw); err != nil {
			return err
		}
		return w.Close()
	}
	if err := send(raw); err != nil {
		t.Fatal(err)
	}
	var newID string
	if err := s.db.QueryRow("SELECT id FROM deliveries WHERE state='pending'").Scan(&newID); err != nil {
		t.Fatal(err)
	}
	if newID == id {
		t.Fatal("receiver transaction ID reused")
	}
	fresh, err := s.Get(ctx, "maddy-proof", newID)
	if err != nil || fresh.Bindings[0].Mailbox != "carol" {
		t.Fatalf("new owner binding: %v", err)
	}
	// Kill the actual holding writer after its commit, before its success exit
	// can lead to upstream SMTP acceptance. Close our own handle first: recovery
	// must open the database afresh, not read through an existing connection.
	if err := os.WriteFile(filepath.Join(root, "kill-after-commit"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	sent := make(chan error, 1)
	go func() { sent <- send(raw) }()
	var marker []byte
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		marker, err = os.ReadFile(filepath.Join(root, "committed"))
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal("holding subprocess did not commit")
	}
	parts := strings.Split(string(marker), "\n")
	if len(parts) != 2 {
		t.Fatal("invalid helper marker")
	}
	pid, err := strconv.Atoi(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	writer, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := writer.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-sent:
		if err == nil {
			t.Fatal("upstream unexpectedly acknowledged killed writer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SMTP did not close after crash")
	}
	if err := os.Remove(filepath.Join(root, "kill-after-commit")); err != nil {
		t.Fatal(err)
	}
	s, err = Open(filepath.Join(root, "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recovered, err := s.Get(ctx, "maddy-proof", parts[1])
	if err != nil || !bytes.HasSuffix(recovered.Raw, raw) {
		t.Fatalf("holding writer crash lost commit: %v", err)
	}
	if err := s.Accept(ctx, "maddy-proof", parts[1], "sender@outside.test", bytes.NewReader(recovered.Raw)); err != nil {
		t.Fatal(err)
	}
	cmd = start()
	client, err = smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// Hold mail without an importer until the logical payload budget fills.
	// A real SMTP transaction must be refused temporarily, never acknowledged
	// and evicted to make space. The receiver queue is not in this pipeline.
	large := []byte("From: sender@outside.test\r\n\r\n" + strings.Repeat(strings.Repeat("x", 900)+"\r\n", 1000))
	for i := 0; i < 4; i++ {
		if err := send(large); err != nil {
			t.Fatal(err)
		}
	}
	err = send(large)
	if err == nil || !strings.Contains(err.Error(), "451") {
		t.Fatalf("full spool did not return temporary SMTP refusal: %v", err)
	}
	old, err := s.Get(ctx, "maddy-proof", id)
	if err != nil || !bytes.Equal(old.Raw, d.Raw) {
		t.Fatalf("capacity pressure lost quarantined mail: %v", err)
	}
	t.Log("real receiver: refusal, immutable owners/Bcc, forged headers ignored, exact MIME, fresh transaction IDs, SIGKILL receiver+holding writer recovery, lost-ACK replay, quarantine and full-spool 451 passed")
}
