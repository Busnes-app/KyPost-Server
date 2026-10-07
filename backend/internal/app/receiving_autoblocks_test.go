//go:build linux

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
)

// authScanner answers every scan with verdict and the scanner's own SPF and
// DKIM symbols for domain ("" for none).
func authScanner(t *testing.T, verdict *string, domain *string) {
	t.Helper()
	lockReceivingRspamdProof(t)
	listener, err := net.Listen("tcp", "127.0.0.1:11333")
	if err != nil {
		t.Fatal("test scanner port occupied", err)
	}
	scanner := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		symbols := `{"MIME_GOOD":{"options":["text/plain"]}}`
		if *domain != "" {
			symbols = `{"R_SPF_ALLOW":{"name":"R_SPF_ALLOW","options":["+ip4:192.0.2.10"]},"R_DKIM_ALLOW":{"name":"R_DKIM_ALLOW","options":["` + *domain + `:s=sel"]}}`
		}
		_, _ = fmt.Fprintf(w, `{"action":%q,"is_skipped":false,"symbols":%s}`, *verdict, symbols)
	}))
	scanner.Listener = listener
	scanner.Start()
	t.Cleanup(scanner.Close)
	t.Setenv("KYPOST_RECEIVING_RSPAMD", "true")
}

func maddyMessage(t *testing.T, r *receivingRuntime, id, envelope, from, extra string) error {
	t.Helper()
	ctx := context.Background()
	if err := r.bind(ctx, id, envelope, "one@example.test"); err != nil {
		return err
	}
	raw := "From: " + from + "\r\nTo: one@example.test\r\nSubject: private subject line\r\n" + extra + "\r\nprivate body\r\n"
	return r.accept(ctx, id, envelope, strings.NewReader(raw), "192.0.2.10", "mx.sender.test")
}

func refusedBlocked(err error) bool {
	var commandError *receivingCommandError
	return errors.As(err, &commandError) && commandError.ExitCode() == 6
}

func TestNativeReceivingAutomaticBlocks(t *testing.T) {
	verdict, domain := "reject", "shared.test"
	authScanner(t, &verdict, &domain)
	r, _ := receivingFixture(t)
	evidence := filepath.Join(r.stateDir, "receiving", ingress.EvidenceFile)
	// The design's offline qualification at Maddy's RCPT check: five reject
	// verdicts passing DKIM and SPF for a shared domain, with envelope
	// sender and From differing, block no address.
	for i := range 5 {
		if err := maddyMessage(t, r, fmt.Sprintf("spoof-%d", i), fmt.Sprintf("env%d@shared.test", i), fmt.Sprintf("from%d@shared.test", i), ""); !errors.Is(err, errSpamReject) {
			t.Fatal("not rejected", err)
		}
	}
	for i := range 5 {
		for j, sender := range []string{fmt.Sprintf("env%d@shared.test", i), fmt.Sprintf("from%d@shared.test", i)} {
			if err := r.bind(context.Background(), fmt.Sprintf("spoof-check-%d-%d", i, j), sender, "one@example.test"); err != nil {
				t.Fatal("spoofed identity blocked at RCPT", sender, err)
			}
		}
	}
	if _, err := os.Stat(evidence); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("spoofed verdicts counted", err)
	}
	// Sender-written Authentication-Results never stand in for the scanner.
	domain = ""
	forged := "Authentication-Results: mx.example.test; spf=pass smtp.mailfrom=forged.test; dkim=pass header.d=forged.test\r\n"
	for i := range 6 {
		_ = maddyMessage(t, r, fmt.Sprintf("forged-%d", i), "victim@forged.test", "victim@forged.test", forged)
	}
	if err := r.bind(context.Background(), "forged-check", "victim@forged.test", "one@example.test"); err != nil {
		t.Fatal("forged headers blocked a sender", err)
	}
	// Only reject verdicts count: deferred or tagged mail never does.
	domain = "shared.test"
	for i, v := range []string{"add header", "no action", "rewrite subject", "add header", "add header", "soft reject", "greylist"} {
		verdict = v
		_ = maddyMessage(t, r, fmt.Sprintf("tagged-%d", i), "tagged@shared.test", "tagged@shared.test", "")
	}
	if err := r.bind(context.Background(), "tagged-check", "tagged@shared.test", "one@example.test"); err != nil {
		t.Fatal("non-reject verdicts blocked a sender", err)
	}
	// An authenticated identity: five rejects block it at RCPT (550).
	verdict = "reject"
	for i := range 5 {
		if err := maddyMessage(t, r, fmt.Sprintf("abuse-%d", i), "Spammer@shared.test", "spammer@shared.test", ""); !errors.Is(err, errSpamReject) {
			t.Fatal("not rejected", i, err)
		}
	}
	if err := r.bind(context.Background(), "abuse-blocked", "spammer@shared.test", "one@example.test"); !refusedBlocked(err) {
		t.Fatal("authenticated abuser not blocked", err)
	}
	list, err := ingress.NewBlocks(filepath.Join(r.stateDir, "receiving")).List(time.Now())
	if err != nil || len(list) != 1 || list[0].Source != "automatic" || list[0].Level != 1 || list[0].Value != "spammer@shared.test" {
		t.Fatal("automatic block", list, err)
	}
	// Accepted mail records only an authenticated domain (the tagged mail's
	// shared.test), never a forged envelope domain, an address or content.
	verdict = "no action"
	if err := maddyMessage(t, r, "good", "friend@friendly.test", "friend@friendly.test", ""); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(evidence)
	if err != nil || !bytes.Contains(raw, []byte(`"`+ingress.BlockID("domain", "shared.test")+`":`)) || bytes.Contains(raw, []byte(ingress.BlockID("domain", "friendly.test"))) ||
		bytes.Contains(raw, []byte("@")) || bytes.Contains(raw, []byte("private")) {
		t.Fatal("evidence", string(raw), err)
	}
	// Evidence work never holds up the SMTP reply: with its lock held, a
	// message is still accepted promptly and the evidence is dropped.
	release, err := fsutil.LockFileContext(context.Background(), evidence)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	defer func(old time.Duration) { evidenceTimeout = old }(evidenceTimeout)
	evidenceTimeout = 200 * time.Millisecond
	started := time.Now()
	domain = "contended.test" // a new good domain, so the lock is needed
	if err := maddyMessage(t, r, "contended", "other@contended.test", "other@contended.test", ""); err != nil || time.Since(started) > 5*time.Second {
		t.Fatal("evidence contention held the reply", err, time.Since(started))
	}
	release()
	if after, _ := os.ReadFile(evidence); !bytes.Equal(after, raw) {
		t.Fatal("contended evidence written")
	}
}

