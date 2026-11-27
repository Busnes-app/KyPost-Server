package sso

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

// NativeDomainsFile is the domain set; its sibling ".lock" is the domain fence.
// The version-1 single-domain file native-domain.json becomes a tombstone.
const NativeDomainsFile = "native-domains.json"

// nativeProofReuse is how much of a saved five-minute proof must remain for
// VerifyDomain to return it rather than rewrite the set.
const nativeProofReuse = 4 * time.Minute

const legacyNativeDomainFile = "native-domain.json"

var ErrNativeDomain = errors.New("mail domain proof missing, expired or changed; configure and verify the current DNS challenge")

var ErrNativeDomainInUse = errors.New("mail domain still in use by an active address, a queued or retryable outbox job, held incoming mail or the relay; disable those addresses, let the mail finish and remove the domain from the relay first, then retry")

var ErrNativeMigration = errors.New("native mail storage is not in the current format (migration to native-domains.json pending, failed or incomplete); native mail is refused and external IMAP is unaffected. Read the `kypost-server migrate-native` error in the container log, fix its cause and restart, or restore the pre-migration backup")

// NativeDomain is a single operator/issuer-bound mail claim. The public DNS
// challenge is not a secret and never grants receiver or SMTP readiness.
type NativeDomain struct {
	Domain        string `json:"domain"`
	Issuer        string `json:"issuer"`
	Token         string `json:"token"`
	ExpiresAt     int64  `json:"expiresAt"`
	Established   bool   `json:"established,omitempty"`
	VerifiedUntil int64  `json:"verifiedUntil,omitempty"`
}

func (d NativeDomain) RecordName() string {
	if d.Domain == "" {
		return ""
	}
	return "_kypost-mail." + d.Domain
}
func (d NativeDomain) RecordValue() string {
	if d.Token == "" {
		return ""
	}
	return "kypost-mail-verify=" + d.Token
}

// nativeDomainSet is native-domains.json version 1. Founding is the first
// configured domain still in service, which the single-domain API serves;
// files written before several domains existed omit it.
type nativeDomainSet struct {
	Version  int                          `json:"version"`
	Issuer   string                       `json:"issuer"`
	Founding string                       `json:"founding,omitempty"`
	Domains  map[string]nativeDomainProof `json:"domains"`
	Retired  []string                     `json:"retired"`
}

type nativeDomainProof struct {
	Token         string `json:"token"`
	ExpiresAt     int64  `json:"expiresAt"`
	Established   bool   `json:"established"`
	VerifiedUntil int64  `json:"verifiedUntil"`
}

// NativeDomainSet holds one issuer and one proof per domain. Retired domains
// keep their address records but have no proof, routing or sending.
type NativeDomainSet struct {
	Issuer   string
	Founding string
	Domains  map[string]NativeDomain
	Retired  []string
}

// Known reports a configured or retired domain.
func (s NativeDomainSet) Known(domain string) bool {
	_, ok := s.Domains[domain]
	return ok || slices.Contains(s.Retired, domain)
}

// AddressDomain is the lowercased domain of a bare address.
func AddressDomain(address string) string {
	return strings.ToLower(address[strings.LastIndexByte(address, '@')+1:])
}

// PlusBase drops an RFC 5233 "+detail" from address: the part before the
// first +. Resolve recipients with NativeAddressFor, which prefers an exact
// active address issued before + was refused.
func PlusBase(address string) string {
	at := strings.LastIndexByte(address, '@')
	if at < 0 {
		return address
	}
	plus := strings.IndexByte(address[:at], '+')
	if plus < 0 {
		return address
	}
	return address[:plus] + address[at:]
}

type NativeDomainStore struct {
	path   string
	lookup func(context.Context, string) ([]string, error)
}

func NewNativeDomainStore(configDir string) *NativeDomainStore {
	return &NativeDomainStore{path: filepath.Join(configDir, NativeDomainsFile), lookup: net.DefaultResolver.LookupTXT}
}

