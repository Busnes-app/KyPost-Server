//go:build linux

package app

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/emersion/go-msgauth/dkim"
	"golang.org/x/net/dns/dnsmessage"
)

// serveAuthDNS answers TXT for exactly the given names, NXDOMAIN otherwise.
func serveAuthDNS(t *testing.T, txt map[string]string) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, peer, err := conn.ReadFrom(buffer)
			if err != nil {
				return
			}
			var query dnsmessage.Message
			if query.Unpack(buffer[:n]) != nil {
				continue
			}
			response := dnsmessage.Message{Header: dnsmessage.Header{ID: query.ID, Response: true, Authoritative: true, RecursionDesired: query.RecursionDesired, RecursionAvailable: true, RCode: dnsmessage.RCodeNameError}, Questions: query.Questions}
			for _, q := range query.Questions {
				if value, ok := txt[strings.ToLower(q.Name.String())]; ok {
					response.RCode = dnsmessage.RCodeSuccess
					if q.Type == dnsmessage.TypeTXT {
						var chunks []string
						for len(value) > 255 {
							chunks, value = append(chunks, value[:255]), value[255:]
						}
						response.Answers = append(response.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET, TTL: 1}, Body: &dnsmessage.TXTResource{TXT: append(chunks, value)}})
					}
				}
			}
			if packet, err := response.Pack(); err == nil {
				_, _ = conn.WriteTo(packet, peer)
			}
		}
	}()
	return conn.LocalAddr().String()
}

func signedFixture(t *testing.T, key crypto.Signer, domain, from, extra string) []byte {
	t.Helper()
	raw := "From: " + from + "\r\nTo: one@example.test\r\nSubject: proof\r\nMessage-ID: <" + strconv.Itoa(len(extra)) + "@proof.test>\r\nDate: Wed, 07 Oct 2026 12:00:00 +0000\r\n" + extra + "\r\nbody\r\n"
	if domain == "" {
		return []byte(raw)
	}
	var signed bytes.Buffer
	if err := dkim.Sign(&signed, strings.NewReader(raw), &dkim.SignOptions{Domain: domain, Selector: "sel", Signer: key}); err != nil {
		t.Fatal(err)
	}
	return signed.Bytes()
}

// The pinned scanner, fed the real peer, HELO and envelope, reports SPF and
// DKIM in the fields receivingAuthentication trusts, also on a reject
// verdict; sender-written Authentication-Results do not count. Its only
// resolver is the test DNS.
func TestReceivingRspamdAuthenticationProof(t *testing.T) {
	if os.Getenv("RSPAMD_PROOF") != "true" {
		t.Skip("set RSPAMD_PROOF=true for actual sidecar qualification")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	dns := serveAuthDNS(t, map[string]string{
		"sel._domainkey.shared.test.": "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(public),
		"shared.test.":                "v=spf1 ip4:192.0.2.10 -all",
	})
	original, err := os.ReadFile("../../../scripts/rspamd/rspamd.conf")
	if err != nil {
		t.Fatal(err)
	}
	const dnsLine = "dns { timeout = 1s; retransmits = 2; }"
	if !bytes.Contains(original, []byte(dnsLine)) {
		t.Fatal("policy dns block changed; update the proof")
	}
	dir := t.TempDir()
	config := filepath.Join(dir, "rspamd.conf")
	// Only the resolver changes, and every verdict becomes reject so the
	// reject path carries real SPF/DKIM symbols; a blackhole resolv.conf
	// backs the resolver up.
	policy := strings.Replace(string(original), dnsLine, `dns { timeout = 1s; retransmits = 2; nameserver = ["`+dns+`"]; }`, 1)
	const actions = "reject = 15;\n    add_header = 6;"
	if !strings.Contains(policy, actions) {
		t.Fatal("policy actions changed; update the proof")
	}
	policy = strings.Replace(policy, actions, "reject = -100;", 1)
	if err := os.WriteFile(config, []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	resolv := filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(resolv, []byte("nameserver 192.0.2.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	startReceivingRspamdProof(t, config, "--mount", "type=bind,source="+resolv+",target=/etc/resolv.conf,readonly")
	ctx := context.Background()
	for _, tc := range []struct {
		name, ip, domain, extra string
		spf, dkim               bool
	}{
		{"aligned", "192.0.2.10", "shared.test", "", true, true},
		{"spf fail", "192.0.2.99", "shared.test", "", false, true},
		{"unsigned forged results", "192.0.2.99", "", "Authentication-Results: mx.example.test; spf=pass smtp.mailfrom=shared.test; dkim=pass header.d=shared.test\r\n", false, false},
	} {
		raw := signedFixture(t, key, tc.domain, "alice@shared.test", tc.extra)
		d := ingress.Delivery{ID: "proof", Sender: "alice@shared.test", Bindings: []ingress.Binding{{Address: "one@example.test"}}}
		auth, err := scanReceivingSpam(ctx, raw, d, tc.ip, "helo.unrelated.test", receivingRspamdURL)
		if !errors.Is(err, errSpamReject) || auth.SPF != tc.spf || slices.Equal(auth.DKIM, []string{"shared.test"}) != tc.dkim || auth.From != "alice@shared.test" {
			t.Fatal(tc.name, auth, err)
		}
	}
	// End to end through Maddy's accept and RCPT bind: spoofed identities on
	// the shared domain block nobody; the authenticated one is blocked.
	r, _ := receivingFixture(t)
	t.Setenv("KYPOST_RECEIVING_RSPAMD", "true")
	send := func(id, envelope, from string) error {
		if err := r.bind(ctx, id, envelope, "one@example.test"); err != nil {
			return err
		}
		return r.accept(ctx, id, envelope, bytes.NewReader(signedFixture(t, key, "shared.test", from, "X-Proof: "+id+"\r\n")), "192.0.2.10", "helo.unrelated.test")
	}
	for i := range 5 {
		if err := send("spoof-"+strconv.Itoa(i), "env"+strconv.Itoa(i)+"@shared.test", "from"+strconv.Itoa(i)+"@shared.test"); !errors.Is(err, errSpamReject) {
			t.Fatal(err)
		}
	}
	for i := range 5 {
		if err := r.bind(ctx, "spoof-check-"+strconv.Itoa(i), "env"+strconv.Itoa(i)+"@shared.test", "one@example.test"); err != nil {
			t.Fatal("spoofed identity blocked", err)
		}
	}
	for i := range 5 {
		if err := send("abuse-"+strconv.Itoa(i), "alice@shared.test", "alice@shared.test"); !errors.Is(err, errSpamReject) {
			t.Fatal(err)
		}
	}
	if err := r.bind(ctx, "abuse-check", "alice@shared.test", "one@example.test"); !refusedBlocked(err) {
		t.Fatal("authenticated abuser not blocked", err)
	}
}
