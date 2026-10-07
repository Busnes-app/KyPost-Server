package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

const cloudflareGateway = "cloudflare-worker"

// A route is a historical expectation, never continuing access authority.
// Revision carries the recipient's address generation; the wire name stays
// for the Worker.
type cloudflareRoute struct {
	Recipient  string `json:"recipient"`
	Issuer     string `json:"issuer"`
	Subject    string `json:"subject"`
	Mailbox    string `json:"mailbox"`
	Source     string `json:"source"`
	Revision   int64  `json:"revision"`
	ValidUntil int64  `json:"validUntil"`
}

func (c cloudflareRoute) matches(a sso.NativeAssignment, x sso.NativeAddress) bool {
	return c.Recipient == x.Address && c.Issuer == a.Owner.Issuer && c.Subject == a.Owner.Subject && c.Mailbox == a.Owner.Mailbox && x.Mailbox == a.Owner.Mailbox && c.Source == a.Source && c.Revision == x.Generation
}

type cloudflareMessage struct {
	ID         string          `json:"id"`
	Sender     string          `json:"sender"`
	CapturedAt int64           `json:"capturedAt"`
	Size       int64           `json:"size"`
	Digest     string          `json:"digest"`
	Route      cloudflareRoute `json:"route"`
}

var cloudflareID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func runCloudflareReceiving(args []string, output io.Writer) (result error) {
	action, correlation := "cloudflare", "cloudflare-pilot"
	replayed := false
	defer func() {
		status := "committed"
		if result != nil {
			status = "refused"
		} else if replayed {
			status = "replayed"
		}
		slog.Info("Cloudflare receiving operation", "actor", "operator", "task_id", "native-receiving", "action", action, "target", "holding-store", "result", status, "correlation_id", correlation)
	}()
	if len(args) == 0 || args[0] == "route" && len(args) != 2 || args[0] == "pickup" && len(args) != 4 || args[0] != "route" && args[0] != "pickup" {
		return errors.New("usage: receiving cloudflare route <recipient> | receiving cloudflare pickup <https-workers-origin> <private-token-file> <route-file>")
	}
	action = args[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := openReceivingRuntime(ctx, false)
	if err != nil {
		return err
	}
	defer r.holding.Close()
	r.gateway = cloudflareGateway
	if args[0] == "route" {
		claim, err := r.cloudflareRoute(ctx, args[1])
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(claim)
	}
	enabled, err := receivingRspamdEnabled()
	if err != nil {
		return err
	}
	if !enabled {
		return errors.New("cloudflare pickup requires KYPOST_RECEIVING_RSPAMD=true and the qualified local sidecar")
	}
	origin, err := cloudflareOrigin(args[1])
	if err != nil {
		return err
	}
	tokenBytes, err := receivingTLSFile(args[2], true)
	if err != nil {
		return errors.New("pickup token requires a readable owner-only regular file")
	}
	token := strings.TrimSuffix(string(tokenBytes), "\n")
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != token {
		return errors.New("pickup token must be 64 lowercase hexadecimal characters")
	}
	claimBytes, err := receivingTLSFile(args[3], true)
	if err != nil || len(claimBytes) > 4096 {
		return errors.New("pickup requires the original owner-only route file, at most 4096 bytes")
	}
	var claim cloudflareRoute
	if strictCloudflareJSON(claimBytes, &claim) != nil {
		return errors.New("invalid pickup route file")
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, MaxResponseHeaderBytes: 16 << 10, TLSHandshakeTimeout: 8 * time.Second, ResponseHeaderTimeout: 8 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	m, raw, err := fetchCloudflareMessage(ctx, client, origin, token)
	if err != nil {
		return err
	}
	correlation = m.ID
	if m.Route != claim {
		return ingress.ErrRoute
	}
	d, err := r.holding.Get(ctx, cloudflareGateway, m.ID)
	replayed = err == nil && d.State == "archived"
	return r.pickupCloudflare(ctx, m, raw)
}

func cloudflareOrigin(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "" && u.Path != "/" || !strings.HasSuffix(u.Hostname(), ".workers.dev") || u.Host != u.Hostname() {
		return "", errors.New("pickup requires a verified HTTPS workers.dev origin without credentials, ports, query or redirects")
	}
	return "https://" + u.Host, nil
}

func strictCloudflareJSON(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func fetchCloudflareMessage(ctx context.Context, client *http.Client, origin, token string) (cloudflareMessage, []byte, error) {
	var m cloudflareMessage
	failure := errors.New("cloudflare pickup unavailable or invalid; retain the R2 object and repair the route, credential or Worker")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/message", nil)
	if err != nil {
		return m, nil, failure
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return m, nil, failure
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return m, nil, failure
	}
	header := resp.Header.Get("X-Kypost-Envelope")
	if len(header) > 5500 {
		return m, nil, failure
	}
	envelope, err := base64.StdEncoding.Strict().DecodeString(header)
	if err != nil || len(envelope) > 4096 || strictCloudflareJSON(envelope, &m) != nil {
		return m, nil, failure
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, receivingLimits.MessageBytes+1))
	if err != nil || !m.valid(raw, time.Now().Unix()) {
		return m, nil, failure
	}
	return m, raw, nil
}

func (m cloudflareMessage) valid(raw []byte, now int64) bool {
	if !cloudflareID.MatchString(m.ID) || m.Size <= 0 || m.Size > receivingLimits.MessageBytes || m.Size != int64(len(raw)) || m.CapturedAt <= 0 || m.CapturedAt > now || m.Route.ValidUntil < m.CapturedAt || m.Route.ValidUntil-m.CapturedAt > 900 || m.Route.Revision <= 0 {
		return false
	}
	if m.Sender != "" {
		a, err := mail.ParseAddress(m.Sender)
		if err != nil || a.Name != "" || a.Address != m.Sender {
			return false
		}
	}
	hash := sha256.Sum256(raw)
	return m.Digest == hex.EncodeToString(hash[:])
}

func (r *receivingRuntime) cloudflareRoute(ctx context.Context, recipient string) (cloudflareRoute, error) {
	var claim cloudflareRoute
	parsed, err := mail.ParseAddress(recipient)
	if err != nil || parsed.Name != "" || parsed.Address != recipient || recipient != strings.ToLower(recipient) {
		return claim, ingress.ErrRoute
	}
	proofs, err := r.verifyDomains(ctx, []string{recipient})
	if err != nil {
		return claim, err
	}
	a, found, err := r.life.NativeAssignmentForAddress(proofs[0].Issuer, recipient)
	if err != nil || !found {
		return claim, ingress.ErrRoute
	}
	err = r.withAuthority(ctx, []string{a.Owner.Mailbox}, []string{recipient}, proofs, func(current map[string]sso.NativeAssignment) error {
		admitted := current[a.Owner.Mailbox]
		x, err := r.activeAddress(recipient)
		if err != nil {
			return err
		}
		if admitted.Owner != a.Owner || x.Mailbox != a.Owner.Mailbox {
			return ingress.ErrRoute
		}
		claim = cloudflareRoute{Recipient: recipient, Issuer: a.Owner.Issuer, Subject: a.Owner.Subject, Mailbox: a.Owner.Mailbox, Source: admitted.Source, Revision: x.Generation, ValidUntil: time.Now().Add(15 * time.Minute).Unix()}
		return nil
	})
	return claim, err
}

func (r *receivingRuntime) pickupCloudflare(ctx context.Context, m cloudflareMessage, raw []byte) error {
	if r.gatewayID() != cloudflareGateway || !m.valid(raw, time.Now().Unix()) {
		return ingress.ErrRoute
	}
	if err := r.bindExpected(ctx, m.ID, m.Sender, m.Route.Recipient, &m.Route); err != nil {
		return err
	}
	if err := r.accept(ctx, m.ID, m.Sender, bytes.NewReader(raw)); err != nil {
		return err
	}
	// No provider ACK/delete in the pilot. Durable local receipts still make a
	// repeated pickup idempotent, including a lost local command response.
	return r.importDelivery(ctx, m.ID)
}
