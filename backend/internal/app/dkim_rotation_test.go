//go:build linux

package app

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/api"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/emersion/go-msgauth/dkim"
)

type txtDNS struct {
	txt   map[string][]string
	calls atomic.Int32
}

func (d *txtDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	d.calls.Add(1)
	if v, ok := d.txt[name]; ok {
		return v, nil
	}
	return nil, &net.DNSError{IsNotFound: true}
}
func (d *txtDNS) LookupIP(context.Context, string, string) ([]net.IP, error) {
	d.calls.Add(1)
	return nil, &net.DNSError{IsNotFound: true}
}
func (d *txtDNS) LookupAddr(context.Context, string) ([]string, error) {
	d.calls.Add(1)
	return nil, &net.DNSError{IsNotFound: true}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// rotatedProfile saves a direct profile and completes one rotation at now,
// leaving the old key retiring for 7 days.
func rotatedProfile(t *testing.T, now time.Time) (mailmsg.DKIMRotation, *txtDNS) {
	t.Helper()
	t.Setenv("CONFIG_DIR", t.TempDir())
	t.Setenv("SECRET_DIR", t.TempDir())
	ctx := context.Background()
	dns := &txtDNS{txt: map[string][]string{}}
	r := api.DKIMRotationFor(config.ConfigDir(), dns)
	if _, err := mailmsg.SaveDomainRelay(ctx, r.Path, r.KeyPath, mailmsg.DomainRelay{Mode: "direct", Domains: []string{"example.test"}, Issuer: "https://idp.example", Host: "mail.example.test"}); err != nil {
		t.Fatal(err)
	}
	c, _, _, err := r.Rotate(ctx, now, "example.test")
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range c.DKIMRecords(now) {
		dns.txt[rec.RecordName+"."] = []string{rec.RecordValue}
	}
	if _, _, _, err := r.Confirm(ctx, now, "example.test"); err != nil {
		t.Fatal(err)
	}
	return r, dns
}

func dkimStates(t *testing.T, r mailmsg.DKIMRotation) string {
	t.Helper()
	c, _, err := mailmsg.ReadDomainRelay(r.Path, r.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	var states []string
	for _, k := range c.DKIM {
		states = append(states, k.State)
	}
	return strings.Join(states, ",")
}

func waitStates(t *testing.T, r mailmsg.DKIMRotation, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for dkimStates(t, r) != want {
		if time.Now().After(deadline) {
			t.Fatal("states", dkimStates(t, r), "want", want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDKIMRotationTickRunsAtStartAndOnInterval(t *testing.T) {
	start := time.Now()
	r, dns := rotatedProfile(t, start)
	interval, now, resolver := dkimRotationInterval, dkimNow, dkimResolver
	t.Cleanup(func() { dkimRotationInterval, dkimNow, dkimResolver = interval, now, resolver })
	var clock atomic.Int64
	clock.Store(start.Add(8 * 24 * time.Hour).Unix())
	dkimNow = func() time.Time { return time.Unix(clock.Load(), 0) }
	dkimRotationInterval, dkimResolver = 20*time.Millisecond, dns
	var out syncBuffer
	logger, err := logging.NewWithOutput(&out)
	if err != nil {
		t.Fatal(err)
	}
	calls := dns.calls.Load()
	ctx, cancel := context.WithCancel(context.Background())
	done := startDKIMRotation(ctx, runDeps{nativeMail: true, configDir: config.ConfigDir(), logger: logger})
	defer func() { cancel(); <-done }()
	waitStates(t, r, "active,removable") // the start tick destroys the expired key
	clock.Store(start.Add(40 * 24 * time.Hour).Unix())
	waitStates(t, r, "active") // a later interval tick drops the entry
	before, _ := os.ReadFile(r.Path)
	time.Sleep(100 * time.Millisecond) // several more ticks
	if after, _ := os.ReadFile(r.Path); !bytes.Equal(before, after) {
		t.Fatal("idle ticks rewrote the profile")
	}
	if dns.calls.Load() != calls {
		t.Fatal("tick looked up DNS")
	}
	for _, want := range []string{`"message":"dkim rotation"`, `"phase":"destroy"`, `"phase":"drop"`, `"trigger":"window"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatal("log lacks", want, out.String())
		}
	}
}

func TestDKIMRotationTickOffOrRelay(t *testing.T) {
	start := time.Now()
	r, _ := rotatedProfile(t, start)
	now := dkimNow
	t.Cleanup(func() { dkimNow = now })
	dkimNow = func() time.Time { return start.Add(400 * 24 * time.Hour) }
	before, _ := os.ReadFile(r.Path)
	done := startDKIMRotation(context.Background(), runDeps{nativeMail: false, configDir: config.ConfigDir(), logger: newTestLogger(t)})
	<-done
	if after, _ := os.ReadFile(r.Path); !bytes.Equal(before, after) {
		t.Fatal("tick ran with native mail off")
	}
	relay, err := mailmsg.SaveDomainRelay(context.Background(), r.Path, r.KeyPath, mailmsg.DomainRelay{Domains: []string{"example.test"}, Issuer: "https://idp.example", Host: "smtp.example.test", Port: 465, Username: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ = os.ReadFile(r.Path)
	ctx, cancel := context.WithCancel(context.Background())
	done = startDKIMRotation(ctx, runDeps{nativeMail: true, configDir: config.ConfigDir(), logger: newTestLogger(t)})
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if after, _ := os.ReadFile(r.Path); !bytes.Equal(before, after) || len(relay.DKIM) != 2 {
		t.Fatal("relay tick touched keys")
	}
}

// A fresh process over the same directories signs with no admin input: the
// key is sealed with native-relay.key, not an admin password.
func TestDKIMKeyLockSignsAfterRestart(t *testing.T) {
	rotatedProfile(t, time.Now())
	if err := Run([]string{"migrate-native"}); err != nil {
		t.Fatal(err)
	}
	c, exists, err := mailmsg.ReadDomainRelay(filepath.Join(config.ConfigDir(), "native-relay.json"), filepath.Join(config.SecretDir(), "native-relay.key"))
	if err != nil || !exists {
		t.Fatal(err)
	}
	var wire []byte
	mailmsg.SetDirectTransportForTest(func(_ string, _ []string, w []byte) error { wire = w; return nil })
	t.Cleanup(func() { mailmsg.SetDirectTransportForTest(nil) })
	if err := c.Deliver("one@example.test", []string{"r@example.org"}, []byte("From: one@example.test\r\nTo: r@example.org\r\nSubject: restart\r\n\r\nbody\r\n")); err != nil {
		t.Fatal(err)
	}
	records := map[string]string{}
	active := ""
	for _, rec := range c.DKIMRecords(time.Now()) {
		records[rec.RecordName] = rec.RecordValue
		if rec.State == mailmsg.DKIMActive {
			active = rec.Selector
		}
	}
	result, err := dkim.VerifyWithOptions(bytes.NewReader(wire), &dkim.VerifyOptions{LookupTXT: func(name string) ([]string, error) { return []string{records[name]}, nil }})
	if err != nil || len(result) != 1 || result[0].Err != nil || !strings.Contains(strings.ReplaceAll(string(wire), "\r\n ", ""), "s="+active+";") {
		t.Fatal("restart lost the signing key", err, result)
	}
}
