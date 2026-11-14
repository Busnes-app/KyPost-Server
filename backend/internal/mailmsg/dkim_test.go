package mailmsg

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/emersion/go-msgauth/dkim"
)

var testDKIMKeys sync.Map

// testDKIMKey generates one RSA-2048 key per domain per test binary.
func testDKIMKey(domain string) DKIMKey {
	if k, ok := testDKIMKeys.Load(domain); ok {
		return k.(DKIMKey)
	}
	k, err := DomainRelay{}.newDKIMKey(domain, DKIMActive, time.Now())
	if err != nil {
		panic(err)
	}
	actual, _ := testDKIMKeys.LoadOrStore(domain, k)
	return actual.(DKIMKey)
}

var selectorPattern = regexp.MustCompile(`^kypost-[0-9a-f]{8}$`)

func requireDKIMKeys(t *testing.T, c DomainRelay, domains ...string) {
	t.Helper()
	if len(c.DKIM) != len(domains) {
		t.Fatalf("want keys for %v, got %d", domains, len(c.DKIM))
	}
	for i, domain := range domains {
		k := c.DKIM[i]
		key, err := k.signer()
		if k.Domain != domain || !selectorPattern.MatchString(k.Selector) || err != nil || key.N.BitLen() != 2048 {
			t.Fatalf("key %d for %s: %s %s %v", i, domain, k.Domain, k.Selector, err)
		}
	}
}

func TestDKIMKeysCreatedOncePerDirectDomain(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path, keyPath := filepath.Join(dir, "native-relay.json"), filepath.Join(dir, "native-relay.key")
	profile := directTestProfile()
	profile.DKIM = nil
	profile.Domains = []string{"a.test", "example.test"}
	first, err := SaveDomainRelay(ctx, path, keyPath, profile)
	if err != nil {
		t.Fatal(err)
	}
	requireDKIMKeys(t, first, "a.test", "example.test")
	again, err := SaveDomainRelay(ctx, path, keyPath, profile)
	if err != nil || again.Generation == first.Generation || !slices.EqualFunc(again.DKIM, first.DKIM, sameDKIM) {
		t.Fatal("saving the same profile changed keys", err)
	}
	added, err := SetDomainRelayDomains(ctx, path, keyPath, []string{"a.test", "b.test", "example.test"})
	if err != nil || added.Generation != again.Generation {
		t.Fatal("domains-only edit rotated the generation", err)
	}
	requireDKIMKeys(t, added, "a.test", "b.test", "example.test")
	if !sameDKIM(added.DKIM[0], first.DKIM[0]) || !sameDKIM(added.DKIM[2], first.DKIM[1]) {
		t.Fatal("domains-only edit replaced an existing key")
	}
	stored, _, err := ReadDomainRelay(path, keyPath)
	if err != nil || !stored.Equal(added) {
		t.Fatal("keys not persisted", err)
	}
	// The private key only exists as ciphertext on disk.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range added.DKIM {
		requireNoPrivateKey(t, raw, k)
	}
}

func sameDKIM(a, b DKIMKey) bool { return a.equal(b) }

// requireNoPrivateKey refuses PEM markers and any 32-character run of the
// key's base64 encoding. The first 64 characters are skipped: they encode the
// PKCS #8 RSA-2048 header every key shares, so another key would match them.
func requireNoPrivateKey(t *testing.T, out []byte, k DKIMKey) {
	t.Helper()
	if bytes.Contains(out, []byte("PRIVATE KEY")) {
		t.Fatal("output contains a PEM private key")
	}
	encoded := base64.StdEncoding.EncodeToString(k.Private)
	for i := 64; i+32 <= len(encoded); i += 16 {
		if bytes.Contains(out, []byte(encoded[i:i+32])) {
			t.Fatal("output contains part of the DKIM private key")
		}
	}
}

