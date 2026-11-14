package mailmsg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

var ErrDomainRelay = errors.New("domain relay configuration missing or invalid; preserve the configuration and master key, then restore or reconfigure through the admin API")

// DomainRelay is always encrypted on disk. Username authenticates the relay;
// it is never the mailbox's From address or authority to send as an address.
// This profile supports verified implicit TLS with mandatory authentication.
// Domains may be used as From; RetiredDomains only validate historical jobs.
// Both are sorted and disjoint.
type DomainRelay struct {
	// Mode is empty for the existing relay, or direct for recipient MX delivery.
	Mode           string
	Generation     string
	Domains        []string
	RetiredDomains []string
	Issuer         string
	Host           string
	Port           int
	Username       string
	Password       string
	// DKIM holds at most one key per domain in Domains ∪ RetiredDomains, sorted
	// by domain. Only direct mode signs; relay mode keeps keys for a switch back.
	DKIM []DKIMKey
}

// Sends reports whether From may use domain.
func (c DomainRelay) Sends(domain string) bool { return slices.Contains(c.Domains, domain) }

// Historical reports whether a stored job's From domain was ever in the set.
func (c DomainRelay) Historical(domain string) bool {
	return c.Sends(domain) || slices.Contains(c.RetiredDomains, domain)
}

func (c DomainRelay) Equal(o DomainRelay) bool {
	return c.Mode == o.Mode && c.Generation == o.Generation && slices.Equal(c.Domains, o.Domains) && slices.Equal(c.RetiredDomains, o.RetiredDomains) && c.Issuer == o.Issuer && c.Host == o.Host && c.Port == o.Port && c.Username == o.Username && c.Password == o.Password && slices.EqualFunc(c.DKIM, o.DKIM, func(a, b DKIMKey) bool {
		return a.Domain == b.Domain && a.Selector == b.Selector && bytes.Equal(a.Private, b.Private)
	})
}

// Each DKIM key adds about 2.2 KiB sealed, so this fits hundreds of domains.
const maxDomainRelayFile = 1 << 20

// domainRelayFile is the sealed JSON. Version 2 holds a domain set; version 1
// (a single "domain") is read only by migration and backup validation.
type domainRelayFile struct {
	Mode           string   `json:"mode,omitempty"`
	Version        int      `json:"version"`
	Generation     string   `json:"generation"`
	Domain         string   `json:"domain,omitempty"`
	Domains        []string `json:"domains"`
	RetiredDomains []string `json:"retiredDomains"`
	Issuer         string   `json:"issuer"`
	Host           string   `json:"host"`
	Port           int      `json:"port"`
	Username       string   `json:"smtpUsername"`
	Password       string   `json:"smtpPassword"`
	DKIM           []DKIMKey `json:"dkim,omitempty"` // absent before DKIM signing existed
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
	if !c.Sends(strings.ToLower(domain)) || len(recipients) == 0 {
		return errors.New("domain relay sender or recipients refused; authorize the mailbox address in the configured domain")
	}
	normalized, err := NormalizeSMTPMessage(msg)
	if err != nil {
		return err
	}
	if c.Mode == "direct" {
		// Never send direct mail unsigned: a missing or broken key is a definite
		// refusal before any DNS or dial, which the outbox retries.
		signed, err := c.dkimSign(strings.ToLower(domain), normalized)
		if err != nil {
			return domainRelaySubmissionError{cause: directRefusal(451)}
		}
		if err := c.directDeliver(from, recipients, signed); err != nil {
			return domainRelaySubmissionError{cause: err}
		}
		return nil
	}
	if err := smtpSendWithImplicitTLS(relayDial, c.Host, c.Port, c.Username, c.Password, from, recipients, normalized, 45*time.Second, true); err != nil {
		return domainRelaySubmissionError{cause: err}
	}
	return nil
}

