//go:build linux

package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"mime"
	"net"
	"net/http/httptest"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
)

func TestNativeOutboundAPIProcess(t *testing.T) {
	root := os.Getenv("KYPOST_NATIVE_SEND_TEST_ROOT")
	if root == "" {
		return
	}
	mode := os.Getenv("KYPOST_NATIVE_SEND_TEST_MODE")
	deviceMode := strings.HasPrefix(mode, "device-")
	if deviceMode {
		t.Setenv("SERVER_BASE_URL", "http://127.0.0.1:5866")
	}
	port, err := strconv.Atoi(os.Getenv("KYPOST_NATIVE_SEND_TEST_PORT"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATE_DIR", t.TempDir())
	s := newNativeRuntimeServer(t)
	var diagnosticLog bytes.Buffer
	s.logger, err = logging.NewWithOutput(&diagnosticLog)
	if err != nil {
		t.Fatal(err)
	}
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "outbound-create", 1, runtimeDirectoryUser(true)))
	u, err := s.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	_, err = mailmsg.SaveDomainRelay(context.Background(), filepath.Join(s.configDir, "native-relay.json"), filepath.Join(config.SecretDir(), "native-relay.key"), mailmsg.DomainRelay{Domains: []string{"example.test"}, Issuer: "https://idp.example", Host: "127.0.0.1", Port: port, Username: "operator-login", Password: "operator-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if mode == "recovery" {
		qualifyNativeFollowOnRecovery(t, s, u.ID)
		return
	}
	path := "/api/mail/send"
	body := []byte(`{"to":"visible@outside.test","bcc":"hidden@outside.test","subject":"native — café ✉","body":"native compose"}`)
	var sender, recipient *pgpmail.Identity
	if mode == "pgp" || mode == "device-pgp" {
		sender, err = pgpmail.GenerateIdentity("Sender", "one@example.test")
		if err != nil {
			t.Fatal(err)
		}
		recipient, err = pgpmail.GenerateIdentity("Recipient", "visible@outside.test")
		if err != nil {
			t.Fatal(err)
		}
		source := mailmsg.Message{From: "one@example.test", To: []string{"visible@outside.test"}, Subject: "protected native subject", Body: "native signed secret"}.Build()
		wire, err := pgpmail.EncryptMIME(source, []string{recipient.ArmoredPublicKey}, sender)
		if err != nil {
			t.Fatal(err)
		}
		sent, err := pgpmail.EncryptMIME(source, []string{sender.ArmoredPublicKey}, sender)
		if err != nil {
			t.Fatal(err)
		}
		expected, normalizeErr := mailmsg.NormalizeSMTPMessage([]byte(strings.TrimSpace(string(wire))))
		if normalizeErr != nil {
			t.Fatal(normalizeErr)
		}
		// SMTP dot framing supplies the final line terminator after the last MIME boundary.
		expected = append(expected, []byte("\r\n")...)
		if err = os.WriteFile(filepath.Join(root, "expected.eml"), expected, 0600); err != nil {
			t.Fatal(err)
		}
		body, err = json.Marshal(clientEncryptedSendRequest{Deliveries: []clientEncryptedDelivery{{Recipients: []string{"visible@outside.test"}, Ciphertext: string(wire)}}, SentCopy: string(sent), SentCopyEncrypted: true})
		if err != nil {
			t.Fatal(err)
		}
		path = "/api/mail/send-pgp"
	}
	var deviceID, deviceSecret string
	request := func(method, path string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if deviceID == "" {
			authRequestAs(s, r, u.ID)
		} else {
			setDeviceHeaders(r, deviceID, deviceSecret)
		}
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return w
	}
	if deviceMode {
		deviceID, deviceSecret = qualifyNativeDeviceReceiving(t, s, u.ID, request)
	}
	anonymous := httptest.NewRequest("GET", "/api/mail/outbox/00000000-0000-0000-0000-000000000000", nil)
	unauthorized := httptest.NewRecorder()
	s.routes().ServeHTTP(unauthorized, anonymous)
	if unauthorized.Code != 401 {
		t.Fatal("anonymous outbox access", unauthorized.Code)
	}
	var w *httptest.ResponseRecorder
	if mode == "device-android" {
		w = qualifyNativeAndroidMail(t, s, u.ID)
	} else {
		w = request("POST", path, body)
	}
	want := 200
	if mode == "lost-ack" || mode == "refused" {
		want = 503
	}
	if w.Code != want {
		t.Fatalf("native send: %d %s", w.Code, w.Body.String())
	}
	var reply struct {
		OK        bool   `json:"ok"`
		SentSaved bool   `json:"sentSaved"`
		ID        string `json:"outboxId"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &reply); err != nil || len(reply.ID) != 36 {
		t.Fatal("missing durable intent diagnostic", reply, err)
	}
	diagnostic := request("GET", "/api/mail/outbox/"+reply.ID, nil)
	if diagnostic.Code != 200 {
		t.Fatal("outbox status unavailable", diagnostic.Code, diagnostic.Body.String())
	}
	other := runtimeDirectoryUser(true)
	other["id"] = "other-native-subject"
	other["externalId"] = "other-native-subject"
	other["userName"] = "other-native-user"
	other["emails"] = []map[string]any{{"value": "other@example.test", "primary": true}}
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "other-native-create", 1, other))
	otherUser, otherErr := s.users.GetBySSOSubIssuer("https://idp.example", "other-native-subject")
	if otherErr != nil {
		t.Fatal(otherErr)
	}
	otherRequest := httptest.NewRequest("GET", "/api/mail/outbox/"+reply.ID, nil)
	authRequestAs(s, otherRequest, otherUser.ID)
	otherResponse := httptest.NewRecorder()
	s.routes().ServeHTTP(otherResponse, otherRequest)
	if otherResponse.Code != 503 || bytes.Contains(otherResponse.Body.Bytes(), []byte("accepted")) {
		t.Fatal("cross-owner outbox evidence leaked", otherResponse.Code, otherResponse.Body.String())
	}
	statuses, saved, err := s.nativeOutbound().Status(context.Background(), u.ID, reply.ID)
	if err != nil || len(statuses) != 1 {
		t.Fatal(statuses, err)
	}
	if mode == "refused" {
		if statuses[0].State != "failed" || statuses[0].Attempts != 1 || statuses[0].NextAttempt != 0 || saved {
			t.Fatal("refusal changed durable outcome", statuses, saved)
		}
		log := diagnosticLog.String()
		if !strings.Contains(log, reply.ID) || !strings.Contains(log, "smtp_554") || strings.Contains(log, "operator-secret") || strings.Contains(log, "hidden@outside.test") || strings.Contains(log, "untrusted-reply") {
			t.Fatal("missing or unsafe native refusal diagnostic")
		}
		return
	}
	wantState := "accepted"
	if mode == "lost-ack" {
		wantState = "uncertain"
	}
	if statuses[0].State != wantState || statuses[0].Attempts != 1 || saved != (mode != "lost-ack") {
		t.Fatal("incorrect delivery evidence", statuses, saved)
	}
	if mode == "lost-ack" {
		if !strings.Contains(diagnosticLog.String(), reply.ID) || !strings.Contains(diagnosticLog.String(), "uncertain") {
			t.Fatal("lost acknowledgment diagnostic missing")
		}
		if err = s.nativeOutbound().Recover(context.Background(), u.ID, reply.ID); err != nil {
			t.Fatal(err)
		}
		statuses, _, err = s.nativeOutbound().Status(context.Background(), u.ID, reply.ID)
		if err != nil || statuses[0].Attempts != 1 {
			t.Fatal("lost acknowledgment retried", statuses, err)
		}
		return
	}
	if !reply.OK || !reply.SentSaved {
		t.Fatal("confirmed send did not retain Sent", reply)
	}
	assignment, _, err := s.nativeMailAssignment(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	box, err := mailbox.OpenExisting(filepath.Join(s.userStateDir(u.ID), "mailbox"), assignment.Owner, assignment.Limits, assignment.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	messages, err := box.List(context.Background(), "Sent", 0, 10)
	if err != nil || len(messages) != 1 {
		t.Fatal("Sent missing or duplicated", messages, err)
	}
	sent, err := box.Raw(context.Background(), "Sent", messages[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if mode == "pgp" || mode == "device-pgp" {
		wire, err := os.ReadFile(filepath.Join(root, "received.eml"))
		if err != nil {
			t.Fatal(err)
		}
		expected, err := os.ReadFile(filepath.Join(root, "expected.eml"))
		if err != nil || !bytes.Equal(wire, expected) {
			t.Fatalf("PGP wire bytes changed: received=%d expected=%d suffix=%q/%q error=%v", len(wire), len(expected), wire[max(0, len(wire)-12):], expected[max(0, len(expected)-12):], err)
		}
		for _, item := range []struct {
			raw      []byte
			identity *pgpmail.Identity
		}{{wire, recipient}, {sent, sender}} {
			result, err := pgpmail.DecryptMIME(extractArmoredPGPMessage(t, item.raw), item.identity, []string{sender.ArmoredPublicKey})
			if err != nil || !result.Verified {
				t.Fatal("recipient or Sent PGP verification failed", err)
			}
			body, _, _, parseErr := pgpmail.ParseContent(result.Content)
			if parseErr != nil || body != "native signed secret" {
				t.Fatal("native PGP plaintext changed", parseErr)
			}
		}
	} else {
		if !bytes.Contains(sent, []byte("hidden@outside.test")) {
			t.Fatal("Sent lost BCC")
		}
		if mode != "device-android" {
			wire, err := os.ReadFile(filepath.Join(root, "received.eml"))
			if err != nil {
				t.Fatal(err)
			}
			identities := map[string]bool{}
			for _, raw := range [][]byte{wire, sent} {
				msg, err := mail.ReadMessage(bytes.NewReader(raw))
				if err != nil {
					t.Fatal(err)
				}
				ids, dates := msg.Header["Message-Id"], msg.Header["Date"]
				if len(ids) != 1 || !strings.HasSuffix(ids[0], "@example.test>") || len(dates) != 1 {
					t.Fatalf("Message-ID %q / Date %q", ids, dates)
				}
				identities[ids[0]+" "+dates[0]] = true
				header := msg.Header.Get("Subject")
				for _, b := range []byte(header) {
					if b >= 128 {
						t.Fatal("SMTP or Sent subject contains raw non-ASCII bytes")
					}
				}
				subject, err := new(mime.WordDecoder).DecodeHeader(header)
				if err != nil || subject != "native — café ✉" {
					t.Fatalf("SMTP or Sent subject = %q, error %v", subject, err)
				}
			}
			if len(identities) != 1 {
				t.Fatalf("submitted and Sent copies differ in Message-ID/Date: %v", identities)
			}
		}
	}
	if err = s.nativeOutbound().Recover(context.Background(), u.ID, reply.ID); err != nil {
		t.Fatal(err)
	}
	messages, err = box.List(context.Background(), "Sent", 0, 10)
	if err != nil || len(messages) != 1 {
		t.Fatal("recovery duplicated Sent", messages, err)
	}
	if deviceMode {
		directoryStatus(t, postDirectory(t, s, testSyncKey, "user.updated", "roundtrip-disable", 2, runtimeDirectoryUser(false)))
		for _, path := range []string{"/api/inbox?mailbox=INBOX", "/api/mail/outbox/" + reply.ID} {
			if revoked := request("GET", path, nil); revoked.Code != 401 {
				t.Fatal("offboarded device retained mailbox access", revoked.Code)
			}
		}
	}
}

func TestNativeOutboundAPIActualTLSAndPGP(t *testing.T) {
	certServer := httptest.NewTLSServer(nil)
	certificate := certServer.TLS.Certificates[0]
	certServer.Close()
	modes := []string{"plain", "pgp", "device-plain", "device-pgp", "lost-ack", "refused", "recovery"}
	if os.Getenv("KYPOST_NATIVE_ANDROID_TEST_SERIAL") != "" {
		modes = append(modes, "device-android")
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
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
				auth, from, rcpts := false, false, 0
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
						credentials, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(line, "AUTH PLAIN ")))
						if decodeErr != nil || string(credentials) != "\x00operator-login\x00operator-secret" {
							err = fmt.Errorf("relay credential separation failed")
							break
						}
						auth = true
						err = write("235 authenticated\r\n")
					case line == "MAIL FROM:<one@example.test>\r\n":
						from = true
						err = write("250 sender\r\n")
					case strings.HasPrefix(line, "RCPT TO:<"):
						rcpts++
						err = write("250 recipient\r\n")
					case line == "DATA\r\n":
						if !auth || !from {
							err = fmt.Errorf("message preceded authorized sender/authentication")
							break
						}
						if err = write("354 data\r\n"); err != nil {
							break
						}
						raw := bytes.Buffer{}
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
						wantRecipients := 2
						if mode == "pgp" || mode == "device-pgp" || mode == "recovery" {
							wantRecipients = 1
						}
						if rcpts != wantRecipients || bytes.Contains(raw.Bytes(), []byte("hidden@outside.test")) {
							err = fmt.Errorf("wire leaked BCC or lost envelope recipients")
							break
						}
						if err = os.WriteFile(filepath.Join(root, "received.eml"), raw.Bytes(), 0600); err != nil {
							break
						}
						if mode == "lost-ack" {
							completed <- nil
							return
						}
						if mode == "refused" {
							completed <- write("554 untrusted-reply operator-secret hidden@outside.test\r\n")
							return
						}
						err = write("250 accepted\r\n")
					case line == "QUIT\r\n":
						err = write("221 bye\r\n")
						completed <- err
						return
					default:
						err = fmt.Errorf("unexpected SMTP command")
					}
				}
				completed <- err
			}()
			timeout := 30 * time.Second
			if mode == "device-android" {
				timeout = 2 * time.Minute
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeOutboundAPIProcess$")
			child.Env = append(os.Environ(), "KYPOST_NATIVE_SEND_TEST_ROOT="+root, "KYPOST_NATIVE_SEND_TEST_MODE="+mode, "KYPOST_NATIVE_SEND_TEST_PORT="+strconv.Itoa(listener.Addr().(*net.TCPAddr).Port), "SSL_CERT_FILE="+ca, "SSL_CERT_DIR="+t.TempDir(), "ALLOW_INSECURE_SMTP=true")
			output, err := child.CombinedOutput()
			if err != nil {
				t.Fatalf("native API qualification: %v\n%s", err, output)
			}
			select {
			case err := <-completed:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("loopback SMTP did not complete")
			}
		})
	}
}

// Simulate the exact durable state left by a crash after primary acceptance.
// An injected Sent-only write fault must not suppress an independent BCC job.
func qualifyNativeFollowOnRecovery(t *testing.T, s *Server, userID string) {
	t.Helper()
	ctx := context.Background()
	u, err := s.users.Get(userID)
	if err != nil {
		t.Fatal(err)
	}
	assignment, _, err := s.nativeMailAssignment(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	wire := mailmsg.Message{From: assignment.Address, To: []string{"visible@outside.test"}, Subject: "recovery", Body: "recovery body"}.Build()
	job := mailbox.OutboundJob{From: assignment.Address, NativeSendEpoch: u.NativeSendEpoch, PGPRevision: u.PGPRevision, Deliveries: []mailbox.OutboundDelivery{{Recipients: []string{"visible@outside.test"}, Raw: wire}, {Recipients: []string{"hidden@outside.test"}, Raw: wire}}, Sent: wire}
	id, err := fsutil.NewUUIDv4()
	if err != nil {
		t.Fatal(err)
	}
	sender := s.nativeOutbound()
	if err = sender.Queue(ctx, userID, id, job); err != nil {
		t.Fatal(err)
	}
	box, err := mailbox.OpenExisting(filepath.Join(s.userStateDir(userID), "mailbox"), assignment.Owner, assignment.Limits, assignment.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	key, err := cryptutil.LoadKey(filepath.Join(config.SecretDir(), "native-relay.key"))
	if err != nil {
		t.Fatal(err)
	}
	relay, _, err := mailmsg.ReadDomainRelay(filepath.Join(s.configDir, "native-relay.json"), filepath.Join(config.SecretDir(), "native-relay.key"))
	if err != nil {
		t.Fatal(err)
	}
	_, claim, err := box.ClaimOutbound(ctx, key, id, 0, relay.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if err = box.CompleteOutbound(ctx, id, 0, claim, nil); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(s.userStateDir(userID), "mailbox", "mailbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec("CREATE TRIGGER refuse_sent BEFORE INSERT ON messages WHEN NEW.folder='Sent' BEGIN SELECT RAISE(ABORT,'test Sent failure'); END")
	if err != nil {
		t.Fatal(err)
	}
	if err = sender.Recover(ctx, userID, id); err == nil {
		t.Fatal("Sent fault was hidden")
	}
	statuses, saved, err := sender.Status(ctx, userID, id)
	if err != nil || len(statuses) != 2 || statuses[0].State != "accepted" || statuses[1].State != "accepted" || statuses[0].Attempts != 1 || statuses[1].Attempts != 1 || saved {
		t.Fatal("Sent fault blocked follow-on or resubmitted primary", statuses, saved, err)
	}
	if _, err = db.Exec("DROP TRIGGER refuse_sent"); err != nil {
		t.Fatal(err)
	}
	if err = sender.Recover(ctx, userID, id); err != nil {
		t.Fatal(err)
	}
	statuses, saved, err = sender.Status(ctx, userID, id)
	if err != nil || !saved || statuses[0].Attempts != 1 || statuses[1].Attempts != 1 {
		t.Fatal("Sent repair caused SMTP retry", statuses, saved, err)
	}
	messages, err := box.List(ctx, "Sent", 0, 10)
	if err != nil || len(messages) != 1 {
		t.Fatal("Sent recovery duplicated", messages, err)
	}
}
