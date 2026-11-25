package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/processor"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// errCleartext refuses a table-mode hop that would cross hosts unencrypted.
var errCleartext = errors.New("table mode encrypts every hop between hosts")

// checkTableTransport is "Encryption in transit" (MH7): in table mode a replica refuses to
// start with any hop that can leave its host in clear text. migrate-volumes-to-postgres
// (MH8) runs this same check. The peer apps (KYPOST_KYCALENDAR_URL, KYPOST_KYDRIVE_URL,
// KYPOST_KYMESSAGE_URL and directory pairing) and KyRecovery already accept https only.
func checkTableTransport(ctx context.Context, getenv func(string) string, issuer sso.SSOSettings) error {
	// The api listens on every interface (WEB_PORT), so plain HTTP is always off loopback.
	cert, key := strings.TrimSpace(getenv("TLS_CERT_FILE")), strings.TrimSpace(getenv("TLS_KEY_FILE"))
	if cert == "" || key == "" {
		return fmt.Errorf("%w: set TLS_CERT_FILE and TLS_KEY_FILE to this host's certificate; the proxy verifies it", errCleartext)
	}
	if _, err := tls.LoadX509KeyPair(cert, key); err != nil {
		return fmt.Errorf("%w: TLS_CERT_FILE and TLS_KEY_FILE do not load: %v", errCleartext, err)
	}
	if insecure, _ := strconv.ParseBool(strings.TrimSpace(getenv("ALLOW_INSECURE_SMTP"))); insecure {
		return fmt.Errorf("%w: ALLOW_INSECURE_SMTP must not be set", errCleartext)
	}
	ollama := strings.TrimSpace(getenv("OLLAMA_BASE_URL"))
	if ollama == "" {
		ollama = strings.TrimSpace(getenv("CLASSIFIER_BASE_URL"))
	}
	if ollama != "" && !httpsOrLoopback(ollama) {
		return fmt.Errorf("%w: OLLAMA_BASE_URL must use https, or run Ollama on this host's loopback", errCleartext)
	}
	for _, name := range []string{"PUSH_RELAY_URL", "APNS_RELAY_URL"} {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			if err := processor.ValidateRelayURL(v); err != nil {
				return fmt.Errorf("%w: %s: %v", errCleartext, name, err)
			}
		}
	}
	if issuer.IssuerURL != "" && (issuer.AllowInsecureIssuer || !httpsOrLoopback(issuer.IssuerURL)) {
		return fmt.Errorf("%w: the KyIdentity issuer must use https, without allowInsecureIssuer", errCleartext)
	}
	return dbbackend.CheckTableTransport(ctx)
}

// httpsOrLoopback reports an https URL, or an http one to this host's loopback.
func httpsOrLoopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Scheme, "https") {
		return true
	}
	if !strings.EqualFold(u.Scheme, "http") {
		return false
	}
	if strings.EqualFold(u.Hostname(), "localhost") {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}