// Check authenticates the saved relay without sending an envelope or message.
// Success proves this connection only, not From authority or delivery readiness.
func (c DomainRelay) Check(ctx context.Context) error {
	if c.Mode == "direct" {
		return errors.New("direct delivery has no relay login to check; qualify controlled recipient delivery separately")
	}
	if err := c.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := withImplicitTLSSMTP(ctx, relayDial, c.Host, c.Port, c.Username, c.Password, 15*time.Second, true, func(client *smtp.Client) error { return client.Quit() }); err != nil {
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
	all := slices.Concat(c.Domains, c.RetiredDomains)
	if len(c.Domains) == 0 || !slices.IsSorted(c.Domains) || !slices.IsSorted(c.RetiredDomains) || len(slices.Compact(slices.Sorted(slices.Values(all)))) != len(all) || slices.ContainsFunc(all, func(d string) bool { return !relayDNSName(d) }) {
		return ErrDomainRelay
	}
	if !validDKIMKeys(c.DKIM, c.Historical) {
		return ErrDomainRelay
	}
	if c.Mode == "direct" {
		if len(c.Generation) != 36 || !fsutil.SafePathComponent(c.Generation) || !relayDNSName(c.Host) || net.ParseIP(c.Host) != nil || c.Port != 0 || c.Username != "" || c.Password != "" || c.Issuer == "" || len(c.Issuer) > 2048 || strings.ContainsAny(c.Issuer, "\r\n\x00") {
			return ErrDomainRelay
		}
		return nil
	}
	if c.Mode != "" {
		return ErrDomainRelay
	}
	if len(c.Generation) != 36 || !fsutil.SafePathComponent(c.Generation) || !relayDNSName(c.Host) || c.Issuer == "" || len(c.Issuer) > 2048 || strings.ContainsAny(c.Issuer, "\r\n\x00") || c.Port < 1 || c.Port > 65535 || c.Username == "" || len(c.Username) > 512 || c.Password == "" || len(c.Password) > 4096 || strings.ContainsAny(c.Username+c.Password, "\r\n\x00") {
		return ErrDomainRelay
	}
	return nil
}

// DecodeDomainRelay refuses plaintext, corrupt envelopes and malformed config,
// and reports the format version (1 or 2). Backup validation and migration
// accept both; runtime reads go through ReadDomainRelay, which accepts 2 only.
func DecodeDomainRelay(raw, key []byte) (DomainRelay, int, error) {
	if len(raw) > maxDomainRelayFile {
		return DomainRelay{}, 0, ErrDomainRelay
	}
	envelope, ok := cryptutil.ParseEnvelope(raw)
	if !ok {
		return DomainRelay{}, 0, ErrDomainRelay
	}
	plain, err := cryptutil.Open(envelope, key)
	var f domainRelayFile
	if err != nil || json.Unmarshal(plain, &f) != nil {
		return DomainRelay{}, 0, ErrDomainRelay
	}
	c := DomainRelay{Mode: f.Mode, Generation: f.Generation, Domains: f.Domains, RetiredDomains: f.RetiredDomains, Issuer: f.Issuer, Host: f.Host, Port: f.Port, Username: f.Username, Password: f.Password, DKIM: f.DKIM}
	switch {
	case f.Version == 1 && f.Domains == nil && f.RetiredDomains == nil && f.DKIM == nil:
		c.Domains = []string{f.Domain}
	case f.Version == 2 && f.Domain == "":
	default:
		return DomainRelay{}, 0, ErrDomainRelay
	}
	if c.Validate() != nil {
		return DomainRelay{}, 0, ErrDomainRelay
	}
	return c, f.Version, nil
}

// SealDomainRelay encrypts c as version 2 with an existing key.
func SealDomainRelay(c DomainRelay, key []byte) ([]byte, error) {
	if c.Validate() != nil {
		return nil, ErrDomainRelay
	}
	plain, err := json.Marshal(domainRelayFile{Mode: c.Mode, Version: 2, Generation: c.Generation, Domains: c.Domains, RetiredDomains: append([]string{}, c.RetiredDomains...), Issuer: c.Issuer, Host: c.Host, Port: c.Port, Username: c.Username, Password: c.Password, DKIM: c.DKIM})
	if err != nil {
		return nil, err
	}
	envelope, err := cryptutil.Seal(plain, key)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(envelope)
	if err == nil && len(raw) > maxDomainRelayFile {
		return nil, ErrDomainRelay // refuse at save rather than write an unreadable file
	}
	return raw, err
}

// ReadDomainRelayAnyVersion returns version 0 when the file is absent.
func ReadDomainRelayAnyVersion(path, keyPath string) (DomainRelay, int, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return DomainRelay{}, 0, nil
	}
	if err != nil {
		return DomainRelay{}, 0, ErrDomainRelay
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxDomainRelayFile+1))
	if err != nil || len(raw) > maxDomainRelayFile {
		return DomainRelay{}, 0, ErrDomainRelay
	}
	key, err := cryptutil.LoadKey(keyPath) // Never replace a missing decryption key.
	if err != nil {
		return DomainRelay{}, 0, ErrDomainRelay
	}
	return DecodeDomainRelay(raw, key)
}