// legacyNativeDomain reports whether native-domain.json is the v2 tombstone or
// still holds version-1 data. Anything but the exact tombstone counts as data.
func legacyNativeDomain(configDir string) (tombstone, data bool, err error) {
	raw, err := vol.OpenDocument(filepath.Join(configDir, legacyNativeDomainFile)).Read()
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) == nil && len(doc) == 1 && doc["migratedTo"] == NativeDomainsFile {
		return true, false, nil
	}
	return false, true, nil
}

// checkNativeFormat is the runtime refusal for unmigrated or half-migrated
// storage: v3 (or a fresh install) only, with the domain tombstone rule.
func checkNativeFormat(configDir string) error {
	format, err := detectAuthority(configDir)
	if err == nil && format == authorityV2 {
		err = ErrNativeMigration
	}
	return err
}

// v2NativeFormat is the v2 runtime's check, kept for the JSON backend
// (offline stores): it returns the ledger bytes it checked, so the caller
// decodes that same read.
func v2NativeFormat(configDir string) (ledger []byte, exists bool, err error) {
	tombstone, data, err := legacyNativeDomain(configDir)
	if err != nil || data {
		return nil, false, ErrNativeMigration
	}
	// A leftover version-1 ledger means migration failed; refuse before any
	// v2 write (Configure included) could adopt it. The relay needs its key
	// to read the version; its runtime reader refuses version 1 itself.
	raw, err := os.ReadFile(filepath.Join(configDir, nativeProvisioningFile))
	if err == nil {
		var head struct {
			Version int `json:"version"`
		}
		if json.Unmarshal(raw, &head) != nil || head.Version != 2 {
			return nil, false, ErrNativeMigration
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, ErrNativeMigration
	}
	exists = err == nil
	if !tombstone {
		return raw, exists, nil
	}
	for _, name := range []string{NativeDomainsFile, nativeProvisioningFile} {
		if _, err := os.Lstat(filepath.Join(configDir, name)); err != nil {
			return nil, false, ErrNativeMigration
		}
	}
	return raw, exists, nil
}

// validNativeDomain is the version-1 claim rule, unchanged. It also rejects
// the tombstone (empty token), which is what makes a v1 binary fail closed.
func validNativeDomain(d NativeDomain) bool {
	token, err := hex.DecodeString(d.Token)
	return err == nil && len(token) == 32 && d.VerifiedUntil >= 0 && (d.Established || d.VerifiedUntil == 0) && nativeDomain(d.Domain) && len(d.RecordName()) <= 253 && directoryIdentifier(d.Issuer) && len(d.Token) == 64 && d.ExpiresAt > 0
}

func parseLegacyNativeDomain(raw []byte) (NativeDomain, error) {
	var d NativeDomain
	if json.Unmarshal(raw, &d) != nil || !validNativeDomain(d) {
		return NativeDomain{}, ErrNativeDomain
	}
	return d, nil
}

// HistoricalNativeDomains reads a snapshot in either format: real version-1
// data (format 1, a set of one), or the domain set (format 2, required with the
// tombstone). Format 0 means no domain. Callers check ledger/relay versions against it.
func HistoricalNativeDomains(configDir string) (NativeDomainSet, int, error) {
	tombstone, data, err := legacyNativeDomain(configDir)
	if err != nil {
		return NativeDomainSet{}, 0, err
	}
	if data {
		raw, err := vol.OpenDocument(filepath.Join(configDir, legacyNativeDomainFile)).Read()
		if err != nil {
			return NativeDomainSet{}, 1, err
		}
		d, err := parseLegacyNativeDomain(raw)
		if err != nil {
			return NativeDomainSet{}, 1, err
		}
		return NativeDomainSet{Issuer: d.Issuer, Founding: d.Domain, Domains: map[string]NativeDomain{d.Domain: d}}, 1, nil
	}
	set, err := NewNativeDomainStore(configDir).readSet()
	empty := len(set.Domains)+len(set.Retired) == 0
	if err == nil && tombstone && empty {
		err = ErrNativeMigration
	}
	if empty && !tombstone {
		return set, 0, err
	}
	return set, 2, err
}

// NativeSnapshotFormatsConsistent is the format-mix rule for snapshots: a v1
// domain may sit beside v1 or v2 files (migration crash windows), a v2 domain
// only beside v2 files or a v3 ledger (native-authority.db), and no domain
// beside none. Versions are 0 when absent.
func NativeSnapshotFormatsConsistent(domainFormat, fileVersion int) bool {
	return fileVersion == 0 || domainFormat == 1 && fileVersion <= 2 || domainFormat == 2 && (fileVersion == 2 || fileVersion == 3)
}

// SetLookupForTest follows the existing test-only transport override contract.
func (s *NativeDomainStore) SetLookupForTest(lookup func(context.Context, string) ([]string, error)) {
	if !testing.Testing() {
		panic("native domain resolver override outside tests")
	}
	s.lookup = lookup
}

// ReadSet is the choke point every native path passes: it refuses unmigrated
// or half-migrated storage before returning the domain set.
func (s *NativeDomainStore) ReadSet() (NativeDomainSet, error) {
	if err := checkNativeFormat(filepath.Dir(s.path)); err != nil {
		return NativeDomainSet{}, err
	}
	return s.readSet()
}

// Read returns the founding domain, which the single-domain API serves.
func (s *NativeDomainStore) Read() (NativeDomain, error) {
	set, err := s.ReadSet()
	if err != nil {
		return NativeDomain{}, err
	}
	return set.Domains[set.Founding], nil
}

func (s *NativeDomainStore) readSet() (NativeDomainSet, error) {
	return s.readSetDocument(vol.OpenDocument(s.path))
}

func (s *NativeDomainStore) readSetDocument(doc *vol.Document) (NativeDomainSet, error) {
	set := NativeDomainSet{Domains: map[string]NativeDomain{}, Retired: []string{}}
	var raw nativeDomainSet
	present := false
	if err := vol.LoadJSON(doc, func(v nativeDomainSet) { raw = v; present = true }, func() error { return nil }); err != nil || !present {
		return set, err
	}
	invalid := NativeDomainSet{Domains: map[string]NativeDomain{}, Retired: []string{}}
	if raw.Version != 1 || len(raw.Domains)+len(raw.Retired) == 0 || !directoryIdentifier(raw.Issuer) {
		return invalid, ErrNativeDomain
	}
	set.Issuer = raw.Issuer
	for domain, p := range raw.Domains {
		d := NativeDomain{Domain: domain, Issuer: raw.Issuer, Token: p.Token, ExpiresAt: p.ExpiresAt, Established: p.Established, VerifiedUntil: p.VerifiedUntil}
		if !validNativeDomain(d) {
			return invalid, ErrNativeDomain
		}
		set.Domains[domain] = d
	}
	for _, domain := range raw.Retired {
		if set.Known(domain) || !nativeDomain(domain) {
			return invalid, ErrNativeDomain
		}
		set.Retired = append(set.Retired, domain)
	}
	set.Founding = raw.Founding
	if set.Founding == "" && len(raw.Domains) == 1 && len(raw.Retired) == 0 {
		for domain := range raw.Domains {
			set.Founding = domain
		}
	}
	if _, ok := set.Domains[set.Founding]; ok != (len(set.Domains) > 0) || !ok && set.Founding != "" {
		return invalid, ErrNativeDomain
	}
	return set, nil
}

// persistSet writes the set, then the tombstone if it is not already there, so
// a v1 binary can never configure a domain behind a v2 one. A retired founding
// domain hands the single-domain API to the smallest remaining domain.
func (s *NativeDomainStore) persistSetDocument(set NativeDomainSet, doc *vol.Document) error {
	raw := nativeDomainSet{Version: 1, Issuer: set.Issuer, Domains: map[string]nativeDomainProof{}, Retired: append([]string{}, set.Retired...)}
	for domain, d := range set.Domains {
		raw.Domains[domain] = nativeDomainProof{d.Token, d.ExpiresAt, d.Established, d.VerifiedUntil}
	}
	if _, ok := set.Domains[set.Founding]; ok {
		raw.Founding = set.Founding
	} else if len(set.Domains) > 0 {
		raw.Founding = slices.Sorted(maps.Keys(set.Domains))[0]
	}
	if err := vol.PersistJSON(doc, raw); err != nil {
		return err
	}
	return writeNativeDomainTombstone(filepath.Dir(s.path))
}

func writeNativeDomainTombstone(configDir string) error {
	if tombstone, _, err := legacyNativeDomain(configDir); err != nil || tombstone {
		return err
	}
	body, _ := json.MarshalIndent(map[string]string{"migratedTo": NativeDomainsFile}, "", "  ")
	return vol.OpenDocument(filepath.Join(configDir, legacyNativeDomainFile)).Replace(body)
}

// Configure is the single-domain API: it claims the founding domain or
// rotates its challenge, and refuses any other domain.
func (s *NativeDomainStore) Configure(ctx context.Context, domain, issuer string) (NativeDomain, error) {
	return s.configure(ctx, domain, issuer, true, false)
}

// ConfigureDomain adds a domain to the set, or rotates the challenge of one
// already configured. Re-adding a retired domain configures it afresh: nothing
// routes, sends or allocates on it until VerifyDomain proves the new challenge.
func (s *NativeDomainStore) ConfigureDomain(ctx context.Context, domain, issuer string) (NativeDomain, error) {
	return s.configure(ctx, domain, issuer, false, false)
}

// EnsureDomain is installer setup: retain an existing challenge, never rotate
// it or reactivate a retired domain. ConfigureDomain remains the rotation API.
func (s *NativeDomainStore) EnsureDomain(ctx context.Context, domain, issuer string) (NativeDomain, error) {
	return s.configure(ctx, domain, issuer, false, true)
}

// ValidateNativeDomainSetup checks the same inputs used by domain publication.
func ValidateNativeDomainSetup(domain, issuer string) error {
	if !nativeDomain(domain) || len("_kypost-mail."+domain) > 253 || !directoryIdentifier(issuer) || strings.HasSuffix(issuer, "/") {
		return ErrNativeDomain
	}
	return nil
}

func (s *NativeDomainStore) configure(ctx context.Context, domain, issuer string, foundingOnly, ensure bool) (NativeDomain, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ValidateNativeDomainSetup(domain, issuer); err != nil {
		return NativeDomain{}, err
	}
	ctx, release, err := vol.Lock(ctx, s.path, vol.FenceNativeDomains, vol.Gated)
	if err != nil {
		return NativeDomain{}, err
	}
	defer release()
	if err := checkNativeFormat(filepath.Dir(s.path)); err != nil {
		return NativeDomain{}, err
	}
	doc := vol.OpenDocument(s.path)
	var result NativeDomain
	err = vol.Retry(func() error {
		var attemptErr error
		result, attemptErr = func() (NativeDomain, error) {
			set, err := s.readSetDocument(doc)
			if err != nil {
				return NativeDomain{}, err
			}
			if set.Issuer != "" && set.Issuer != issuer || foundingOnly && set.Founding != "" && set.Founding != domain {
				return NativeDomain{}, ErrNativeDomain
			}
			if ensure {
				if slices.Contains(set.Retired, domain) {
					return NativeDomain{}, ErrNativeDomain
				}
				if prior, ok := set.Domains[domain]; ok {
					return prior, nil
				}
			}
			var b [32]byte
			if _, err = rand.Read(b[:]); err != nil {
				return NativeDomain{}, err
			}
			d := NativeDomain{Domain: domain, Issuer: issuer, Token: hex.EncodeToString(b[:]), ExpiresAt: time.Now().Add(24 * time.Hour).Unix()}
			if err = ctx.Err(); err != nil {
				return NativeDomain{}, err
			}
			// The ledger exists before the set and tombstone, so native can start at
			// once and a crash never leaves a tombstone without a ledger.
			if err = NewLifecycleStore(filepath.Dir(s.path)).ensureNativeLedger(ctx); err != nil {
				return NativeDomain{}, err
			}
			set.Issuer = issuer
			set.Retired = slices.DeleteFunc(set.Retired, func(r string) bool { return r == domain })
			// Founding changes only when no domain is in service.
			if set.Founding == "" {
				set.Founding = domain
			}
			set.Domains[domain] = d
			return d, s.persistSetDocument(set, doc)

		}()
		return attemptErr
	})
	return result, err
}

// Verify proves the founding domain.
func (s *NativeDomainStore) Verify(ctx context.Context) (NativeDomain, error) {
	d, err := s.Read()
	if err != nil {
		return NativeDomain{}, err
	}
	if d.Domain == "" {
		return d, ErrNativeDomain
	}
	return s.VerifyDomain(ctx, d.Domain)
}

// VerifyDomain refreshes one domain's proof. It fences only on that domain,
// so re-verifying one domain never invalidates proofs of the others.
func (s *NativeDomainStore) VerifyDomain(ctx context.Context, domain string) (NativeDomain, error) {
	ctx, lockCancel := context.WithTimeout(ctx, 30*time.Second)
	defer lockCancel()
	set, err := s.ReadSet()
	if err != nil {
		return NativeDomain{}, err
	}
	d, ok := set.Domains[domain]
	if !ok || !d.Established && d.ExpiresAt <= time.Now().Unix() {
		return d, ErrNativeDomain
	}
	dnsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	values, lookupErr := s.lookup(dnsCtx, d.RecordName()+".")
	match := false
	if lookupErr == nil {
		for _, v := range values {
			if v == d.RecordValue() {
				match = true
				break
			}
		}
	}
	// Fresh evidence agreeing with a proof saved within the last minute needs
	// no write; every fence still compares it under the lock (CurrentProof).
	if match && d.Established && d.VerifiedUntil-time.Now().Unix() >= int64(nativeProofReuse.Seconds()) {
		return d, nil
	}
	ctx, release, err := vol.Lock(ctx, s.path, vol.FenceNativeDomains, vol.Gated)
	if err != nil {
		return d, err
	}
	defer release()
	if err := checkNativeFormat(filepath.Dir(s.path)); err != nil {
		return NativeDomain{}, err
	}
	doc := vol.OpenDocument(s.path)
	var result NativeDomain
	err = vol.Retry(func() error {
		var attemptErr error
		result, attemptErr = func() (NativeDomain, error) {
			current, err := s.readSetDocument(doc)
			if err != nil {
				return d, err
			}
			// Only the challenge must be unchanged: a concurrent refresh of the same
			// challenge (Established, VerifiedUntil) is not a change, and refusing it
			// failed parallel deliveries and sends with a spurious domain error.
			cur := current.Domains[domain]
			if cur.Domain != d.Domain || cur.Issuer != d.Issuer || cur.Token != d.Token || cur.ExpiresAt != d.ExpiresAt ||
				!cur.Established && cur.ExpiresAt <= time.Now().Unix() {
				return cur, ErrNativeDomain
			}
			d = cur
			d.VerifiedUntil = 0
			if match {
				d.VerifiedUntil = time.Now().Add(5 * time.Minute).Unix()
				if !d.Established {
					d.VerifiedUntil = min(d.ExpiresAt, d.VerifiedUntil)
				}
				d.Established = true
			}
			if err = ctx.Err(); err != nil {
				return d, err
			}
			current.Domains[domain] = d
			if err = s.persistSetDocument(current, doc); err != nil {
				return d, err
			}
			if lookupErr != nil {
				return d, lookupErr
			}
			if !match {
				return d, ErrNativeDomain
			}
			return d, nil

		}()
		return attemptErr
	})
	return result, err
}

// CurrentProof reports whether proof's challenge is still the domain's stored,
// proven challenge and both are unexpired. A concurrent refresh of the same
// challenge only moves VerifiedUntil, so it is not a change (as in
// VerifyDomain). Callers hold the domain fence; other domains never affect it.
func (s NativeDomainSet) CurrentProof(proof NativeDomain) bool {
	cur, now := s.Domains[proof.Domain], time.Now().Unix()
	return proof.Domain != "" && cur.Domain == proof.Domain && cur.Issuer == proof.Issuer && cur.Token == proof.Token &&
		cur.ExpiresAt == proof.ExpiresAt && cur.Established && cur.VerifiedUntil > now && proof.VerifiedUntil > now
}

// RetireDomain moves a configured domain to retired until re-added: no proof,
// routing or sending, while its address records stay so generations are never
// reused. Lock order: domain -> directory -> mailbox/ingress SQLite. Holding the
// domain fence keeps new outbox jobs, receiving binds and relay writes out
// while they are checked.
func (s *NativeDomainStore) RetireDomain(ctx context.Context, domain, stateRoot, relayKeyPath string) error {
	if err := RequireNativeRestoreReleased(stateRoot); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx, release, err := vol.Lock(ctx, s.path, vol.FenceNativeDomains, vol.Gated)
	if err != nil {
		return err
	}
	defer release()
	if err := checkNativeFormat(filepath.Dir(s.path)); err != nil {
		return err
	}
	doc := vol.OpenDocument(s.path)
	return vol.Retry(func() error {
		set, err := s.readSetDocument(doc)
		if err != nil {
			return err
		}
		if _, ok := set.Domains[domain]; !ok {
			return ErrNativeDomain
		}
		relay, _, err := mailmsg.ReadDomainRelay(filepath.Join(filepath.Dir(s.path), "native-relay.json"), relayKeyPath)
		if err != nil {
			return err
		}
		if relay.Sends(domain) {
			return ErrNativeDomainInUse
		}
		life := NewLifecycleStore(filepath.Dir(s.path))
		ctx, releaseDirectory, err := life.LockDirectoryContext(ctx)
		if err != nil {
			return err
		}
		defer releaseDirectory()
		f, err := life.loadNative()
		if err != nil {
			return err
		}
		for address, x := range f.stored.Addresses {
			if x.State == "active" && AddressDomain(address) == domain {
				return ErrNativeDomainInUse
			}
		}
		queued, err := nativeQueuedFromDomains(ctx, f, stateRoot, relayKeyPath)
		if err != nil {
			return err
		}
		if queued[domain] {
			return ErrNativeDomainInUse
		}
		held, err := nativeHeldRecipients(ctx, stateRoot)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(held, func(address string) bool { return AddressDomain(address) == domain }) {
			return ErrNativeDomainInUse
		}
		delete(set.Domains, domain)
		set.Retired = append(set.Retired, domain)
		return s.persistSetDocument(set, doc)

	})
}

// nativeHeldRecipients lists recipients bound to staged or pending incoming
// mail. Binds take the domain fence, which the caller holds.
func nativeHeldRecipients(ctx context.Context, stateRoot string) ([]string, error) {
	path, err := filepath.Abs(filepath.Join(stateRoot, "receiving", "ingress.db"))
	if err != nil {
		return nil, err
	}
	db, err := dbbackend.OpenFile(path, "mode=ro")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT b.address FROM bindings b JOIN deliveries d ON d.gateway=b.gateway AND d.id=b.id WHERE d.state='pending'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	held := []string{}
	for rows.Next() {
		var address string
		if err := rows.Scan(&address); err != nil {
			return nil, err
		}
		held = append(held, address)
	}
	return held, rows.Err()
}

// NativeQueuedFromDomains lists the From domains of outbox jobs with a queued
// or retryable delivery. Submitting and uncertain attempts are never reclaimed
// and do not count. Callers hold the domain fence, which every queue takes.
func NativeQueuedFromDomains(ctx context.Context, configDir, stateRoot, relayKeyPath string) (map[string]bool, error) {
	f, err := NewLifecycleStore(configDir).loadNative()
	if err != nil {
		return nil, err
	}
	return nativeQueuedFromDomains(ctx, f, stateRoot, relayKeyPath)
}

// ponytail: opens every native mailbox per admin action; index From domains
// in the outbox if domain changes ever need to be cheap.
func nativeQueuedFromDomains(ctx context.Context, f nativeAssignments, stateRoot, relayKeyPath string) (map[string]bool, error) {
	// No key means nothing was ever queued; a queued row then fails to open.
	key, err := cryptutil.LoadKey(relayKeyPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	domains := map[string]bool{}
	for _, a := range f.mailboxes() {
		if a.Source == "" {
			continue
		}
		box, err := mailbox.OpenExisting(filepath.Join(a.Dir(stateRoot), "mailbox"), a.Owner, a.Limits, a.Source)
		if err != nil {
			return nil, err
		}
		from, err := box.QueuedOutboundFrom(ctx, key)
		_ = box.Close()
		if err != nil {
			return nil, err
		}
		for _, address := range from {
			domains[AddressDomain(address)] = true
		}
	}
	return domains, nil
}
