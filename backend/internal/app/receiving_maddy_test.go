//go:build linux

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
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
	for _, mode := range []string{"direct", "launcher", "supervised", "supervised_partial", "rspamd"} {
		t.Run(mode, func(t *testing.T) { testNativeReceivingMaddyRuntime(t, mode) })
	}
}

func testNativeReceivingMaddyRuntime(t *testing.T, mode string) {
	if mode == "rspamd" {
		if os.Getenv("RSPAMD_PROOF") != "true" {
			t.Skip("set RSPAMD_PROOF=true for actual sidecar qualification")
		}
		config, err := filepath.Abs("../../../scripts/rspamd/rspamd.conf")
		if err != nil {
			t.Fatal(err)
		}
		startReceivingRspamdProof(t, config)
		t.Setenv("KYPOST_RECEIVING_RSPAMD", "true")
	}
	if strings.HasPrefix(mode, "supervised") && os.Getenv("RECEIVING_PROOF_IMAGE") == "" {
		t.Skip("set RECEIVING_PROOF_IMAGE to the locally built image for Supervisor qualification")
	}
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
	var dnsProof atomic.Bool
	dnsProof.Store(true)
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
				if dnsProof.Load() && question.Type == dnsmessage.TypeTXT && question.Name.String() == domain.RecordName()+"." {
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
	certPath, keyPath, leaf := receivingTestCertificate(t, root)
	generator := exec.Command(os.Args[0], "-test.run=^TestNativeReceivingCommandHelper$", "--", "receiving", "config", address, leaf.DNSNames[0], certPath, keyPath)
	var diagnostics bytes.Buffer
	generator.Stderr = &diagnostics
	generated, err := generator.Output()
	if err != nil {
		t.Fatalf("configuration generation: %v %s", err, diagnostics.Bytes())
	}
	dnsProof.Store(false)
	refusedGenerator := exec.Command(os.Args[0], "-test.run=^TestNativeReceivingCommandHelper$", "--", "receiving", "config", address, leaf.DNSNames[0], certPath, keyPath)
	refused, err := refusedGenerator.Output()
	if err == nil || len(refused) != 0 {
		t.Fatal("missing live DNS proof produced configuration", err, string(refused))
	}
	dnsProof.Store(true)
	// Only the test binary's dispatch prefix changes; the production profile is
	// generated through Run with fresh DNS and existing-only storage preflight.
	config := strings.ReplaceAll(string(generated), strconv.Quote(os.Args[0])+" receiving", strconv.Quote(os.Args[0])+" -test.run=^TestNativeReceivingCommandHelper$ -- receiving")
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
	newReceiver := func() *exec.Cmd {
		return exec.Command(binary, "--config", configPath, "run")
	}
	if mode != "direct" && mode != "rspamd" {
		// The wrapper only adds the test binary dispatch prefix; all generated
		// authority/storage/TLS checks and subsequent helpers remain production.
		wrapper := filepath.Join(root, "kypost-server")
		program := fmt.Sprintf(`#!/usr/bin/python3
import os, subprocess, sys
prefix = [%s, "-test.run=^TestNativeReceivingCommandHelper$", "--"]
if sys.argv[1:3] == ["receiving", "config"]:
    result = subprocess.run(prefix + sys.argv[1:], stdout=subprocess.PIPE)
    sys.stdout.buffer.write(result.stdout.replace(%s.encode(), %s.encode()))
    sys.exit(result.returncode)
os.execv(prefix[0], prefix + sys.argv[1:])
`, strconv.Quote(os.Args[0]), strconv.Quote(strconv.Quote(os.Args[0])+" receiving"), strconv.Quote(strconv.Quote(wrapper)+" receiving"))
		if err := os.WriteFile(wrapper, []byte(program), 0o700); err != nil {
			t.Fatal(err)
		}
		engine := filepath.Join(root, "maddy")
		if err := os.WriteFile(engine, data, 0o555); err != nil {
			t.Fatal(err)
		}
		launcher, err := filepath.Abs("../../../scripts/start-receiving.sh")
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", root+":"+os.Getenv("PATH"))
		t.Setenv("KYPOST_NATIVE_RECEIVER", "true")
		t.Setenv("KYPOST_RECEIVER_BINARY", engine)
		t.Setenv("KYPOST_RECEIVING_LISTEN", address)
		t.Setenv("KYPOST_RECEIVING_HOSTNAME", leaf.DNSNames[0])
		t.Setenv("KYPOST_RECEIVING_CERT", certPath)
		t.Setenv("KYPOST_RECEIVING_KEY", keyPath)
		newReceiver = func() *exec.Cmd { return exec.Command("/bin/sh", launcher) }
		// Preflight failure must preserve prior complete config and clear temps.
		prior := filepath.Join(r.configDir, "receiving.conf")
		if err := os.WriteFile(prior, []byte("previous complete configuration"), 0o600); err != nil {
			t.Fatal(err)
		}
		dnsProof.Store(false)
		failed, err := newReceiver().CombinedOutput()
		if err == nil {
			t.Fatal("launcher started without live DNS proof", string(failed))
		}
		dnsProof.Store(true)
		retained, err := os.ReadFile(prior)
		temps, globErr := filepath.Glob(filepath.Join(r.configDir, "receiving-config.*"))
		if err != nil || string(retained) != "previous complete configuration" || globErr != nil || len(temps) != 0 {
			t.Fatal("launcher failed to preserve config or clear temporary output", err, globErr, temps)
		}
	}
	container := ""
	if strings.HasPrefix(mode, "supervised") {
		digest := sha256.Sum256([]byte(root))
		container = "kypost-receiver-proof-" + hex.EncodeToString(digest[:8])
		proofConfig := filepath.Join(root, "supervisor.conf")
		productionConfig, err := filepath.Abs("../../../supervisord.conf")
		if err != nil {
			t.Fatal(err)
		}
		// Keep the image's exact receiver/control/crash-exit configuration;
		// unrelated API/classifier programs are absent from this disposable proof.
		filter := exec.Command("python3", "-c", `import configparser, sys
c = configparser.ConfigParser(interpolation=None)
c.read(sys.argv[1])
for section in c.sections():
    if section.startswith("program:") and section != "program:receiver":
        c.remove_section(section)
with open(sys.argv[2], "w") as out:
    c.write(out)
`, productionConfig, proofConfig)
		if output, err := filter.CombinedOutput(); err != nil {
			t.Fatal(err, string(output))
		}
		args := []string{"run", "--rm", "--name", container, "--network", "host", "--user", "kypost"}
		for _, path := range []string{r.configDir, r.stateDir} {
			args = append(args, "--mount", "type=bind,source="+path+",target="+path)
		}
		for _, path := range []string{root, os.Args[0]} {
			args = append(args, "--mount", "type=bind,source="+path+",target="+path+",readonly")
		}
		args = append(args, "--mount", "type=bind,source="+proofConfig+",target=/etc/supervisord.conf,readonly")
		for _, key := range []string{"PATH", "CONFIG_DIR", "STATE_DIR", "KYPOST_TEST_DNS", "GORACE", "KYPOST_NATIVE_MAIL", "KYPOST_NATIVE_RECEIVING", "KYPOST_NATIVE_RECEIVER", "KYPOST_RECEIVER_BINARY", "KYPOST_RECEIVING_LISTEN", "KYPOST_RECEIVING_HOSTNAME", "KYPOST_RECEIVING_CERT", "KYPOST_RECEIVING_KEY"} {
			args = append(args, "-e", key+"="+os.Getenv(key))
		}
		args = append(args, "--entrypoint", "supervisord", os.Getenv("RECEIVING_PROOF_IMAGE"), "-c", "/etc/supervisord.conf")
		newReceiver = func() *exec.Cmd { return exec.Command("docker", args...) }
	}
	cmd := newReceiver()
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if container != "" {
			_ = exec.Command("docker", "rm", "-f", "-v", container).Run()
		}
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
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
	if err := client.Mail(""); err == nil {
		if err := client.Rcpt("one@example.test"); err == nil {
			t.Fatal("plaintext recipient was admitted")
		}
	}
	rowsBeforeTLS, err := r.holding.List(context.Background(), receivingGateway, 0, 100)
	if err != nil || len(rowsBeforeTLS) != 0 {
		t.Fatal("plaintext attempted delivery created bindings", rowsBeforeTLS, err)
	}
	if err := client.Reset(); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	if err := client.StartTLS(&tls.Config{RootCAs: roots, ServerName: leaf.DNSNames[0], MinVersion: tls.VersionTLS12}); err != nil {
		t.Fatal(err)
	}
	if err := client.Mail(""); err != nil {
		t.Fatal(err)
	}
	var rejection *textproto.Error
	// Limits apply to active MAIL transactions, including transactions that
	// have not bound a recipient. A third transaction from this IP must refuse.
	newTLSClient := func() *smtp.Client {
		t.Helper()
		c, err := smtp.Dial(address)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if err := c.StartTLS(&tls.Config{RootCAs: roots, ServerName: leaf.DNSNames[0], MinVersion: tls.VersionTLS12}); err != nil {
			t.Fatal(err)
		}
		return c
	}
	second, third := newTLSClient(), newTLSClient()
	if err := second.Mail(""); err != nil {
		t.Fatal(err)
	}
	if err := third.Mail(""); !errors.As(err, &rejection) || rejection.Code < 400 || rejection.Code >= 500 {
		t.Fatalf("third concurrent IP transaction must be temporarily refused: %v", err)
	}
	if err := second.Reset(); err != nil {
		t.Fatal(err)
	}
	_ = second.Close()
	_ = third.Close()
	// A blocked sender is refused permanently at RCPT, before any binding.
	if _, err := ingress.NewBlocks(filepath.Join(r.stateDir, "receiving")).Put(context.Background(), ingress.SenderBlock{Kind: "domain", Value: "blocked.test", Source: "manual", Actor: "test", Reason: "spam"}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	blockedClient := newTLSClient()
	if err := blockedClient.Mail("bad@blocked.test"); err != nil {
		t.Fatal(err)
	}
	if err := blockedClient.Rcpt("one@example.test"); !errors.As(err, &rejection) || rejection.Code != 550 || !strings.Contains(rejection.Msg, "Sender blocked") {
		t.Fatalf("blocked sender must be permanently refused: %v", err)
	}
	_ = blockedClient.Close()
	if rows, err := r.holding.List(context.Background(), receivingGateway, 0, 100); err != nil || len(rows) != 0 {
		t.Fatal("blocked sender created a binding", rows, err)
	}
	if err := client.Rcpt("unknown@example.test"); !errors.As(err, &rejection) || rejection.Code != 550 {
		t.Fatalf("unknown recipient must be permanently refused: %v", err)
	}
	if err := client.Rcpt("relay@outside.test"); !errors.As(err, &rejection) || rejection.Code != 550 {
		t.Fatalf("external relay recipient must be refused: %v", err)
	}
	if mode == "rspamd" {
		if err := client.Rcpt("one@example.test"); err != nil {
			t.Fatal(err)
		}
		writer, err := client.Data()
		if err != nil {
			t.Fatal(err)
		}
		_, err = writer.Write([]byte("From: sender@outside.test\r\nTo: one@example.test\r\nSubject: spam test\r\n\r\nXJS*C4JDBQADN1.NSBN3*2IDNEN*GTUBE-STANDARD-ANTI-UBE-TEST-EMAIL*C.34X\r\n"))
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); !errors.As(err, &rejection) || rejection.Code != 550 {
			t.Fatal("GTUBE not rejected before commit", err)
		}
		rows, err := r.holding.List(context.Background(), receivingGateway, 0, 100)
		if err != nil || len(rows) != 1 || rows[0].State != "staged" {
			t.Fatal("rejected payload committed", rows, err)
		}
		if err := client.Reset(); err != nil {
			t.Fatal(err)
		}
		if err := client.Mail(""); err != nil {
			t.Fatal(err)
		}
	}
	for _, recipient := range []string{"one@example.test", "two@example.test"} {
		if err := client.Rcpt(recipient); err != nil {
			output, _ := os.ReadFile(logPath)
			t.Fatalf("native RCPT: %v %s", err, output)
		}
	}
	wire := []byte("From: sender@outside.test\r\nTo: one@example.test\r\nMessage-ID: <forged-receipt>\r\nX-KyPost-Owner: attacker\r\nSubject: actual runtime\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=receiving-proof\r\n\r\n--receiving-proof\r\nContent-Type: text/plain\r\n\r\n.dot\r\n--receiving-proof\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=proof.bin\r\nContent-Transfer-Encoding: base64\r\n\r\nAAEC/w==\r\n--receiving-proof--\r\n")
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
	if mode == "rspamd" {
		var pending []ingress.Summary
		for _, row := range rows {
			if row.State == "pending" {
				pending = append(pending, row)
			}
		}
		rows = pending
	}
	if err != nil || len(rows) != 1 || rows[0].State != "pending" {
		t.Fatalf("SMTP success without one durable obligation: %+v %v", rows, err)
	}
	delivery, err := r.holding.Get(ctx, receivingGateway, rows[0].ID)
	if err != nil || len(delivery.Bindings) != 2 || !bytes.HasSuffix(delivery.Raw, wire) {
		t.Fatalf("envelope/raw mismatch after actual helper: bindings=%d error=%v", len(delivery.Bindings), err)
	}
	if mode == "rspamd" {
		if out, err := exec.Command("docker", "rm", "-f", "kypost-rspamd-proof-"+strconv.Itoa(os.Getpid())).CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
		if err := r.accept(ctx, delivery.ID, delivery.Sender, bytes.NewReader(delivery.Raw)); err != nil {
			t.Fatal("durable replay failed during scanner outage", err)
		}
		if err := client.Mail(""); err != nil {
			t.Fatal(err)
		}
		if err := client.Rcpt("one@example.test"); err != nil {
			t.Fatal(err)
		}
		writer, err := client.Data()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(wire); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); !errors.As(err, &rejection) || rejection.Code != 451 {
			t.Fatal("scanner outage did not temporarily refuse SMTP", err)
		}
		rows, err := r.holding.List(ctx, receivingGateway, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		pending := 0
		for _, row := range rows {
			if row.State == "pending" {
				pending++
			}
		}
		if pending != 1 {
			t.Fatal("outage committed another delivery", rows)
		}
	}
	// The configured receiver can die after SMTP acceptance without owning the
	// only copy. Restart it before importing the same durable obligation.
	_ = client.Close()
	if container != "" {
		until := time.Now().Add(10 * time.Second)
		for {
			status, err := exec.Command("docker", "exec", container, "supervisorctl", "-c", "/etc/supervisord.conf", "status", "receiver").CombinedOutput()
			if err == nil && strings.Contains(string(status), "RUNNING") {
				break
			}
			if time.Now().After(until) {
				t.Fatal("supervised receiver never reached RUNNING", err, string(status))
			}
			time.Sleep(50 * time.Millisecond)
		}
		pid, err := exec.Command("docker", "exec", container, "supervisorctl", "-c", "/etc/supervisord.conf", "pid", "receiver").Output()
		n, parseErr := strconv.Atoi(strings.TrimSpace(string(pid)))
		if err != nil || parseErr != nil || n <= 1 {
			t.Fatal("invalid receiver PID", err, parseErr)
		}
		// Kill only the recorded receiver inside its disposable PID namespace.
		if output, err := exec.Command("docker", "exec", container, "/bin/sh", "-c", `kill -KILL "$1"`, "sh", strconv.Itoa(n)).CombinedOutput(); err != nil {
			t.Fatal(err, string(output))
		}
	} else {
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		var killed *exec.ExitError
		if err := cmd.Wait(); !errors.As(err, &killed) || killed.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
			t.Fatalf("receiver did not terminate by SIGKILL: %v", err)
		}
		cmd = newReceiver()
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
	}
	until = time.Now().Add(10 * time.Second)
	for {
		client, err = smtp.Dial(address)
		if err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("receiver did not restart", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	defer client.Close()
	if err := client.StartTLS(&tls.Config{RootCAs: roots, ServerName: leaf.DNSNames[0], MinVersion: tls.VersionTLS12}); err != nil {
		t.Fatal(err)
	}
	if mode == "supervised_partial" {
		// Leave DATA open with flushed bytes, then stop the actual Supervisor.
		// The earlier accepted obligation must survive; incomplete DATA must not
		// become an importable payload or receive SMTP success.
		if err := client.Mail(""); err != nil {
			t.Fatal(err)
		}
		if err := client.Rcpt("one@example.test"); err != nil {
			t.Fatal(err)
		}
		partial, err := client.Data()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := partial.Write([]byte("Subject: interrupted DATA\r\n\r\npartial body\r\n")); err != nil {
			t.Fatal(err)
		}
		if err := client.Text.W.Flush(); err != nil {
			t.Fatal(err)
		}
		stopCtx, cancel := context.WithTimeout(ctx, 55*time.Second)
		output, stopErr := exec.CommandContext(stopCtx, "docker", "stop", "--time", "50", container).CombinedOutput()
		cancel()
		if stopErr != nil {
			t.Fatal("active-DATA shutdown exceeded its bound", stopErr, string(output))
		}
		if err := cmd.Wait(); err != nil {
			t.Fatal("Supervisor did not exit cleanly during DATA", err)
		}
		if err := partial.Close(); err == nil {
			t.Fatal("incomplete DATA received SMTP success during shutdown")
		}
		_ = client.Close()
		shutdownLog, err := os.ReadFile(logPath)
		if err != nil || !bytes.Contains(shutdownLog, []byte("stopped: receiver (exit status 0)")) {
			t.Fatal("receiver was not stopped cleanly before Supervisor exit", err, string(shutdownLog))
		}
		retained, err := r.holding.Get(ctx, receivingGateway, delivery.ID)
		if err != nil || retained.State != "pending" || !bytes.Equal(retained.Raw, delivery.Raw) {
			t.Fatal("accepted obligation changed during partial-DATA shutdown", err, retained.State)
		}
		remaining, err := r.holding.List(ctx, receivingGateway, 0, 100)
		if err != nil || len(remaining) != 2 {
			t.Fatal("expected accepted receipt and one retained partial binding", err, remaining)
		}
		for _, row := range remaining {
			if row.ID == delivery.ID {
				continue
			}
			staged, err := r.holding.Get(ctx, receivingGateway, row.ID)
			if err != nil || staged.State != "staged" || len(staged.Raw) != 0 || staged.Digest != "" || len(staged.Bindings) != 1 {
				t.Fatal("partial DATA published payload or lost its binding", err, staged.State)
			}
		}
		cmd = newReceiver()
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		until = time.Now().Add(10 * time.Second)
		for {
			client, err = smtp.Dial(address)
			if err == nil {
				break
			}
			if time.Now().After(until) {
				t.Fatal("receiver did not start after active-DATA shutdown", err)
			}
			time.Sleep(25 * time.Millisecond)
		}
		defer client.Close()
		if err := client.StartTLS(&tls.Config{RootCAs: roots, ServerName: leaf.DNSNames[0], MinVersion: tls.VersionTLS12}); err != nil {
			t.Fatal(err)
		}
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
		if stored.State == "archived" {
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
	qualifyReceivedMailAPI(t, r, created, logger, certPath, keyPath, leaf)
	_ = client.Close()
	rateClient := newTLSClient()
	rateRefused := false
	for range 11 {
		err := rateClient.Mail("")
		if err != nil {
			if !errors.As(err, &rejection) || rejection.Code < 400 || rejection.Code >= 500 {
				t.Fatalf("burst exhaustion must be temporary refusal: %v", err)
			}
			rateRefused = true
			break
		}
		if err := rateClient.Reset(); err != nil {
			t.Fatal(err)
		}
	}
	if !rateRefused {
		t.Fatal("configured per-IP burst limit never refused")
	}
	if container != "" {
		// Close the test sockets, then prove the actual image process groups stop.
		_ = rateClient.Close()
		_ = client.Close()
		if output, err := exec.Command("docker", "stop", "--time", "50", container).CombinedOutput(); err != nil {
			t.Fatal("supervised receiver did not stop", err, string(output))
		}
		if err := cmd.Wait(); err != nil {
			t.Fatal("Supervisor exited unsuccessfully", err)
		}
	}
}

func receivingTestCertificate(t *testing.T, root string) (string, string, *x509.Certificate) {
	t.Helper()
	server := httptest.NewTLSServer(nil)
	pair := server.TLS.Certificates[0]
	server.Close()
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || len(leaf.DNSNames) == 0 {
		t.Fatal("test certificate lacks hostname", err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(root, "cert.pem"), filepath.Join(root, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath, leaf
}

// startReceivingRspamdProof runs the pinned scanner with config; extra are
// further docker run options (mounts).
func startReceivingRspamdProof(t *testing.T, config string, extra ...string) {
	t.Helper()
	lockReceivingRspamdProof(t)
	listener, err := net.Listen("tcp", "127.0.0.1:11333")
	if err != nil {
		t.Fatal("proof port occupied", err)
	}
	_ = listener.Close()
	name := "kypost-rspamd-proof-" + strconv.Itoa(os.Getpid())
	image := "rspamd/rspamd:3.14.3@sha256:b2fc96714bc4e376c87c5f2ee405f6edcc7acaa10b1ad04e023c4854ef47c6fc"
	args := []string{"run", "--rm", "-d", "--name", name, "--network", "host", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--memory", "512m", "--pids-limit", "64", "--tmpfs", "/tmp:uid=11333,gid=11333,mode=0700", "--tmpfs", "/var/lib/rspamd:uid=11333,gid=11333,mode=0700", "--mount", "type=bind,source=" + config + ",target=/etc/rspamd/rspamd.conf,readonly"}
	args = append(append(args, extra...), "--entrypoint", "rspamd", image, "-f", "-c", "/etc/rspamd/rspamd.conf")
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	until := time.Now().Add(15 * time.Second)
	for time.Now().Before(until) {
		client := http.Client{Timeout: time.Second}
		resp, err := client.Get("http://127.0.0.1:11333/ping")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	out, _ := exec.Command("docker", "logs", name).CombinedOutput()
	t.Fatal("Rspamd startup failed", string(out))
}
