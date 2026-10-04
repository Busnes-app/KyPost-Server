package api

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpdiscovery"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
)

func pickupSMTPFinalReply(t *testing.T, finalReply string) (string, int, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(conn)
		_, _ = conn.Write([]byte("220 test ready\r\n"))
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					if finalReply != "" {
						_, _ = conn.Write([]byte(finalReply + "\r\n"))
					}
					return // Complete DATA received, final answer optionally lost.
				}
				continue
			}
			if line == "DATA\r\n" {
				inData = true
				_, _ = conn.Write([]byte("354 send data\r\n"))
			} else {
				_, _ = conn.Write([]byte("250 test\r\n"))
			}
		}
	}()
	host, portText, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return host, port, ln.Addr().String()
}

func TestPickupNotificationPreservesMessageWhenSMTPAcknowledgmentIsLost(t *testing.T) {
	previous := mailmsg.AllowInsecureSMTP
	mailmsg.AllowInsecureSMTP = true // Test-only loopback SMTP transport.
	t.Cleanup(func() { mailmsg.AllowInsecureSMTP = previous })
	for _, finalReply := range []string{"", "451 try later", "550 rejected"} {
		t.Run(finalReply, func(t *testing.T) {
			srv := newTestServer(t)
			host, port, addr := pickupSMTPFinalReply(t, finalReply)
			err := srv.sendPickupNotification("user-1", "from@example.com", "recipient@example.com", "Subject", "Body", "plain", host, port, addr, "user", "pass")
			if err == nil || errors.Is(err, mailmsg.ErrSMTPAcceptanceUncertain) != (finalReply == "") {
				t.Fatalf("unexpected submission result: %v", err)
			}
			dir := filepath.Join(srv.stateDir, "pickup")
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if finalReply != "" {
				if len(entries) != 0 {
					t.Fatal("definitely refused notification retained a quota slot")
				}
				return
			}
			if len(entries) != 1 {
				t.Fatalf("lost acknowledgment left %d records, want 1", len(entries))
			}
			// Reopen from disk to prove retention survives a server restart.
			store := pgpmail.NewPickupStore(dir, filepath.Join(srv.configDir, "pickup-store.key"))
			id := strings.TrimSuffix(entries[0].Name(), ".json")
			subject, body, _, err := store.View(id)
			if err != nil || subject != "Subject" || body != "Body" {
				t.Fatalf("notification may have arrived but its message was lost: %q %q %v", subject, body, err)
			}
		})
	}
}

