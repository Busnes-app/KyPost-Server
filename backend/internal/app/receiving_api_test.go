//go:build linux

package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/api"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/health"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// Registration is covered separately. Synthetic credentials join actual SMTP
// acceptance/import to production HTTPS routing and per-owner selection.
func qualifyReceivedMailAPI(t *testing.T, r *receivingRuntime, owners []users.User, logger *logging.Logger, certPath, keyPath string, leaf *x509.Certificate) {
	t.Helper()
	t.Setenv("SECRET_DIR", t.TempDir())
	t.Setenv("LOG_DIR", t.TempDir())
	t.Setenv("WEB_PORT", strconv.Itoa(freeTCPPort(t)))
	t.Setenv("TLS_CERT_FILE", certPath)
	t.Setenv("TLS_KEY_FILE", keyPath)
	secrets := make(map[string]string)
	for _, u := range owners {
		store, err := state.New(filepath.Join(r.stateDir, "users", u.ID))
		if err != nil {
			t.Fatal(err)
		}
		secret := rand.Text()
		err = store.UpsertNativeDevice(state.NativeDevice{DeviceID: "receiving-proof-" + u.ID, UserID: u.ID, Platform: "android", SecretHash: users.HashDeviceSecret(secret)})
		closeErr := store.Close()
		if err != nil || closeErr != nil {
			t.Fatal(err, closeErr)
		}
		secrets[u.ID] = secret
	}
	srv := api.NewServer(config.Default(), logger, health.NewService(), r.accounts, nil, nil)
	srv.EnableNativeMail()
	srv.Prepare()
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Error("receiving API did not stop")
		}
	})
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: leaf.DNSNames[0], MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	origin := "https://127.0.0.1:" + os.Getenv("WEB_PORT")
	until := time.Now().Add(5 * time.Second)
	for {
		response, err := client.Get(origin + "/healthz")
		if err == nil {
			_ = response.Body.Close()
			break
		}
		if time.Now().After(until) {
			t.Fatal("receiving API startup timeout", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	request := func(u users.User, path string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest("GET", origin+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Kypost-Device-Id", "receiving-proof-"+u.ID)
		req.Header.Set("X-Kypost-Device-Secret", secrets[u.ID])
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, data
	}
	references := make(map[string]string)
	for _, u := range owners {
		if _, err := os.Stat(filepath.Join(r.configDir, "users", u.ID, "imap-config.json")); !os.IsNotExist(err) {
			t.Fatal("received native mail required IMAP", err)
		}
		code, data := request(u, "/api/inbox?mailbox=INBOX&since=999999")
		var inbox struct {
			Delta *bool `json:"delta"`
			ByTab map[string][]struct {
				MessageID      string `json:"messageId"`
				Subject        string `json:"subject"`
				Body           string `json:"body"`
				BodyMode       string `json:"bodyMode"`
				HasAttachments bool   `json:"hasAttachments"`
			} `json:"byTab"`
		}
		if code != 200 || json.Unmarshal(data, &inbox) != nil || inbox.Delta == nil || *inbox.Delta {
			t.Fatal("received inbox/full snapshot", code, string(data))
		}
		count := 0
		for _, messages := range inbox.ByTab {
			for _, m := range messages {
				count++
				if !strings.HasPrefix(m.MessageID, "n1:") || m.Subject != "actual runtime" || !strings.Contains(m.Body, ".dot") || m.BodyMode != "plain" || !m.HasAttachments {
					t.Fatal("received MIME was not exposed through native API")
				}
				references[u.ID] = m.MessageID
			}
		}
		if count != 1 {
			t.Fatal("received API lost or duplicated a delivery", count)
		}
		query := "?mailbox=INBOX&messageId=" + url.QueryEscape(references[u.ID])
		if code, body := request(u, "/api/mail/body"+query); code != 200 || !bytes.Contains(body, []byte(".dot")) {
			t.Fatal("received body", code)
		}
		if code, attachment := request(u, "/api/mail/attachment"+query+"&index=0"); code != 200 || !bytes.Equal(attachment, []byte{0, 1, 2, 255}) {
			t.Fatal("received attachment", code)
		}
	}
	for _, owner := range owners {
		for _, other := range owners {
			if owner.ID != other.ID {
				if references[owner.ID] == references[other.ID] {
					t.Fatal("native namespaces collided")
				}
				code, _ := request(owner, "/api/mail/body?mailbox=INBOX&messageId="+url.QueryEscape(references[other.ID]))
				if code != http.StatusBadRequest {
					t.Fatal("foreign native reference accepted", code)
				}
			}
		}
	}
}
