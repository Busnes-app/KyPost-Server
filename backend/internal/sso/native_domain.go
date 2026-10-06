package sso

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// NativeDomainsFile is the domain set; its sibling ".lock" is the domain fence.
// The version-1 single-domain file native-domain.json becomes a tombstone.
const NativeDomainsFile = "native-domains.json"

const legacyNativeDomainFile = "native-domain.json"

var ErrNativeDomain = errors.New("mail domain proof missing, expired or changed; configure and verify the current DNS challenge")

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

// nativeDomainSet is native-domains.json version 1. Phase 1 holds exactly one
// domain and no retired ones; the single-domain API below reads and writes it.
type nativeDomainSet struct {
	Version int                          `json:"version"`
	Issuer  string                       `json:"issuer"`
	Domains map[string]nativeDomainProof `json:"domains"`
	Retired []string                     `json:"retired"`
}

type nativeDomainProof struct {
	Token         string `json:"token"`
	ExpiresAt     int64  `json:"expiresAt"`
	Established   bool   `json:"established"`
	VerifiedUntil int64  `json:"verifiedUntil"`
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
	raw, err := os.ReadFile(filepath.Join(configDir, legacyNativeDomainFile))
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
// storage. A fresh install (neither file) passes as "not configured".
func checkNativeFormat(configDir string) error {
	tombstone, data, err := legacyNativeDomain(configDir)
	if err != nil || data {
		return ErrNativeMigration
	}
	if !tombstone {
		return nil
	}
	for _, name := range []string{NativeDomainsFile, nativeProvisioningFile} {
		if _, err := os.Lstat(filepath.Join(configDir, name)); err != nil {
			return ErrNativeMigration
		}
	}
	return nil
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

// HistoricalNativeDomain reads a snapshot in either format: real version-1
// data, or the domain set (required when the tombstone is present).
func HistoricalNativeDomain(configDir string) (NativeDomain, error) {
	tombstone, data, err := legacyNativeDomain(configDir)
	if err != nil {
		return NativeDomain{}, err
	}
	if data {
		raw, err := os.ReadFile(filepath.Join(configDir, legacyNativeDomainFile))
		if err != nil {
			return NativeDomain{}, err
		}
		return parseLegacyNativeDomain(raw)
	}
	d, err := NewNativeDomainStore(configDir).read()
	if err == nil && tombstone && d.Domain == "" {
		err = ErrNativeMigration
	}
	return d, err
}

// SetLookupForTest follows the existing test-only transport override contract.
func (s *NativeDomainStore) SetLookupForTest(lookup func(context.Context, string) ([]string, error)) {
	if !testing.Testing() {
		panic("native domain resolver override outside tests")
	}
	s.lookup = lookup
}

// Read is the choke point every native path passes: it refuses unmigrated or
// half-migrated storage before returning the single configured domain.
func (s *NativeDomainStore) Read() (NativeDomain, error) {
	if err := checkNativeFormat(filepath.Dir(s.path)); err != nil {
		return NativeDomain{}, err
	}
	return s.read()
}

func (s *NativeDomainStore) read() (NativeDomain, error) {
	var set nativeDomainSet
	present := false
	if err := fsutil.LoadJSONFile(s.path, func(v nativeDomainSet) { set = v; present = true }, nil); err != nil || !present {
		return NativeDomain{}, err
	}
	if set.Version != 1 || len(set.Domains) != 1 || len(set.Retired) != 0 {
		return NativeDomain{}, ErrNativeDomain
	}
	var d NativeDomain
	for domain, p := range set.Domains {
		d = NativeDomain{Domain: domain, Issuer: set.Issuer, Token: p.Token, ExpiresAt: p.ExpiresAt, Established: p.Established, VerifiedUntil: p.VerifiedUntil}
	}
	if !validNativeDomain(d) {
		return d, ErrNativeDomain
	}
	return d, nil
}

// persist writes the set, then the tombstone if it is not already there, so a
// v1 binary can never configure a domain behind a v2 one.
func (s *NativeDomainStore) persist(d NativeDomain) error {
	set := nativeDomainSet{Version: 1, Issuer: d.Issuer, Domains: map[string]nativeDomainProof{d.Domain: {d.Token, d.ExpiresAt, d.Established, d.VerifiedUntil}}, Retired: []string{}}
	if err := fsutil.PersistJSONFile(s.path, set); err != nil {
		return err
	}
	return writeNativeDomainTombstone(filepath.Dir(s.path))
}

func writeNativeDomainTombstone(configDir string) error {
	if tombstone, _, err := legacyNativeDomain(configDir); err != nil || tombstone {
		return err
	}
	return fsutil.PersistJSONFile(filepath.Join(configDir, legacyNativeDomainFile), map[string]string{"migratedTo": NativeDomainsFile})
}
func (s *NativeDomainStore) Configure(ctx context.Context, domain, issuer string) (NativeDomain, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !nativeDomain(domain) || len("_kypost-mail."+domain) > 253 || !directoryIdentifier(issuer) || strings.HasSuffix(issuer, "/") {
		return NativeDomain{}, ErrNativeDomain
	}
	release, err := fsutil.LockFileContext(ctx, s.path)
	if err != nil {
		return NativeDomain{}, err
	}
	defer release()
	prior, err := s.Read()
	if err != nil {
		return NativeDomain{}, err
	}
	if prior.Domain != "" && (prior.Domain != domain || prior.Issuer != issuer) {
		return NativeDomain{}, ErrNativeDomain
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
	err = s.persist(d)
	return d, err
}
func (s *NativeDomainStore) Verify(ctx context.Context) (NativeDomain, error) {
	ctx, lockCancel := context.WithTimeout(ctx, 30*time.Second)
	defer lockCancel()
	d, err := s.Read()
	if err != nil {
		return NativeDomain{}, err
	}
	if d.Domain == "" || !d.Established && d.ExpiresAt <= time.Now().Unix() {
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
	release, err := fsutil.LockFileContext(ctx, s.path)
	if err != nil {
		return d, err
	}
	defer release()
	current, err := s.Read()
	if err != nil {
		return d, err
	}
	if current != d || !d.Established && d.ExpiresAt <= time.Now().Unix() {
		return current, ErrNativeDomain
	}
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
	if err = s.persist(d); err != nil {
		return d, err
	}
	if lookupErr != nil {
		return d, lookupErr
	}
	if !match {
		return d, ErrNativeDomain
	}
	return d, nil
}
