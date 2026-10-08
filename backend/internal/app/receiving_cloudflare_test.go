//go:build linux

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

func cloudflareFixtureMessage(t *testing.T, r *receivingRuntime) (cloudflareMessage, []byte) {
	t.Helper()
	claim, err := r.cloudflareRoute(context.Background(), "one@example.test")
	if err != nil {
		t.Fatal(err)
	}
	id, err := fsutil.NewUUIDv4()
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: controlled@outside.test\r\nTo: one@example.test\r\nSubject: original bytes\r\nAuthentication-Results: forged; spf=pass\r\nContent-Type: application/octet-stream\r\n\r\n\x00\xff\x80PGP\r\n")
	hash := sha256.Sum256(raw)
	return cloudflareMessage{ID: id, Sender: "controlled@outside.test", CapturedAt: time.Now().Unix(), Size: int64(len(raw)), Digest: hex.EncodeToString(hash[:]), Route: claim}, raw
}

func startCloudflareTestScanner(t *testing.T, before func()) *httptest.Server {
	t.Helper()
	lockReceivingRspamdProof(t)
	t.Setenv("KYPOST_RECEIVING_RSPAMD", "true")
	listener, err := net.Listen("tcp", "127.0.0.1:11333")
	if err != nil {
		t.Fatal(err)
	}
	scanner := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Settings") != `{"groups_disabled":["spf","dmarc","arc"]}` || req.Header.Get("IP") != "" || req.Header.Get("Helo") != "" {
			t.Error("scanner used invented peer provenance or caller policy")
		}
		if before != nil {
			before()
		}
		_, _ = w.Write([]byte(`{"action":"no action","is_skipped":false}`))
	}))
	scanner.Listener = listener
	scanner.Start()
	t.Cleanup(scanner.Close)
	return scanner
}

