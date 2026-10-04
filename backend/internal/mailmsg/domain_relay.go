package mailmsg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/smtp"
	"net/textproto"
	"os"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

var ErrDomainRelay = errors.New("domain relay configuration missing or invalid; preserve the configuration and master key, then restore or reconfigure through the admin API")

// DomainRelay is always encrypted on disk. Username authenticates the relay;
// it is never the mailbox's From address or authority to send as an address.
// This profile supports verified implicit TLS with mandatory authentication.
type DomainRelay struct {
	Version    int    `json:"version"`
	Generation string `json:"generation"`
	Domain     string `json:"domain"`
	Issuer     string `json:"issuer"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"smtpUsername"`
	Password   string `json:"smtpPassword"`
}

// Deliver is the native relay transport boundary, not account admission. Its
// caller must authorize the current mailbox/address and persist the outbox job
// before calling. ALLOW_INSECURE_SMTP never weakens this profile.
func (c DomainRelay) Deliver(from string, recipients []string, msg []byte) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := validateSMTPEnvelope(from, recipients); err != nil {
		return err
	}
	_, domain, _ := strings.Cut(from, "@")
	if strings.ToLower(domain) != c.Domain || len(recipients) == 0 {
		return errors.New("domain relay sender or recipients refused; authorize the mailbox address in the configured domain")
	}
	normalized, err := NormalizeSMTPMessage(msg)
	if err != nil {
		return err
	}
	if err := smtpSendWithImplicitTLS(c.Host, c.Port, c.Username, c.Password, from, recipients, normalized, 45*time.Second, true); err != nil {
		return domainRelaySubmissionError{cause: err}
	}
	return nil
}

// Check authenticates the saved relay without sending an envelope or message.
// Success proves this connection only, not From authority or delivery readiness.
func (c DomainRelay) Check(ctx context.Context) error {
	if err := c.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := withImplicitTLSSMTP(ctx, c.Host, c.Port, c.Username, c.Password, 15*time.Second, true, func(client *smtp.Client) error { return client.Quit() }); err != nil {
		return domainRelayCheckError{cause: err}
	}
	return ctx.Err()
}

type domainRelayCheckError struct{ cause error }

func (e domainRelayCheckError) Unwrap() error { return e.cause }
func (e domainRelayCheckError) Error() string {
	return "relay connection/authentication check failed; verify host, port, trusted certificate and provider credentials. No email was sent."
}

// Native queue/API callers must not render provider responses that can echo
// AUTH credentials or message metadata. Classification retains the real cause.
type domainRelaySubmissionError struct{ cause error }

func (e domainRelaySubmissionError) Unwrap() error { return e.cause }
func (e domainRelaySubmissionError) Error() string {
	if errors.Is(e.cause, ErrSMTPAcceptanceUncertain) {
		return ErrSMTPAcceptanceUncertain.Error()
	}
	if errors.Is(e.cause, ErrSMTPAcceptedThenFailed) {
		return ErrSMTPAcceptedThenFailed.Error() + "; check provider evidence before retrying to avoid duplicates"
	}
	if errors.Is(e.cause, errSMTPRelayAuthUnavailable) {
		return errSMTPRelayAuthUnavailable.Error()
	}
	var reply *textproto.Error
	if errors.As(e.cause, &reply) && reply.Code >= 400 && reply.Code <= 599 {
		return fmt.Sprintf("domain relay submission refused (SMTP %d); check provider evidence before retrying", reply.Code)
	}
	return "domain relay submission failed; check TLS, authentication and provider evidence before retrying"
}

func relayDNSName(v string) bool {
	if len(v) > 253 || !strings.Contains(v, ".") {
		return false
	}
	for _, label := range strings.Split(v, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

func (c DomainRelay) Validate() error {
	if c.Version != 1 || len(c.Generation) != 36 || !fsutil.SafePathComponent(c.Generation) || !relayDNSName(c.Domain) || !relayDNSName(c.Host) || c.Issuer == "" || len(c.Issuer) > 2048 || strings.ContainsAny(c.Issuer, "\r\n\x00") || c.Port < 1 || c.Port > 65535 || c.Username == "" || len(c.Username) > 512 || c.Password == "" || len(c.Password) > 4096 || strings.ContainsAny(c.Username+c.Password, "\r\n\x00") {
		return ErrDomainRelay
	}
	return nil
}

// DecodeDomainRelay refuses plaintext, corrupt envelopes and malformed config.
// Used by runtime reads and backup validation of the actual collected bytes.
func DecodeDomainRelay(raw, key []byte) (DomainRelay, error) {
	if len(raw) > 64<<10 {
		return DomainRelay{}, ErrDomainRelay
	}
	envelope, ok := cryptutil.ParseEnvelope(raw)
	if !ok {
		return DomainRelay{}, ErrDomainRelay
	}
	plain, err := cryptutil.Open(envelope, key)
	var c DomainRelay
	if err != nil || json.Unmarshal(plain, &c) != nil || c.Validate() != nil {
		return DomainRelay{}, ErrDomainRelay
	}
	return c, nil
}

func ReadDomainRelay(path, keyPath string) (DomainRelay, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return DomainRelay{}, false, nil
	}
	if err != nil {
		return DomainRelay{}, false, ErrDomainRelay
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		return DomainRelay{}, false, ErrDomainRelay
	}
	key, err := cryptutil.LoadKey(keyPath) // Never replace a missing decryption key.
	if err != nil {
		return DomainRelay{}, false, ErrDomainRelay
	}
	c, err := DecodeDomainRelay(raw, key)
	return c, err == nil, err
}

// SaveDomainRelay is called only under the admin's fresh domain-proof fence.
// Its own disk lock serializes secret rotation and reads existing state before
// generating anything. A new generation prevents queued jobs adopting a later
// relay configuration silently. It does not enable sending or test the provider.
func SaveDomainRelay(ctx context.Context, path, keyPath string, c DomainRelay) (DomainRelay, error) {
	c.Version = 1
	var err error
	c.Generation, err = fsutil.NewUUIDv4()
	if err != nil || c.Validate() != nil {
		return DomainRelay{}, ErrDomainRelay
	}
	release, err := fsutil.LockFileContext(ctx, path)
	if err != nil {
		return DomainRelay{}, err
	}
	defer release()
	prior, exists, err := ReadDomainRelay(path, keyPath)
	if err != nil || exists && (prior.Domain != c.Domain || prior.Issuer != c.Issuer) {
		return DomainRelay{}, ErrDomainRelay
	}
	key, err := cryptutil.LoadOrCreateKey(keyPath)
	if err != nil {
		return DomainRelay{}, ErrDomainRelay
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return DomainRelay{}, err
	}
	envelope, err := cryptutil.Seal(plain, key)
	if err != nil {
		return DomainRelay{}, err
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return DomainRelay{}, err
	}
	if err := ctx.Err(); err != nil {
		return DomainRelay{}, err
	}
	if err := fsutil.AtomicWriteFile(path, raw, 0600); err != nil {
		return DomainRelay{}, err
	}
	return c, nil
}
