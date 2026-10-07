package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cfreceiving"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
)

const receivingRspamdURL = "http://127.0.0.1:11333/checkv2"

func receivingRspamdEnabled() (bool, error) {
	switch os.Getenv("KYPOST_RECEIVING_RSPAMD") {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errors.New("KYPOST_RECEIVING_RSPAMD must be true or false")
	}
}

var errSpamReject = errors.New("message rejected by configured spam policy")

// Scan before holding.Accept and outside authority locks. Raw MIME remains
// unchanged; neither sender-written spam headers nor verdicts grant authority.
// For direct SMTP it also returns the scanner's own SPF/DKIM result, computed
// from the real peer IP and envelope (receivingAuthentication).
func scanReceivingSpam(ctx context.Context, raw []byte, delivery ingress.Delivery, ip, helo, endpoint string) (auth ingress.Authentication, resultErr error) {
	defer func() {
		outcome := "allowed"
		if resultErr != nil {
			outcome = "refused"
		}
		slog.Info("receiving spam check", "actor", delivery.Gateway, "task_id", "native-receiving", "action", "scan", "target", "local-rspamd", "result", outcome, "correlation_id", delivery.ID)
	}()
	failure := &receivingCommandError{err: errors.New("spam scanner unavailable or invalid; restore the local Rspamd sidecar and retry"), code: 5}
	address, err := netip.ParseAddr(ip)
	cloudflare := delivery.Gateway == cloudflareGateway || delivery.Gateway == cfGateway
	if cloudflare && (ip != "" || helo != "") || !cloudflare && (err != nil || address.Zone() != "" || len(helo) > 253 || strings.ContainsFunc(helo, func(c rune) bool { return c < 32 || c > 126 })) {
		return auth, failure
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return auth, failure
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if cloudflare {
		// The Email Worker supplies no original peer. MIME auth headers and
		// Cloudflare's HTTPS address cannot substitute for SMTP provenance.
		req.Header.Set("Settings", `{"groups_disabled":["spf","dmarc","arc"]}`)
	} else {
		req.Header.Set("IP", address.String())
		req.Header.Set("Helo", helo)
	}
	req.Header.Set("From", delivery.Sender)
	req.Header.Set("Queue-ID", delivery.ID)
	req.Header.Set("Pass", "all")
	for _, binding := range delivery.Bindings {
		req.Header.Add("Rcpt", binding.Address)
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return auth, failure
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return auth, failure
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (256<<10)+1))
	if err != nil || len(body) > 256<<10 {
		return auth, failure
	}
	var result struct {
		Action  string          `json:"action"`
		Skipped *bool           `json:"is_skipped"`
		Error   json.RawMessage `json:"error"`
		Symbols json.RawMessage `json:"symbols"`
	}
	if json.Unmarshal(body, &result) != nil || (len(result.Error) != 0 && string(result.Error) != "null") {
		return auth, failure
	}
	// GTUBE intentionally skips the remaining checks while forcing rejection.
	// A skipped scan can refuse mail, but cannot authorize acceptance.
	if result.Action != "reject" && (result.Skipped == nil || *result.Skipped) {
		return auth, failure
	}
	if !cloudflare {
		auth = receivingAuthentication(raw, delivery.Sender, result.Symbols)
	}
	switch result.Action {
	case "no action", "add header", "rewrite subject":
		return auth, nil
	case "reject":
		return auth, &receivingCommandError{err: errSpamReject, code: 4}
	case "soft reject", "greylist":
		return auth, &receivingCommandError{err: errors.New("spam policy temporarily deferred this message; retry later"), code: 5}
	default:
		return auth, failure
	}
}

// receivingAuthentication trusts exactly two scanner fields, both computed by
// the pinned Rspamd from the actual peer IP and MAIL FROM KyPost sent it:
// R_SPF_ALLOW (SPF pass for the envelope domain; the null sender never
// counts) and the "domain:s=selector" options of R_DKIM_ALLOW (signatures it
// verified against DNS). Authentication-Results and other message headers are
// sender-written and ignored. From is the single From header's single mailbox.
// A symbols field that does not parse yields no authentication, never a
// refusal: evidence must not change whether mail is accepted.
func receivingAuthentication(raw []byte, sender string, rawSymbols json.RawMessage) ingress.Authentication {
	auth := ingress.Authentication{Sender: sender}
	var symbols map[string]struct {
		Options []string `json:"options"`
	}
	if json.Unmarshal(rawSymbols, &symbols) != nil {
		return auth
	}
	_, auth.SPF = symbols["R_SPF_ALLOW"]
	for _, option := range symbols["R_DKIM_ALLOW"].Options {
		if domain, _, ok := strings.Cut(option, ":s="); ok {
			auth.DKIM = append(auth.DKIM, cfreceiving.LowerASCII(domain))
		}
	}
	if message, err := mail.ReadMessage(bytes.NewReader(raw)); err == nil && len(message.Header["From"]) == 1 {
		if list, err := mail.ParseAddressList(message.Header["From"][0]); err == nil && len(list) == 1 {
			auth.From = list[0].Address
		}
	}
	return auth
}