func TestMailSendKeylessLostAcknowledgmentWarnsWithoutDiscardingMessage(t *testing.T) {
	previous := mailmsg.AllowInsecureSMTP
	mailmsg.AllowInsecureSMTP = true // Test-only loopback SMTP transport.
	t.Cleanup(func() { mailmsg.AllowInsecureSMTP = previous })
	srv, userID := newPickupGateServer(t)
	host, port, _ := pickupSMTPFinalReply(t, "")
	if err := writeIMAPConfigPayload(srv.userIMAPConfigPath(userID), srv.imapConfigKeyPath, imapConfigPayload{
		Host: "imap.example.com", Port: 993, Username: "alice@example.com", Password: "pw", Mailbox: "INBOX", SMTPHost: host, SMTPPort: port,
	}); err != nil {
		t.Fatal(err)
	}
	if err := pgpdiscovery.AddSuppression(srv.userStateDir(userID), "carol@example.com", "test"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/mail/send", strings.NewReader(`{"to":"carol@example.com","subject":"Subject","body":"Body","encrypt":true,"allowPickupFallback":true}`))
	authRequest(srv, req)
	rec := httptest.NewRecorder()
	srv.withAuth(srv.handleMailSend)(rec, req)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "may already have arrived") || !strings.Contains(rec.Body.String(), "avoid duplicates") || strings.Contains(rec.Body.String(), "nothing was sent") {
		t.Fatalf("HTTP response hid uncertain acceptance: %d %s", rec.Code, rec.Body.String())
	}
	entries, err := os.ReadDir(filepath.Join(srv.stateDir, "pickup"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("uncertain link lost its message: %v %v", entries, err)
	}
	id := strings.TrimSuffix(entries[0].Name(), ".json")
	subject, body, _, err := srv.pickupStore.View(id)
	if err != nil || subject != "Subject" || body != "Body" {
		t.Fatalf("uncertain link cannot retrieve its message: %q %q %v", subject, body, err)
	}
}

// pickupMux builds a minimal ServeMux with the same route pattern server.go
// registers for the pickup page, so r.PathValue("id") resolves the way it
// would in the real server.
func pickupMux(srv *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /pickup/{id}", srv.handlePickup)
	mux.HandleFunc("POST /pickup/{id}/open", srv.handlePickupOpen)
	return mux
}

// openPickup drives the reveal step. run-4 M2 moved rendering off the GET
// landing page and onto this POST, so every test that asserts on message
// content goes through here now — the GET deliberately shows nothing.
func openPickup(srv *Server, id, token string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	pickupMux(srv).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pickup/"+id+"/open?t="+token, nil))
	return rec
}

func TestHandlePickupHappyPath(t *testing.T) {
	srv := newTestServer(t)

	id, err := srv.pickupStore.Create("user-1", "recipient@example.com", "Hello <there>", "body & stuff", "plain", time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	token, _, err := srv.createPairingToken(id, pairingPurposePickupLink, time.Hour)
	if err != nil {
		t.Fatalf("createPairingToken: %v", err)
	}

	rec := openPickup(srv, id, token)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	bodyStr := rec.Body.String()
	if !strings.Contains(bodyStr, "Hello &lt;there&gt;") {
		t.Fatalf("expected HTML-escaped subject in body, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "body &amp; stuff") {
		t.Fatalf("expected HTML-escaped body in body, got: %s", bodyStr)
	}
}

// TestHandlePickupRendersHTMLBodyAsReadableText covers the case that made the
// page useless for most real mail: the compose editor posts `mode: "html"`
// and a body of markup, so escaping that body and dropping it in a <pre>
// showed the recipient the tags themselves rather than the message.
//
// The expectation is the same one pickup-decrypt.js already holds for the
// client-sealed twin of this page: HTML is flattened to readable text, never
// rendered as markup.
func TestHandlePickupRendersHTMLBodyAsReadableText(t *testing.T) {
	srv := newTestServer(t)

	html := `<p>Hello <strong>there</strong>.</p><p>Read <a href="https://example.com/x">the notes</a>.</p>`
	id, err := srv.pickupStore.Create("user-1", "recipient@example.com", "Subject", html, "html", time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	token, _, err := srv.createPairingToken(id, pairingPurposePickupLink, time.Hour)
	if err != nil {
		t.Fatalf("createPairingToken: %v", err)
	}

	rec := openPickup(srv, id, token)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	page := rec.Body.String()
	if strings.Contains(page, "&lt;p&gt;") || strings.Contains(page, "&lt;strong&gt;") {
		t.Fatalf("recipient was shown the escaped tags instead of the message: %s", page)
	}
	// Rendered as markup would be just as wrong as showing the tags: this page
	// has no sanitizer and shares an origin with the app.
	if strings.Contains(page, "<strong>") {
		t.Fatalf("sender markup reached the page as live HTML: %s", page)
	}
	// Emphasis survives as the plain-text convention (*bold*) rather than
	// being dropped, and a link keeps its target — text extraction alone
	// would silently discard the href and leave "the notes" pointing nowhere.
	if !strings.Contains(page, "Hello *there*.") {
		t.Fatalf("expected readable text of the message, got: %s", page)
	}
	if !strings.Contains(page, "https://example.com/x") {
		t.Fatalf("expected the link target to survive flattening, got: %s", page)
	}
}

// TestHandlePickupEscapesPlainBody pins the plain-mode path: a body that was
// never HTML must still be escaped, not run through the HTML flattener, or a
// message that merely talks about markup would lose it.
func TestHandlePickupEscapesPlainBody(t *testing.T) {
	srv := newTestServer(t)

	id, err := srv.pickupStore.Create("user-1", "recipient@example.com", "Subject", "use <b> for bold", "plain", time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	token, _, err := srv.createPairingToken(id, pairingPurposePickupLink, time.Hour)
	if err != nil {
		t.Fatalf("createPairingToken: %v", err)
	}

	rec := openPickup(srv, id, token)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "use &lt;b&gt; for bold") {
		t.Fatalf("expected the plain body escaped verbatim, got: %s", rec.Body.String())
	}
}

func TestHandlePickupInvalidTokenNeverConsumesRecord(t *testing.T) {
	srv := newTestServer(t)

	id, err := srv.pickupStore.Create("user-1", "recipient@example.com", "Subject", "Body", "plain", time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/pickup/"+id+"?t=not-a-real-token", nil)
	rec := httptest.NewRecorder()
	pickupMux(srv).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}

	// The record must still be intact: an invalid token must never reach
	// pickupStore.View. Mint a real token now and confirm the record can
	// still be viewed once.
	token, _, err := srv.createPairingToken(id, pairingPurposePickupLink, time.Hour)
	if err != nil {
		t.Fatalf("createPairingToken: %v", err)
	}
	subject, body, _, err := srv.pickupStore.View(id)
	if err != nil {
		t.Fatalf("View after bad-token attempt should still succeed, got err: %v", err)
	}
	if subject != "Subject" || body != "Body" {
		t.Fatalf("unexpected record contents: subject=%q body=%q", subject, body)
	}
	_ = token // token minted only to demonstrate a valid one could still be built
}

func TestHandlePickupSecondViewIsGone(t *testing.T) {
	srv := newTestServer(t)

	id, err := srv.pickupStore.Create("user-1", "recipient@example.com", "Subject", "Body", "plain", time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	token, _, err := srv.createPairingToken(id, pairingPurposePickupLink, time.Hour)
	if err != nil {
		t.Fatalf("createPairingToken: %v", err)
	}

	// The landing page may be fetched any number of times — that is the point
	// of M2's split — so the one-time property is asserted on the reveal step.
	mux := pickupMux(srv)
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pickup/"+id+"?t="+token, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("landing fetch %d status = %d, want 200", i, rec.Code)
		}
	}

	if firstRec := openPickup(srv, id, token); firstRec.Code != http.StatusOK {
		t.Fatalf("first open status = %d, want %d; body=%s", firstRec.Code, http.StatusOK, firstRec.Body.String())
	}
	if secondRec := openPickup(srv, id, token); secondRec.Code != http.StatusGone {
		t.Fatalf("second open status = %d, want %d; body=%s", secondRec.Code, http.StatusGone, secondRec.Body.String())
	}
}

// TestHandlePickupRefusesWhenPairingSecretUnset guards against pickup tokens
// being silently HMAC-signed with a known-empty key: both the page and the
// notification sender must fail closed instead of degrading silently when
// PAIRING_SECRET was never configured.
func TestHandlePickupRefusesWhenPairingSecretUnset(t *testing.T) {
	srv := newTestServer(t)
	srv.pairingSecret = ""

	id, err := srv.pickupStore.Create("user-1", "recipient@example.com", "Subject", "Body", "plain", time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/pickup/"+id+"?t=anything", nil)
	rec := httptest.NewRecorder()
	pickupMux(srv).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}

	if err := srv.sendPickupNotification("user-1", "from@example.com", "recipient@example.com", "Subject", "Body", "plain", "smtp.example.com", 587, "smtp.example.com:587", "user", "pass"); err == nil {
		t.Fatalf("sendPickupNotification: expected error when PAIRING_SECRET is unset, got nil")
	}
}

func TestHandlePickupUnknownIDIsGone(t *testing.T) {
	srv := newTestServer(t)

	// Never Create()d: a syntactically valid token for an ID that has no
	// backing record on disk.
	token, _, err := srv.createPairingToken("never-created-id", pairingPurposePickupLink, time.Hour)
	if err != nil {
		t.Fatalf("createPairingToken: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/pickup/never-created-id?t="+token, nil)
	rec := httptest.NewRecorder()
	pickupMux(srv).ServeHTTP(rec, req)

	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusGone, rec.Body.String())
	}
}
