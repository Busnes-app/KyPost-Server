package api

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestNativeMailRelayCheckRuntime(t *testing.T) {
	certServer := httptest.NewTLSServer(http.NotFoundHandler())
	cert := certServer.TLS.Certificates[0]
	certServer.Close()
	if os.Getenv("KYPOST_RELAY_CHECK_CHILD") != "1" {
		root := t.TempDir()
		ca := filepath.Join(root, "ca.pem")
		if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeMailRelayCheckRuntime$", "-test.count=1")
		cmd.Env = append(os.Environ(), "KYPOST_RELAY_CHECK_CHILD=1", "SSL_CERT_FILE="+ca, "SSL_CERT_DIR="+root)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("relay check proof: %v\n%s", err, out)
		}
		return
	}
	for _, mode := range []string{"accepted", "rotate-relay", "rotate-challenge", "disable-issuer", "restore-hold", "dns-lost", "auth-echo"} {
		t.Run(mode, func(t *testing.T) {
			srv := newDirectoryTestServer(t)
			const password = "long-password-for-relay-check"
			admin, err := srv.users.Create(context.Background(), "relay-check-admin", password, users.RoleAdmin)
			if err != nil {
				t.Fatal(err)
			}
			token, csrf := mintSessionForTest(srv, admin.ID)
			member, err := srv.users.Create(context.Background(), "relay-check-member", password, users.RoleUser)
			if err != nil {
				t.Fatal(err)
			}
			memberToken, memberCSRF := mintSessionForTest(srv, member.ID)
			if _, err := srv.nativeDomains.Configure(context.Background(), "example.test", srv.ssoStore.Load().IssuerURL); err != nil {
				t.Fatal(err)
			}
			lookups := 0
			srv.nativeDomains.SetLookupForTest(func(context.Context, string) ([]string, error) {
				lookups++
				d, err := srv.nativeDomains.Read()
				if mode == "dns-lost" && lookups > 1 {
					return nil, context.DeadlineExceeded
				}
				return []string{d.RecordValue()}, err
			})
			ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			_, portText, _ := net.SplitHostPort(ln.Addr().String())
			port, _ := strconv.Atoi(portText)
			path, keyPath := filepath.Join(srv.configDir, "native-relay.json"), filepath.Join(srv.configDir, "native-relay.key")
			profile, err := mailmsg.SaveDomainRelay(context.Background(), path, keyPath, mailmsg.DomainRelay{Domains: []string{"example.test"}, Issuer: srv.ssoStore.Load().IssuerURL, Host: "127.0.0.1", Port: port, Username: "relay-login", Password: "relay-secret"})
			if err != nil {
				t.Fatal(err)
			}
			call := func(token, csrf, body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest("POST", "/api/admin/mail-relay/test", strings.NewReader(body))
				req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
				req.Header.Set("X-CSRF-Token", csrf)
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				srv.routes().ServeHTTP(w, req)
				return w
			}
			body, _ := json.Marshal(map[string]string{"expectedGeneration": profile.Generation, "password": password})
			for _, c := range []struct {
				token, csrf, body string
				want              int
			}{{"", "", string(body), 401}, {memberToken, memberCSRF, string(body), 403}, {token, "", string(body), 403}, {token, csrf, `{}`, 401}, {token, csrf, `{"password":"wrong"}`, 401}, {token, csrf, `{"password":"` + password + `"}`, 400}, {token, csrf, `{"password":"` + password + `","expectedGeneration":"12345678-1234-4234-8234-123456789abc"}`, 409}} {
				if w := call(c.token, c.csrf, c.body); w.Code != c.want {
					t.Fatal("pre-network gate", w.Code, c.want, w.Body)
				}
			}
			commands := make(chan []string, 1)
			mutation := make(chan error, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					commands <- nil
					mutation <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				observed := []string{}
				defer func() { commands <- observed }()
				write := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
				write("220 relay proof")
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					observed = append(observed, line)
					switch {
					case strings.HasPrefix(line, "EHLO "):
						write("250-relay\r\n250 AUTH PLAIN")
					case strings.HasPrefix(line, "AUTH PLAIN "):
						var changeErr error
						switch mode {
						case "rotate-relay":
							_, changeErr = mailmsg.SaveDomainRelay(context.Background(), path, keyPath, profile)
						case "rotate-challenge":
							_, changeErr = srv.nativeDomains.Configure(context.Background(), profile.Domains[0], profile.Issuer)
						case "disable-issuer":
							st := srv.ssoStore.Load()
							st.Enabled = false
							changeErr = srv.ssoStore.Save(st)
						case "restore-hold":
							changeErr = os.WriteFile(filepath.Join(srv.stateDir, sso.NativeRestoreHoldFile), []byte("held during check"), 0600)
						}
						mutation <- changeErr
						if mode == "auth-echo" {
							write("535 relay-login relay-secret")
						} else {
							write("235 authenticated")
						}
					case line == "QUIT\r\n":
						write("221 bye")
						return
					case line == "*\r\n":
						write("501 cancelled")
					default:
						return
					}
				}
			}()
			w := call(token, csrf, string(body))
			want := 409
			if mode == "accepted" {
				want = 200
			}
			if mode == "auth-echo" {
				want = 503
			}
			if w.Code != want || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "relay-secret") || strings.Contains(w.Body.String(), "relay-login") {
				t.Fatal("check result/redaction", w.Code, want, w.Body)
			}
			select {
			case err := <-mutation:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(6 * time.Second):
				t.Fatal("check held authority locks across AUTH")
			}
			select {
			case observed := <-commands:
				for _, line := range observed {
					if !strings.HasPrefix(line, "EHLO ") && !strings.HasPrefix(line, "AUTH PLAIN ") && line != "QUIT\r\n" && line != "*\r\n" {
						t.Fatalf("check sent mail: %q", line)
					}
				}
				if mode == "accepted" && len(observed) != 3 {
					t.Fatal("missing AUTH/QUIT", observed)
				}
			case <-time.After(6 * time.Second):
				t.Fatal("check connection did not close")
			}
			if mode == "accepted" {
				var result struct {
					Generation, Host                   string
					Port                               int
					TLS, Authenticated, DeliveryTested bool
				}
				if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Generation != profile.Generation || result.Host != profile.Host || result.Port != profile.Port || !result.TLS || !result.Authenticated || result.DeliveryTested {
					t.Fatal("invented delivery readiness", w.Body)
				}
				if lookups != 2 {
					t.Fatal("check reused cached proof", lookups)
				}
				if again := call(token, csrf, string(body)); again.Code != 429 || again.Header().Get("Retry-After") == "" {
					t.Fatal("missing cooldown", again.Code, again.Body)
				}
			}
		})
	}
}
