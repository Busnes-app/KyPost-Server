package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

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

// Scan before holding.Accept and outside authority locks. Raw MIME remains
// unchanged; neither sender-written spam headers nor verdicts grant authority.
func scanReceivingSpam(ctx context.Context, raw []byte, delivery ingress.Delivery, ip, helo, endpoint string) (resultErr error) {
	defer func() {
		outcome := "allowed"
		if resultErr != nil {
			outcome = "refused"
		}
		slog.Info("receiving spam check", "actor", receivingGateway, "task_id", "native-receiving", "action", "scan", "target", "local-rspamd", "result", outcome, "correlation_id", delivery.ID)
	}()
	failure := &receivingCommandError{err: errors.New("spam scanner unavailable or invalid; restore the local Rspamd sidecar and retry"), code: 5}
	address, err := netip.ParseAddr(ip)
	if err != nil || address.Zone() != "" || len(helo) > 253 || strings.ContainsFunc(helo, func(c rune) bool { return c < 32 || c > 126 }) {
		return failure
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return failure
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("IP", address.String())
	req.Header.Set("Helo", helo)
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
		return failure
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return failure
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (256<<10)+1))
	if err != nil || len(body) > 256<<10 {
		return failure
	}
	var result struct {
		Action  string          `json:"action"`
		Skipped *bool           `json:"is_skipped"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &result) != nil || (len(result.Error) != 0 && string(result.Error) != "null") {
		return failure
	}
	// GTUBE intentionally skips the remaining checks while forcing rejection.
	// A skipped scan can refuse mail, but cannot authorize acceptance.
	if result.Action != "reject" && (result.Skipped == nil || *result.Skipped) {
		return failure
	}
	switch result.Action {
	case "no action", "add header", "rewrite subject":
		return nil
	case "reject":
		return &receivingCommandError{err: errors.New("message rejected by configured spam policy"), code: 4}
	case "soft reject", "greylist":
		return &receivingCommandError{err: errors.New("spam policy temporarily deferred this message; retry later"), code: 5}
	default:
		return failure
	}
}