// Without the scanner nothing is authenticated, so nothing is recorded.
func TestNativeReceivingNoScannerNoEvidence(t *testing.T) {
	r, _ := receivingFixture(t)
	t.Setenv("KYPOST_RECEIVING_RSPAMD", "false")
	if err := maddyMessage(t, r, "plain", "friend@friendly.test", "friend@friendly.test", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.stateDir, "receiving", ingress.EvidenceFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("evidence recorded without a scanner", err)
	}
}

// The hosted pilot gateway has no SMTP peer: its scans never feed evidence.
func TestCloudflareReceivingFeedsNoEvidence(t *testing.T) {
	verdict, domain := "reject", "shared.test"
	authScanner(t, &verdict, &domain)
	r, _ := receivingFixture(t)
	r.gateway = cloudflareGateway
	ctx := context.Background()
	for i := range 6 {
		id := fmt.Sprintf("hosted-%d", i)
		if err := r.bind(ctx, id, "spammer@shared.test", "one@example.test"); err != nil {
			t.Fatal(err)
		}
		if err := r.accept(ctx, id, "spammer@shared.test", strings.NewReader("From: spammer@shared.test\r\n\r\nbody\r\n")); !errors.Is(err, errSpamReject) {
			t.Fatal("not rejected", err)
		}
	}
	verdict = "no action"
	if err := r.bind(ctx, "hosted-good", "friend@friendly.test", "one@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := r.accept(ctx, "hosted-good", "friend@friendly.test", strings.NewReader("From: friend@friendly.test\r\n\r\nbody\r\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.stateDir, "receiving", ingress.EvidenceFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("hosted scan fed evidence", err)
	}
}

// Exactly R_SPF_ALLOW and R_DKIM_ALLOW's d= are trusted, and From is the one
// mailbox of the one From header.
func TestReceivingAuthenticationFields(t *testing.T) {
	symbols := []byte(`{"R_SPF_ALLOW":{"options":["+ip4:192.0.2.10"]},"R_DKIM_ALLOW":{"options":["Shared.TEST:s=sel","other.test:s=x","garbage"]},"DKIM_TRACE":{"options":["forged.test:+"]}}`)
	auth := receivingAuthentication([]byte("From: Alice <alice@shared.test>\r\nAuthentication-Results: x; dkim=pass header.d=forged.test\r\n\r\nbody"), "alice@shared.test", symbols)
	if !auth.SPF || auth.From != "alice@shared.test" || strings.Join(auth.DKIM, ",") != "shared.test,other.test" {
		t.Fatal("fields", auth)
	}
	if id, ok := auth.Identity(); !ok || id != "alice@shared.test" {
		t.Fatal("identity", id)
	}
	for name, tc := range map[string]struct{ raw, symbols string }{
		"two From headers":   {"From: alice@shared.test\r\nFrom: alice@shared.test\r\n\r\n", string(symbols)},
		"two mailboxes":      {"From: alice@shared.test, bob@shared.test\r\n\r\n", string(symbols)},
		"no From":            {"Subject: x\r\n\r\n", string(symbols)},
		"no SPF symbol":      {"From: alice@shared.test\r\n\r\n", `{"R_SPF_FAIL":{},"R_DKIM_ALLOW":{"options":["shared.test:s=sel"]}}`},
		"DKIM trace only":    {"From: alice@shared.test\r\n\r\n", `{"R_SPF_ALLOW":{},"DKIM_TRACE":{"options":["shared.test:+"]}}`},
		"DKIM rejected":      {"From: alice@shared.test\r\n\r\n", `{"R_SPF_ALLOW":{},"R_DKIM_REJECT":{"options":["shared.test:s=sel"]}}`},
		"malformed symbols":  {"From: alice@shared.test\r\n\r\n", `{"R_SPF_ALLOW":{"options":[1]}}`},
		"headers only claim": {"From: alice@shared.test\r\nAuthentication-Results: x; spf=pass smtp.mailfrom=shared.test; dkim=pass header.d=shared.test\r\n\r\n", `{}`},
	} {
		if _, ok := receivingAuthentication([]byte(tc.raw), "alice@shared.test", []byte(tc.symbols)).Identity(); ok {
			t.Fatal("counted with", name)
		}
	}
}