func TestDKIMKeysOutliveModeSwitches(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path, keyPath := filepath.Join(dir, "native-relay.json"), filepath.Join(dir, "native-relay.key")
	direct := directTestProfile()
	direct.DKIM = nil
	first, err := SaveDomainRelay(ctx, path, keyPath, direct)
	if err != nil {
		t.Fatal(err)
	}
	relay := DomainRelay{Domains: direct.Domains, Issuer: direct.Issuer, Host: "smtp.example.test", Port: 465, Username: "login", Password: "secret"}
	switched, err := SaveDomainRelay(ctx, path, keyPath, relay)
	if err != nil || !slices.EqualFunc(switched.DKIM, first.DKIM, sameDKIM) {
		t.Fatal("relay mode dropped the direct key", err)
	}
	// Relay mode adds no key for a new domain.
	relay.Domains = []string{"example.test", "relay-only.test"}
	if switched, err = SaveDomainRelay(ctx, path, keyPath, relay); err != nil || len(switched.DKIM) != 1 {
		t.Fatal("relay mode generated a key", err)
	}
	back, err := SaveDomainRelay(ctx, path, keyPath, direct)
	if err != nil || !sameDKIM(back.DKIM[0], first.DKIM[0]) {
		t.Fatal("direct -> relay -> direct changed the key", err)
	}
	requireDKIMKeys(t, back, "example.test") // relay-only.test is retired: no new key
	// A key goes only once its domain leaves both Domains and RetiredDomains.
	retired := DomainRelay{Mode: "direct", Domains: []string{"new.test"}, RetiredDomains: []string{"example.test"}}
	if err := retired.keepDKIMKeys([]DKIMKey{testDKIMKey("example.test"), testDKIMKey("gone.test")}, time.Now()); err != nil {
		t.Fatal(err)
	}
	requireDKIMKeys(t, retired, "example.test", "new.test")
	if !sameDKIM(retired.DKIM[0], testDKIMKey("example.test")) {
		t.Fatal("retired domain key replaced")
	}
}

func TestDKIMReconcileAddsMissingKeysOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path, keyPath := filepath.Join(dir, "native-relay.json"), filepath.Join(dir, "native-relay.key")
	if changed, err := ReconcileDKIMKeys(ctx, path, keyPath); err != nil || changed {
		t.Fatal("absent profile changed", err)
	}
	old := directTestProfile()
	old.DKIM = nil // the pre-DKIM file format: no "dkim" field at all
	writeSealedRelay(t, path, keyPath, old)
	if fixture, _, err := ReadDomainRelayAnyVersion(path, keyPath); err != nil || len(fixture.DKIM) != 0 {
		t.Fatal("fixture is not a keyless direct profile", err)
	}
	changed, err := ReconcileDKIMKeys(ctx, path, keyPath)
	if err != nil || !changed {
		t.Fatal("missing key not added", err)
	}
	keyed, _, err := ReadDomainRelay(path, keyPath)
	if err != nil || keyed.Generation != old.Generation {
		t.Fatal("reconciliation changed the generation", err)
	}
	requireDKIMKeys(t, keyed, "example.test")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err = ReconcileDKIMKeys(ctx, path, keyPath); err != nil || changed {
		t.Fatal("second run changed the profile", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("second run rewrote the file")
	}
	relay := DomainRelay{Generation: old.Generation, Domains: old.Domains, Issuer: old.Issuer, Host: "smtp.example.test", Port: 465, Username: "login", Password: "secret"}
	writeSealedRelay(t, path, keyPath, relay)
	if changed, err = ReconcileDKIMKeys(ctx, path, keyPath); err != nil || changed {
		t.Fatal("relay profile got keys", err)
	}
}