func TestCloudflarePickupPermanentReceiptReplayAndNamespace(t *testing.T) {
	r, created := receivingFixture(t)
	r.gateway = cloudflareGateway
	m, raw := cloudflareFixtureMessage(t, r)
	scanner := startCloudflareTestScanner(t, nil)
	ctx := context.Background()
	// Capture can outlive its route window: pickup uses fresh current authority.
	m.Route.ValidUntil = time.Now().Add(-time.Hour).Unix()
	m.CapturedAt = m.Route.ValidUntil - 1
	if err := r.pickupCloudflare(ctx, m, raw); err != nil {
		t.Fatal(err)
	}
	scanner.Close()
	if err := r.pickupCloudflare(ctx, m, raw); err != nil {
		t.Fatal("committed replay depended on scanner", err)
	}
	d, err := r.holding.Get(ctx, cloudflareGateway, m.ID)
	if err != nil || d.State != "archived" || len(d.Raw) != 0 || d.Digest != m.Digest {
		t.Fatal("receipt not durable", d, err)
	}
	a, _, err := r.life.NativeAssignment(m.Route.Issuer, m.Route.Subject)
	if err != nil {
		t.Fatal(err)
	}
	store, err := mailbox.OpenExisting(filepath.Join(r.stateDir, "users", created[0].ID, "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Raw(ctx, "INBOX", 1)
	_ = store.Close()
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("binary MIME changed", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(r.stateDir, "users", created[0].ID, "mailbox", "mailbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var receipts, messages int
	if err := db.QueryRow("SELECT count(*) FROM receipts WHERE gateway=? AND delivery=?", cloudflareGateway, m.ID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM messages").Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 || messages != 1 {
		t.Fatal("pickup duplicated local delivery", receipts, messages)
	}
	// Equal event IDs under Maddy cannot adopt Cloudflare delivery history.
	direct := *r
	direct.gateway = receivingGateway
	if err := direct.bind(ctx, m.ID, "other@outside.test", "two@example.test"); err != nil {
		t.Fatal(err)
	}
	other, err := r.holding.Get(ctx, receivingGateway, m.ID)
	if err != nil || other.State != "staged" || other.Sender != "other@outside.test" {
		t.Fatal("gateway namespaces collided", err)
	}
	changed := m
	changed.Sender = "replacement@outside.test"
	if err := r.pickupCloudflare(ctx, changed, raw); err == nil {
		t.Fatal("changed envelope accepted")
	}
	changedRaw := []byte("Subject: replacement\r\n\r\nreplacement")
	changed = m
	hash := sha256.Sum256(changedRaw)
	changed.Size = int64(len(changedRaw))
	changed.Digest = hex.EncodeToString(hash[:])
	if err := r.pickupCloudflare(ctx, changed, changedRaw); err == nil {
		t.Fatal("changed body accepted with reused id")
	}
}

func TestCloudflarePickupRefusesChangedAuthorityAndRetainsProvider(t *testing.T) {
	for _, field := range []string{"issuer", "subject", "mailbox", "source", "revision", "recipient", "hold", "disabled", "DNS", "scanner"} {
		t.Run(field, func(t *testing.T) {
			r, created := receivingFixture(t)
			r.gateway = cloudflareGateway
			m, raw := cloudflareFixtureMessage(t, r)
			t.Setenv("KYPOST_RECEIVING_RSPAMD", "true")
			switch field {
			case "issuer":
				m.Route.Issuer = "https://foreign.example.test"
			case "subject":
				m.Route.Subject = "two"
			case "mailbox":
				m.Route.Mailbox = created[1].ID
			case "source":
				m.Route.Source += "foreign"
			case "revision":
				m.Route.Revision++
			case "recipient":
				m.Route.Recipient = "two@example.test"
			case "hold":
				if err := os.WriteFile(filepath.Join(r.stateDir, sso.NativeRestoreHoldFile), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if _, err := r.accounts.Deactivate(created[0].ID); err != nil {
					t.Fatal(err)
				}
			case "DNS":
				r.domains.SetLookupForTest(func(context.Context, string) ([]string, error) { return nil, nil })
			case "scanner":
				t.Setenv("KYPOST_RECEIVING_RSPAMD", "false")
			}
			before := append([]byte(nil), raw...)
			if err := r.pickupCloudflare(context.Background(), m, raw); err == nil {
				t.Fatal("changed authority allowed")
			}
			if !bytes.Equal(raw, before) {
				t.Fatal("provider payload changed")
			}
		})
	}
}

func TestCloudflareScanRechecksRevocationAndOutage(t *testing.T) {
	r, created := receivingFixture(t)
	r.gateway = cloudflareGateway
	m, raw := cloudflareFixtureMessage(t, r)
	scanner := startCloudflareTestScanner(t, func() {
		if _, err := r.accounts.Deactivate(created[0].ID); err != nil {
			t.Error(err)
		}
	})
	if err := r.pickupCloudflare(context.Background(), m, raw); err == nil {
		t.Fatal("revocation during scan bypassed")
	}
	d, err := r.holding.Get(context.Background(), cloudflareGateway, m.ID)
	if err != nil || d.State != "staged" || len(d.Raw) != 0 {
		t.Fatal("unauthorized bytes accepted", err)
	}
	scanner.Close()
	if _, err := r.accounts.Reactivate(created[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := r.pickupCloudflare(context.Background(), m, raw); err == nil {
		t.Fatal("new pickup bypassed scanner outage")
	}
	if _, err := r.holding.Get(context.Background(), receivingGateway, m.ID); err == nil {
		t.Fatal("Cloudflare staged under Maddy")
	}
}

func TestCloudflareHTTPSPickupBoundsAndIntegrity(t *testing.T) {
	r, _ := receivingFixture(t)
	m, raw := cloudflareFixtureMessage(t, r)
	for _, variant := range []string{"exact", "digest", "size", "sender", "id", "capture", "future", "window", "trailing", "unknown", "oversize", "redirect", "failure"} {
		t.Run(variant, func(t *testing.T) {
			envelope := m
			switch variant {
			case "digest":
				envelope.Digest = strings.Repeat("0", 64)
			case "size":
				envelope.Size++
			case "sender":
				envelope.Sender = "From: attacker"
			case "id":
				envelope.ID = "sender-message-id"
			case "capture":
				envelope.CapturedAt = envelope.Route.ValidUntil + 1
			case "future":
				envelope.CapturedAt = time.Now().Add(time.Hour).Unix()
			case "window":
				envelope.Route.ValidUntil = envelope.CapturedAt + 901
			}
			encoded, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			if variant == "trailing" {
				encoded = append(encoded, []byte(" {}")...)
			}
			if variant == "unknown" {
				encoded = []byte(`{"id":"unknown","policy":"skip"}`)
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Header.Get("Authorization") != "Bearer dedicated-token" || req.URL.Path != "/message" || req.Method != "GET" {
					t.Error("wrong pickup request")
				}
				w.Header().Set("X-Kypost-Envelope", base64.StdEncoding.EncodeToString(encoded))
				if variant == "redirect" {
					w.Header().Set("Location", "https://private.invalid/message")
					w.WriteHeader(302)
					return
				}
				if variant == "failure" {
					w.WriteHeader(503)
					_, _ = w.Write([]byte("sensitive correspondence"))
					return
				}
				if variant == "oversize" {
					_, _ = w.Write(make([]byte, receivingLimits.MessageBytes+1))
					return
				}
				_, _ = w.Write(raw)
			}))
			defer server.Close()
			client := server.Client()
			client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
			got, body, err := fetchCloudflareMessage(context.Background(), client, server.URL, "dedicated-token")
			if variant == "exact" {
				if err != nil || got != m || !bytes.Equal(body, raw) {
					t.Fatal("exact HTTPS failed", err)
				}
			} else if err == nil {
				t.Fatal("invalid HTTPS accepted")
			}
			if err != nil && strings.Contains(err.Error(), "sensitive") {
				t.Fatal("provider response leaked")
			}
		})
	}
	for _, origin := range []string{"http://x.workers.dev", "https://evil.test", "https://x.workers.dev.evil.test", "https://x.workers.dev:443", "https://token@x.workers.dev", "https://x.workers.dev?token=secret", "https://x.workers.dev/other", "https://x.workers.dev#secret"} {
		if _, err := cloudflareOrigin(origin); err == nil {
			t.Fatal("unsafe origin accepted", origin)
		}
	}
	if got, err := cloudflareOrigin("https://pilot.operator.workers.dev/"); err != nil || got != "https://pilot.operator.workers.dev" {
		t.Fatal(got, err)
	}
}
