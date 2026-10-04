package sso

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

var ErrNativeDomain = errors.New("mail domain proof missing, expired or changed; configure and verify the current DNS challenge")

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

type NativeDomainStore struct {
	path   string
	lookup func(context.Context, string) ([]string, error)
}

func NewNativeDomainStore(configDir string) *NativeDomainStore {
	return &NativeDomainStore{path: filepath.Join(configDir, "native-domain.json"), lookup: net.DefaultResolver.LookupTXT}
}

// SetLookupForTest follows the existing test-only transport override contract.
func (s *NativeDomainStore) SetLookupForTest(lookup func(context.Context, string) ([]string, error)) {
	if !testing.Testing() {
		panic("native domain resolver override outside tests")
	}
	s.lookup = lookup
}
func (s *NativeDomainStore) Read() (NativeDomain, error) {
	var d NativeDomain
	present := false
	err := fsutil.LoadJSONFile(s.path, func(v NativeDomain) { d = v; present = true }, nil)
	token, tokenErr := hex.DecodeString(d.Token)
	if err == nil && present && (tokenErr != nil || len(token) != 32 || d.VerifiedUntil < 0 || !d.Established && d.VerifiedUntil > 0 || !nativeDomain(d.Domain) || !directoryIdentifier(d.Issuer) || len(d.Token) != 64 || d.ExpiresAt <= 0) {
		err = ErrNativeDomain
	}
	return d, err
}
func (s *NativeDomainStore) Configure(ctx context.Context, domain, issuer string) (NativeDomain, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !nativeDomain(domain) || !directoryIdentifier(issuer) || strings.HasSuffix(issuer, "/") {
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
	err = fsutil.PersistJSONFile(s.path, d)
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
	values, lookupErr := s.lookup(dnsCtx, d.RecordName())
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
	if err = fsutil.PersistJSONFile(s.path, d); err != nil {
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
