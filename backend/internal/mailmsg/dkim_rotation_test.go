package mailmsg

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
)

type rotationFixture struct {
	r   DKIMRotation
	dns *fakeDNS
	gen string
}

// newRotation saves a direct profile for example.test; its first key is active.
func newRotation(t *testing.T) rotationFixture {
	t.Helper()
	dir := t.TempDir()
	f := rotationFixture{dns: &fakeDNS{txt: map[string][]string{}, errs: map[string]error{}, ptr: map[string][]string{}}}
	f.r = DKIMRotation{Path: filepath.Join(dir, "native-relay.json"), KeyPath: filepath.Join(dir, "native-relay.key"), Fence: filepath.Join(dir, "native-domains.json"), Resolver: f.dns}
	profile := directTestProfile()
	profile.DKIM = nil
	c, err := SaveDomainRelay(context.Background(), f.r.Path, f.r.KeyPath, profile)
	if err != nil {
		t.Fatal(err)
	}
	f.gen = c.Generation
	return f
}

func (f rotationFixture) load(t *testing.T) DomainRelay {
	t.Helper()
	c, _, err := ReadDomainRelay(f.r.Path, f.r.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.Generation != f.gen {
		t.Fatal("rotation changed the relay generation")
	}
	return c
}

func (f rotationFixture) bytes(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(f.r.Path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (f rotationFixture) key(t *testing.T, state string) DKIMKey {
	t.Helper()
	k, ok := f.load(t).dkimKey("example.test", state)
	if !ok {
		t.Fatal("no key in state", state)
	}
	return k
}

func (f rotationFixture) publish(t *testing.T, k DKIMKey) {
	t.Helper()
	record, err := k.publicRecord()
	if err != nil {
		t.Fatal(err)
	}
	f.dns.txt[k.recordName()+"."] = []string{record}
}

// unchanged runs op and requires it to leave the sealed bytes alone.
func (f rotationFixture) unchanged(t *testing.T, op func()) {
	t.Helper()
	before := f.bytes(t)
	op()
	if !bytes.Equal(before, f.bytes(t)) {
		t.Fatal("operation rewrote the profile")
	}
}

// confirm switches to the published next key through the admin path.
func (f rotationFixture) confirm(t *testing.T, now time.Time) []DKIMEvent {
	t.Helper()
	_, _, events, err := f.r.Confirm(context.Background(), now, "example.test")
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// tick runs the background step and requires it to make no DNS lookup.
func (f rotationFixture) tick(t *testing.T, now time.Time) []DKIMEvent {
	t.Helper()
	calls := f.dns.calls
	_, events, err := f.r.Tick(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if f.dns.calls != calls {
		t.Fatal("tick looked up DNS")
	}
	return events
}

// signingSelector delivers through the test transport, verifies the wire
// signature against the profile's records and returns its s= selector.
func signingSelector(t *testing.T, c DomainRelay) string {
	t.Helper()
	var wire []byte
	directTestTransport = func(_ string, _ []string, w []byte) error { wire = w; return nil }
	defer func() { directTestTransport = nil }()
	if err := c.Deliver("mailbox@example.test", []string{"visible@example.org"}, []byte(relayProofMIME)); err != nil {
		t.Fatal(err)
	}
	if verifyWire(t, c, string(wire)) != relayProofMIME {
		t.Fatal("signing changed the message")
	}
	tags, _ := signatureField(t, string(wire))
	return tags["s"]
}

func TestDKIMRotationPrepareKeepsActiveKey(t *testing.T) {
	f := newRotation(t)
	ctx := context.Background()
	active := f.key(t, DKIMActive)
	_, next, events, err := f.r.Rotate(ctx, time.Now(), "example.test")
	if err != nil || next.State != DKIMNext || next.Selector == active.Selector || !selectorPattern.MatchString(next.Selector) || len(events) != 1 || events[0].Phase != "prepare" || events[0].Trigger != "manual" {
		t.Fatal("prepare", err, next.Selector, events)
	}
	if key, err := next.signer(); err != nil || key.N.BitLen() != 2048 {
		t.Fatal("next key is not RSA-2048", err)
	}
	if !f.key(t, DKIMActive).equal(active) {
		t.Fatal("prepare changed the active key")
	}
	f.unchanged(t, func() {
		_, again, events, err := f.r.Rotate(ctx, time.Now(), "example.test")
		if err != nil || again.Selector != next.Selector || len(events) != 0 {
			t.Fatal("second prepare made another key", err)
		}
	})
	if _, _, _, err := f.r.Rotate(ctx, time.Now(), "other.test"); !errors.Is(err, ErrDKIMUnknown) {
		t.Fatal("unknown domain rotated", err)
	}
	relay := DomainRelay{Domains: []string{"example.test"}, Issuer: directTestProfile().Issuer, Host: "smtp.example.test", Port: 465, Username: "u", Password: "p"}
	if _, err := SaveDomainRelay(ctx, f.r.Path, f.r.KeyPath, relay); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := f.r.Rotate(ctx, time.Now(), "example.test"); !errors.Is(err, ErrDKIMNotDirect) {
		t.Fatal("relay profile rotated", err)
	}
}

func TestDKIMRotationWaitsForLiveDNS(t *testing.T) {
	ctx := context.Background()
	other, _ := testDKIMKey("example.test").publicRecord()
	for name, serve := range map[string]func(f rotationFixture, next DKIMKey){
		"missing": func(rotationFixture, DKIMKey) {},
		"other-p": func(f rotationFixture, next DKIMKey) { f.dns.txt[next.recordName()+"."] = []string{other} },
		"servfail": func(f rotationFixture, next DKIMKey) {
			f.dns.errs[next.recordName()+"."] = &net.DNSError{IsTemporary: true}
		},
		"timeout": func(f rotationFixture, next DKIMKey) {
			f.dns.errs[next.recordName()+"."] = &net.DNSError{IsTimeout: true}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRotation(t)
			active := f.key(t, DKIMActive)
			_, next, _, err := f.r.Rotate(ctx, time.Now(), "example.test")
			if err != nil {
				t.Fatal(err)
			}
			serve(f, next)
			f.unchanged(t, func() { f.tick(t, time.Now()) })
			_, check, _, err := f.r.Confirm(ctx, time.Now(), "example.test")
			if !errors.Is(err, ErrDKIMNotLive) || check.Status == "pass" || check.Message == "" {
				t.Fatal("unconfirmed next accepted", err, check)
			}
			if s := signingSelector(t, f.load(t)); s != active.Selector {
				t.Fatal("unconfirmed key signed", s)
			}
		})
	}
}

func TestDKIMRotationConfirmedSwitch(t *testing.T) {
	ctx := context.Background()
	f := newRotation(t)
	old := f.key(t, DKIMActive)
	_, next, _, err := f.r.Rotate(ctx, time.Now(), "example.test")
	if err != nil {
		t.Fatal(err)
	}
	f.publish(t, next)
	// The record is live, but only the administrator's confirm switches.
	f.unchanged(t, func() { f.tick(t, time.Now()) })
	now := time.Now().Add(time.Hour)
	events := f.confirm(t, now)
	if len(events) != 1 || events[0].Phase != "switch" || events[0].Trigger != "manual" || events[0].OldSelector != old.Selector || events[0].NewSelector != next.Selector {
		t.Fatal("switch event", events)
	}
	active, retiring := f.key(t, DKIMActive), f.key(t, DKIMRetiring)
	if active.Selector != next.Selector || !bytes.Equal(active.Private, next.Private) || !active.ActivatedAt.Equal(at(now)) {
		t.Fatal("next not active", active)
	}
	if retiring.Selector != old.Selector || !retiring.RetiredAt.Equal(at(now)) || !retiring.KeepPublishedUntil.Equal(at(now.Add(7*24*time.Hour))) || !bytes.Equal(retiring.Private, old.Private) {
		t.Fatal("old key not retiring for 7 days", retiring)
	}
	if s := signingSelector(t, f.load(t)); s != next.Selector {
		t.Fatal("switch did not move signing", s)
	}
	f.unchanged(t, func() {
		f.tick(t, now)
		if _, _, events, err := f.r.Confirm(ctx, now, "example.test"); !errors.Is(err, ErrDKIMNoNext) || len(events) != 0 {
			t.Fatal("confirming again", err)
		}
	})
}

// decrypted returns the plaintext JSON of the sealed profile.
func (f rotationFixture) decrypted(t *testing.T) []byte {
	t.Helper()
	key, err := cryptutil.LoadKey(f.r.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	envelope, ok := cryptutil.ParseEnvelope(f.bytes(t))
	if !ok {
		t.Fatal("not an envelope")
	}
	plain, err := cryptutil.Open(envelope, key)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

func TestDKIMRotationRetireWindowAndDestruction(t *testing.T) {
	ctx := context.Background()
	f := newRotation(t)
	old := f.key(t, DKIMActive)
	_, next, _, err := f.r.Rotate(ctx, time.Now(), "example.test")
	if err != nil {
		t.Fatal(err)
	}
	f.publish(t, next)
	f.confirm(t, time.Now())
	until := f.key(t, DKIMRetiring).KeepPublishedUntil
	f.unchanged(t, func() { f.tick(t, until.Add(-time.Second)) })
	if k := f.key(t, DKIMRetiring); !bytes.Equal(k.Private, old.Private) {
		t.Fatal("retiring key lost early")
	}
	if s := signingSelector(t, f.load(t)); s != next.Selector {
		t.Fatal("retiring key signed", s)
	}
	if _, _, _, err := f.r.Rotate(ctx, until.Add(-time.Second), "example.test"); !errors.Is(err, ErrDKIMRetiring) {
		t.Fatal("rotated while the old key retires", err)
	}
	events := f.tick(t, until)
	removable := f.key(t, DKIMRemovable)
	if len(events) != 1 || events[0].Phase != "destroy" || events[0].Trigger != "window" || removable.Selector != old.Selector || len(removable.Private) != 0 {
		t.Fatal("old key not destroyed", events, removable)
	}
	plain := f.decrypted(t)
	requireNoPrivateKey(t, plain, old)
	if bytes.Contains(plain, old.Private) {
		t.Fatal("PKCS #8 bytes left in the sealed file")
	}
	var records []DNSRecord
	for _, r := range f.load(t).DKIMRecords(until) {
		if r.State == DKIMRemovable {
			records = append(records, r)
		}
	}
	if len(records) != 1 || records[0].RecordName != old.recordName() || records[0].RecordValue != "" {
		t.Fatal("removable record", records)
	}
	f.unchanged(t, func() { f.tick(t, until) })
	f.unchanged(t, func() { f.tick(t, until.Add(30*24*time.Hour-time.Second)) })
	if events := f.tick(t, until.Add(30*24*time.Hour)); len(events) != 1 || events[0].Phase != "drop" {
		t.Fatal("removable entry not dropped", events)
	}
	if _, ok := f.load(t).dkimKey("example.test", DKIMRemovable); ok {
		t.Fatal("removable entry kept")
	}
	f.unchanged(t, func() { f.tick(t, until.Add(30*24*time.Hour)) })
	// The domain may rotate again.
	if _, _, _, err := f.r.Rotate(ctx, until.Add(30*24*time.Hour), "example.test"); err != nil {
		t.Fatal(err)
	}
}

func TestDKIMRotationEmergency(t *testing.T) {
	ctx := context.Background()
	f := newRotation(t)
	first := f.key(t, DKIMActive)
	_, next, _, err := f.r.Rotate(ctx, time.Now(), "example.test")
	if err != nil {
		t.Fatal(err)
	}
	f.publish(t, next)
	f.confirm(t, time.Now()) // first retiring, next active
	_, waiting, _, err := f.r.Rotate(ctx, time.Now(), "example.test")
	if !errors.Is(err, ErrDKIMRetiring) || waiting.Selector != "" {
		t.Fatal("normal rotation allowed while retiring", err)
	}
	// A next waits too, so the emergency must drop it.
	c := f.load(t)
	if err := c.keepDKIMKeys(c.DKIM, time.Now()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c, events, err := f.r.RotateNow(ctx, now, "example.test")
	if err != nil || len(events) != 1 || events[0].Trigger != "emergency" || events[0].OldSelector != next.Selector {
		t.Fatal("emergency", err, events)
	}
	active, _ := c.dkimKey("example.test", DKIMActive)
	removable, _ := c.dkimKey("example.test", DKIMRemovable)
	retiring, _ := c.dkimKey("example.test", DKIMRetiring)
	if active.Selector == next.Selector || !active.ActivatedAt.Equal(at(now)) || removable.Selector != next.Selector || len(removable.Private) != 0 ||
		!removable.KeepPublishedUntil.Equal(at(now)) || retiring.Selector != first.Selector {
		t.Fatal("emergency state", c.DKIM)
	}
	if _, ok := c.dkimKey("example.test", DKIMNext); ok {
		t.Fatal("emergency kept a next key")
	}
	if s := signingSelector(t, f.load(t)); s != active.Selector {
		t.Fatal("emergency key not signing", s)
	}
	// Repeated emergencies keep at most four retired entries per domain.
	for range 6 {
		if _, _, err := f.r.RotateNow(ctx, time.Now(), "example.test"); err != nil {
			t.Fatal(err)
		}
	}
	old := 0
	for _, k := range f.load(t).DKIM {
		if k.State == DKIMRetiring || k.State == DKIMRemovable {
			old++
		}
	}
	if old != 4 {
		t.Fatal("retired list not bounded", old)
	}
}

func TestDKIMRotationValidateRefusesBadStates(t *testing.T) {
	k := testDKIMKey("example.test")
	next := k
	next.Selector, next.State, next.ActivatedAt = "kypost-00000001", DKIMNext, time.Time{}
	removable := k
	removable.Selector, removable.State, removable.RetiredAt, removable.KeepPublishedUntil = "kypost-00000002", DKIMRemovable, k.CreatedAt, k.CreatedAt
	for name, keys := range map[string][]DKIMKey{
		"next without active": {next},
		"two active":          {k, func() DKIMKey { o := k; o.Selector = "kypost-ffffffff"; return o }()},
		"duplicate selector":  {k, func() DKIMKey { o := next; o.Selector = k.Selector; return o }()},
		"removable with key":  {k, removable},
		"unknown state":       {func() DKIMKey { o := k; o.State = "spare"; return o }()},
	} {
		c := directTestProfile()
		c.DKIM = keys
		if c.Validate() == nil {
			t.Fatal("accepted", name)
		}
	}
	ok := directTestProfile()
	removable.Private = nil
	ok.DKIM = []DKIMKey{k, next, removable}
	if ok.Validate() != nil {
		t.Fatal("refused a valid rotation state")
	}
}

// Rotation is manual: an old key only earns a recommendation, never a new key.
func TestDKIMRotationNeverStartsOnItsOwn(t *testing.T) {
	f := newRotation(t)
	created := f.key(t, DKIMActive).CreatedAt
	for _, now := range []time.Time{created.Add(179 * 24 * time.Hour), created.Add(180 * 24 * time.Hour), created.Add(400 * 24 * time.Hour), created.Add(-24 * time.Hour)} {
		f.unchanged(t, func() {
			if events := f.tick(t, now); len(events) != 0 {
				t.Fatal("tick acted on its own", events)
			}
		})
	}
	c := f.load(t)
	if r := c.DKIMRecords(created.Add(180*24*time.Hour - time.Second)); *r[0].RotationRecommended {
		t.Fatal("rotation recommended early")
	}
	if r := c.DKIMRecords(created.Add(180 * 24 * time.Hour)); len(r) != 1 || !r[0].RotationRecommendedAt.Equal(created.Add(180*24*time.Hour)) || *r[0].AgeDays != 180 || !*r[0].RotationRecommended {
		t.Fatal("rotation recommendation", r)
	}

	t.Run("concurrent ticks", func(t *testing.T) {
		f := newRotation(t)
		_, next, _, err := f.r.Rotate(context.Background(), time.Now(), "example.test")
		if err != nil {
			t.Fatal(err)
		}
		f.publish(t, next)
		f.confirm(t, time.Now())
		until := f.key(t, DKIMRetiring).KeepPublishedUntil
		var wg sync.WaitGroup
		var mu sync.Mutex
		destroyed := 0
		for range 2 {
			wg.Go(func() {
				_, events, err := f.r.Tick(context.Background(), until)
				if err != nil {
					t.Error(err)
				}
				mu.Lock()
				destroyed += len(events)
				mu.Unlock()
			})
		}
		wg.Wait()
		removable := f.key(t, DKIMRemovable)
		if destroyed != 1 || len(removable.Private) != 0 || f.key(t, DKIMActive).Selector != next.Selector {
			t.Fatal("concurrent ticks", destroyed, f.load(t).DKIM)
		}
	})
	t.Run("relay mode", func(t *testing.T) {
		f := newRotation(t)
		if _, _, _, err := f.r.Rotate(context.Background(), time.Now(), "example.test"); err != nil {
			t.Fatal(err)
		}
		keys := f.load(t).DKIM
		relay := DomainRelay{Domains: []string{"example.test"}, Issuer: directTestProfile().Issuer, Host: "smtp.example.test", Port: 465, Username: "u", Password: "p"}
		c, err := SaveDomainRelay(context.Background(), f.r.Path, f.r.KeyPath, relay)
		if err != nil {
			t.Fatal(err)
		}
		f.gen = c.Generation
		f.unchanged(t, func() { f.tick(t, keys[0].CreatedAt.Add(400*24*time.Hour)) })
		if got := f.load(t).DKIM; !slices.EqualFunc(got, keys, DKIMKey.equal) {
			t.Fatal("relay tick touched keys")
		}
	})
}

func TestDKIMRotationCrashResume(t *testing.T) {
	ctx := context.Background()
	prepared := func(t *testing.T, f rotationFixture) time.Time {
		_, next, _, err := f.r.Rotate(ctx, time.Now(), "example.test")
		if err != nil {
			t.Fatal(err)
		}
		f.publish(t, next)
		return time.Now()
	}
	// Each case reaches a state, fails the next transition, then resumes it:
	// the window by a tick (a restart runs one at start), admin steps only by
	// the administrator retrying.
	for _, c := range []struct {
		name   string
		window bool
		setup  func(t *testing.T, f rotationFixture) time.Time
		run    func(f rotationFixture, now time.Time) error
	}{
		{"prepare", false, func(*testing.T, rotationFixture) time.Time { return time.Now() },
			func(f rotationFixture, now time.Time) error {
				_, next, _, err := f.r.Rotate(ctx, now, "example.test")
				if err != nil && next.Selector != "" {
					return errors.New("unsaved selector shown")
				}
				return err
			}},
		{"switch", false, prepared, func(f rotationFixture, now time.Time) error {
			_, _, _, err := f.r.Confirm(ctx, now, "example.test")
			return err
		}},
		{"destruction", true, func(t *testing.T, f rotationFixture) time.Time {
			prepared(t, f)
			f.confirm(t, time.Now())
			return f.key(t, DKIMRetiring).KeepPublishedUntil
		}, func(f rotationFixture, now time.Time) error { _, _, err := f.r.Tick(ctx, now); return err }},
		{"emergency", false, func(*testing.T, rotationFixture) time.Time { return time.Now() },
			func(f rotationFixture, now time.Time) error {
				_, _, err := f.r.RotateNow(ctx, now, "example.test")
				return err
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRotation(t)
			now := c.setup(t, f)
			before := f.bytes(t)
			active := f.key(t, DKIMActive)
			relayWriteFile = func(string, []byte, os.FileMode) error { return errors.New("disk full") }
			t.Cleanup(func() { relayWriteFile = fsutilWrite })
			if err := c.run(f, now); err == nil {
				t.Fatal("failed write reported success")
			}
			relayWriteFile = fsutilWrite
			if !bytes.Equal(before, f.bytes(t)) {
				t.Fatal("failed transition changed the file")
			}
			if s := signingSelector(t, f.load(t)); s != active.Selector {
				t.Fatal("active key lost after a failed write", s)
			}
			if !c.window {
				f.unchanged(t, func() { f.tick(t, now) }) // a restart never finishes an admin step
			}
			if err := c.run(f, now); err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(before, f.bytes(t)) {
				t.Fatal("resumed transition wrote nothing")
			}
			f.unchanged(t, func() { f.tick(t, now) })
		})
	}
}

var fsutilWrite = relayWriteFile

func TestDKIMRotationRecordsShape(t *testing.T) {
	f := newRotation(t)
	created := f.key(t, DKIMActive).CreatedAt
	records := f.load(t).DKIMRecords(created.Add(10*24*time.Hour + time.Hour))
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(raw, &got); err != nil || len(got) != 1 {
		t.Fatal(err)
	}
	if got[0]["state"] != "active" || got[0]["ageDays"] != float64(10) || got[0]["rotationRecommendedAt"] != created.Add(180*24*time.Hour).Format(time.RFC3339) || got[0]["rotationRecommended"] != false {
		t.Fatal("active record fields", got[0])
	}
	if _, ok := got[0]["privateKey"]; ok {
		t.Fatal("private key in records")
	}
	requireNoPrivateKey(t, raw, f.key(t, DKIMActive))
	if base64.StdEncoding.EncodeToString(f.key(t, DKIMActive).Private) == "" {
		t.Fatal("fixture has no key")
	}
}