// ReadDomainRelay is the runtime reader: version 2 only. A version-1 file
// means migrate-native has not run, and is refused like any invalid profile.
func ReadDomainRelay(path, keyPath string) (DomainRelay, bool, error) {
	c, version, err := ReadDomainRelayAnyVersion(path, keyPath)
	if err == nil && version == 1 {
		return DomainRelay{}, false, ErrDomainRelay
	}
	return c, version != 0, err
}

// SaveDomainRelay is called only under the admin's fresh domain-proof fence.
// Its own disk lock serializes secret rotation and reads existing state before
// generating anything. A new generation prevents queued jobs adopting a later
// relay configuration silently. It does not enable sending or test the provider.
func SaveDomainRelay(ctx context.Context, path, keyPath string, c DomainRelay) (DomainRelay, error) {
	return saveDomainRelay(ctx, path, keyPath, c, true)
}

// SetDomainRelayDomains changes only the saved relay's domain set and keeps its
// Generation, so queued jobs on retained domains stay valid. Callers hold the
// domain fence and have refused removing a domain that queued jobs still use.
func SetDomainRelayDomains(ctx context.Context, path, keyPath string, domains []string) (DomainRelay, error) {
	return saveDomainRelay(ctx, path, keyPath, DomainRelay{Domains: domains}, false)
}

// ReconcileDKIMKeys gives a saved direct profile a key for every sending
// domain that lacks one (a profile saved before DKIM signing existed). It keeps
// Generation and rewrites nothing when every key exists. Callers hold the
// native domain fence.
func ReconcileDKIMKeys(ctx context.Context, path, keyPath string) (bool, error) {
	release, err := fsutil.LockFileContext(ctx, path)
	if err != nil {
		return false, err
	}
	defer release()
	prior, exists, err := ReadDomainRelay(path, keyPath)
	if err != nil || !exists || prior.Mode != "direct" {
		return false, err
	}
	c := prior
	if err := c.keepDKIMKeys(prior.DKIM); err != nil {
		return false, err
	}
	if c.Equal(prior) {
		return false, nil
	}
	return true, writeDomainRelay(ctx, path, keyPath, c)
}

// Domains leaving the set move to RetiredDomains; re-added ones leave it.
func saveDomainRelay(ctx context.Context, path, keyPath string, c DomainRelay, transport bool) (DomainRelay, error) {
	domains := slices.Compact(slices.Sorted(slices.Values(c.Domains)))
	release, err := fsutil.LockFileContext(ctx, path)
	if err != nil {
		return DomainRelay{}, err
	}
	defer release()
	prior, exists, err := ReadDomainRelay(path, keyPath)
	if err != nil || !exists && !transport || exists && transport && prior.Issuer != c.Issuer {
		return DomainRelay{}, ErrDomainRelay
	}
	if transport {
		if c.Generation, err = fsutil.NewUUIDv4(); err != nil {
			return DomainRelay{}, ErrDomainRelay
		}
	} else {
		c = prior
	}
	c.Domains, c.RetiredDomains = domains, nil
	for _, domain := range slices.Concat(prior.Domains, prior.RetiredDomains) {
		if !slices.Contains(domains, domain) {
			c.RetiredDomains = append(c.RetiredDomains, domain)
		}
	}
	slices.Sort(c.RetiredDomains)
	if c.Validate() != nil {
		return DomainRelay{}, ErrDomainRelay
	}
	if err := c.keepDKIMKeys(prior.DKIM); err != nil {
		return DomainRelay{}, err
	}
	if err := writeDomainRelay(ctx, path, keyPath, c); err != nil {
		return DomainRelay{}, err
	}
	return c, nil
}

func writeDomainRelay(ctx context.Context, path, keyPath string, c DomainRelay) error {
	key, err := cryptutil.LoadOrCreateKey(keyPath)
	if err != nil {
		return ErrDomainRelay
	}
	raw, err := SealDomainRelay(c, key)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(path, raw, 0600)
}