func writeSealedRelay(t *testing.T, path, keyPath string, c DomainRelay) {
	t.Helper()
	master, err := cryptutil.LoadOrCreateKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := SealDomainRelay(c, master)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// signatureField splits the leading DKIM-Signature field from the message.
func signatureField(t *testing.T, wire string) (map[string]string, string) {
	t.Helper()
	if !strings.HasPrefix(wire, "DKIM-Signature:") {
		t.Fatal("wire does not start with DKIM-Signature")
	}
	end := 0
	for {
		i := strings.Index(wire[end:], "\r\n")
		if i < 0 {
			t.Fatal("unterminated signature")
		}
		end += i + 2
		if wire[end] != ' ' && wire[end] != '\t' {
			break
		}
	}
	value := strings.Join(strings.Fields(strings.TrimPrefix(wire[:end], "DKIM-Signature:")), "")
	tags := map[string]string{}
	for _, part := range strings.Split(value, ";") {
		if k, v, ok := strings.Cut(part, "="); ok {
			tags[k] = v
		}
	}
	if strings.Count(wire, "DKIM-Signature:") != 1 {
		t.Fatal("more than one DKIM-Signature")
	}
	return tags, wire[end:]
}

// verifyWire checks a captured DATA payload with dkim.Verify against the
// profile's own public record and returns the bytes after the signature.
func verifyWire(t *testing.T, c DomainRelay, wire string) string {
	t.Helper()
	records := map[string]string{}
	for _, r := range c.DKIMRecords(time.Now()) {
		records[r.RecordName] = r.RecordValue
	}
	result, err := dkim.VerifyWithOptions(strings.NewReader(wire), &dkim.VerifyOptions{LookupTXT: func(name string) ([]string, error) {
		if v, ok := records[name]; ok {
			return []string{v}, nil
		}
		return nil, &net.DNSError{IsNotFound: true}
	}})
	if err != nil || len(result) != 1 || result[0].Err != nil || result[0].Domain != "example.test" {
		t.Fatal("wire signature does not verify", err, result)
	}
	tags, rest := signatureField(t, wire)
	k, _ := c.dkimKey("example.test", DKIMActive)
	if tags["d"] != "example.test" || tags["s"] != k.Selector || tags["a"] != "rsa-sha256" || tags["c"] != "relaxed/relaxed" {
		t.Fatal("signature tags", tags)
	}
	if _, ok := tags["l"]; ok {
		t.Fatal("body length limit set")
	}
	return rest
}

func TestDKIMSignedHeaders(t *testing.T) {
	c := directTestProfile()
	msg := "From: A <mailbox@example.test>\r\nTo: b@example.org\r\nCc: c@example.org\r\nBcc: hidden@example.org\r\nSubject: s\r\nDate: Mon, 02 Jan 2006 15:04:05 +0000\r\nMessage-ID: <id@example.test>\r\nReply-To: r@example.test\r\nIn-Reply-To: <p@example.org>\r\nReferences: <p@example.org>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 7bit\r\nX-Unsigned: kept\r\n\r\nbody\r\n"
	signed, err := c.dkimSign("example.test", []byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	tags, rest := signatureField(t, string(signed))
	if rest != msg {
		t.Fatal("signing changed the message bytes")
	}
	want := "from:to:cc:subject:date:message-id:reply-to:in-reply-to:references:mime-version:content-type:content-transfer-encoding:from"
	got := strings.ToLower(tags["h"])
	// Order inside h= is free; compare as multisets with From oversigned.
	gotList, wantList := strings.Split(got, ":"), strings.Split(want, ":")
	slices.Sort(gotList)
	slices.Sort(wantList)
	if !slices.Equal(gotList, wantList) || strings.Contains(got, "bcc") || strings.Contains(got, "x-unsigned") {
		t.Fatal("signed header list", tags["h"])
	}
	if _, err := verifyWireResult(c, string(signed)); err != nil {
		t.Fatal(err)
	}
}

func verifyWireResult(c DomainRelay, wire string) (*dkim.Verification, error) {
	records := map[string]string{}
	for _, r := range c.DKIMRecords(time.Now()) {
		records[r.RecordName] = r.RecordValue
	}
	result, err := dkim.VerifyWithOptions(strings.NewReader(wire), &dkim.VerifyOptions{LookupTXT: func(name string) ([]string, error) {
		return []string{records[name]}, nil
	}})
	if err != nil || len(result) != 1 {
		return nil, errors.New("no verification")
	}
	return result[0], result[0].Err
}

func TestDirectDeliverFailsClosedWithoutUsableKey(t *testing.T) {
	broken := testDKIMKey("example.test")
	broken.Private = []byte("not a key")
	for name, keys := range map[string][]DKIMKey{"missing": nil, "other-domain": {testDKIMKey("a.test")}, "broken": {broken}} {
		t.Run(name, func(t *testing.T) {
			preserveDirectSeams(t)
			calls := 0
			directLookupMX = func(context.Context, string) ([]*net.MX, error) { calls++; return nil, errors.New("unexpected") }
			directLookupIP = func(context.Context, string, string) ([]net.IP, error) { calls++; return nil, errors.New("unexpected") }
			directDial = func(context.Context, string, string) (net.Conn, error) { calls++; return nil, errors.New("unexpected") }
			c := directTestProfile()
			c.DKIM = keys
			if name == "other-domain" {
				c.Domains = []string{"a.test", "example.test"}
			}
			err := c.Deliver("mailbox@example.test", []string{"visible@example.org"}, []byte(relayProofMIME))
			var reply *textproto.Error
			if !errors.As(err, &reply) || reply.Code != 451 || calls != 0 {
				t.Fatal("unsigned direct mail not refused before DNS and dial", err, calls)
			}
		})
	}
}
