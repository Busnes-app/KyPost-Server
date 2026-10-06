package sso

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func provenNativeDomain(t *testing.T, config string) *NativeDomainStore {
	t.Helper()
	s := NewNativeDomainStore(config)
	if _, err := s.Configure(context.Background(), "example.test", nativeIssuer); err != nil {
		t.Fatal(err)
	}
	s.SetLookupForTest(func(_ context.Context, name string) ([]string, error) {
		d, e := s.Read()
		if name != d.RecordName()+"." {
			t.Errorf("wrong purpose %q", name)
		}
		return []string{d.RecordValue()}, e
	})
	return s
}

func TestNativeDomainProofAndFailure(t *testing.T) {
	ctx := context.Background()
	s := provenNativeDomain(t, t.TempDir())
	d, err := s.Verify(ctx)
	if err != nil || d.VerifiedUntil <= time.Now().Unix() {
		t.Fatal(d, err)
	}
	for _, change := range []struct{ domain, issuer string }{{"other.test", nativeIssuer}, {"example.test", "https://other.test"}, {"Example.test", nativeIssuer}, {"example.test", nativeIssuer + "/"}} {
		if _, err := s.Configure(ctx, change.domain, change.issuer); !errors.Is(err, ErrNativeDomain) {
			t.Fatal("changed binding", err)
		}
	}
	s.SetLookupForTest(func(context.Context, string) ([]string, error) {
		return []string{"kypost-wkd-verify=" + d.Token, " " + d.RecordValue()}, nil
	})
	if _, err := s.Verify(ctx); !errors.Is(err, ErrNativeDomain) {
		t.Fatal("wrong purpose/whitespace accepted", err)
	}
	if got, _ := s.Read(); got.VerifiedUntil != 0 {
		t.Fatal("stale positive retained")
	}
	dnsErr := errors.New("DNS unavailable")
	s.SetLookupForTest(func(context.Context, string) ([]string, error) { return nil, dnsErr })
	if _, err := s.Verify(ctx); !errors.Is(err, dnsErr) {
		t.Fatal("DNS failure hidden", err)
	}
	d.Established = false
	d.ExpiresAt = time.Now().Unix() - 1
	d.VerifiedUntil = 0
	if err := s.persist(d); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(ctx); !errors.Is(err, ErrNativeDomain) {
		t.Fatal("expired accepted", err)
	}
	if err := os.WriteFile(s.path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(); !errors.Is(err, ErrNativeDomain) {
		t.Fatal("empty restored claim accepted", err)
	}
}

func TestNativeDomainRotationDuringLookup(t *testing.T) {
	s := provenNativeDomain(t, t.TempDir())
	old, _ := s.Read()
	s.SetLookupForTest(func(ctx context.Context, _ string) ([]string, error) {
		_, err := s.Configure(ctx, old.Domain, old.Issuer)
		return []string{old.RecordValue()}, err
	})
	if _, err := s.Verify(context.Background()); !errors.Is(err, ErrNativeDomain) {
		t.Fatal("stale lookup authorized rotated challenge", err)
	}
	next, _ := s.Read()
	if next.Token == old.Token || next.VerifiedUntil != 0 || next.Established {
		t.Fatal("rotation lost", next)
	}
	s.SetLookupForTest(func(ctx context.Context, _ string) ([]string, error) { <-ctx.Done(); return nil, ctx.Err() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := s.Verify(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("DNS not cancelled", err)
	}
}

func TestNativeDomainEstablishedProfileRechecksWithoutDailyDNSChanges(t *testing.T) {
	s := provenNativeDomain(t, t.TempDir())
	d, err := s.Verify(context.Background())
	if err != nil || !d.Established {
		t.Fatal(d, err)
	}
	token := d.Token
	d.ExpiresAt = time.Now().Add(-time.Hour).Unix()
	d.VerifiedUntil = 0
	if err = s.persist(d); err != nil {
		t.Fatal(err)
	}
	d, err = s.Verify(context.Background())
	if err != nil || d.Token != token || d.VerifiedUntil <= time.Now().Unix() {
		t.Fatal("established domain needs daily TXT change", d, err)
	}
	s.SetLookupForTest(func(context.Context, string) ([]string, error) { return nil, nil })
	d, err = s.Verify(context.Background())
	if !errors.Is(err, ErrNativeDomain) || d.VerifiedUntil != 0 {
		t.Fatal("established marker substituted for DNS", d, err)
	}
	rotated, err := s.Configure(context.Background(), d.Domain, d.Issuer)
	if err != nil || rotated.Established || rotated.Token == token {
		t.Fatal("rotation kept authority", rotated, err)
	}
}

func TestNativeDomainRefusesOversizedChallengeName(t *testing.T) {
	prefix := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "."
	s := NewNativeDomainStore(t.TempDir())
	oversized := prefix + strings.Repeat("d", 50)
	if _, err := s.Configure(context.Background(), oversized, nativeIssuer); !errors.Is(err, ErrNativeDomain) {
		t.Fatal("oversized first claim accepted", err)
	}
	d, err := s.Configure(context.Background(), prefix+strings.Repeat("d", 48), nativeIssuer)
	if err != nil || len(d.RecordName()) != 253 {
		t.Fatal("valid boundary refused", d, err)
	}
	d.Domain = oversized
	if err = s.persist(d); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Read(); !errors.Is(err, ErrNativeDomain) {
		t.Fatal("oversized restored profile accepted", err)
	}
}
