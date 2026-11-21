//go:build linux

package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
)

// The hosted receiving transport (RX PR 2). Multi mode is refused at startup until K's
// PR 7, so these tests record it directly, as K's own tests do.

const tlsStaffOrg = "6f1c2b9e-3d4a-4f5b-8c7d-0e1f2a3b4c5d"

type tlsFixture struct {
	r    *receivingRuntime
	db   *sql.DB
	cert tls.Certificate
	pin  string // the server key's pin
	keys string // a directory for MX keys and remote files
}

func multiReceivingFixture(t *testing.T) *tlsFixture {
	t.Helper()
	r, _ := receivingFixture(t)
	quarantineCLI(t, r)
	t.Setenv("KYPOST_TENANCY", config.TenancyMulti)
	t.Setenv("KYPOST_STAFF_ORG_ID", tlsStaffOrg)
	t.Setenv("SECRET_DIR", t.TempDir())
	t.Setenv(receivingRemoteEnv, "")
	st, err := state.New(r.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	err = tenancy.Check(context.Background(), st, r.stateDir, config.TenancyMulti, tlsStaffOrg, func() (bool, error) { return true, nil })
	_ = st.Close()
	if err != nil {
		t.Fatal(err)
	}
	db, err := tenancy.Open(r.stateDir, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	key, err := receivingServerKey()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := receivingCertificate(key)
	if err != nil {
		t.Fatal(err)
	}
	keys := t.TempDir()
	if err := os.Chmod(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	return &tlsFixture{r: r, db: db, cert: cert, pin: keyPin(key), keys: keys}
}

// serve starts a replica's TLS listener on loopback and counts its connections.
func (f *tlsFixture) serve(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var conns atomic.Int32
	go func() {
		defer close(done)
		_ = serveReceiving(ctx, ln, func(c net.Conn) { conns.Add(1); f.r.serveTLS(c, f.db, f.cert) })
	}()
	t.Cleanup(func() { cancel(); <-done })
	return ln.Addr().String(), &conns
}

// key generates an MX key with the CLI and, for a non-empty id, enrols it.
func (f *tlsFixture) key(t *testing.T, name, id string) (string, string) {
	t.Helper()
	path := filepath.Join(f.keys, name+".key")
	var out bytes.Buffer
	if err := runReceivingGateway([]string{"keygen", path}, &out); err != nil {
		t.Fatal(err)
	}
	pin := strings.TrimSpace(out.String())
	if id != "" {
		if err := runReceivingGateway([]string{"add", id, pin}, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	return path, pin
}

// remote points this process's helper at servers, with key and the given server pins.
func (f *tlsFixture) remote(t *testing.T, key string, servers []string, pins ...string) {
	t.Helper()
	if len(pins) == 0 {
		pins = []string{f.pin}
	}
	raw, err := json.Marshal(map[string]any{"key": key, "servers": servers, "server_pins": pins})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.keys, fmt.Sprintf("remote-%d.json", time.Now().UnixNano()))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(receivingRemoteEnv, path)
}

// rawCall completes a TLS handshake with key and sends header and body as given. It
// returns the server's whole answer, or the error of a connection closed without one.
func rawCall(t *testing.T, addr string, key ed25519.PrivateKey, header string, body []byte) ([]byte, error) {
	t.Helper()
	cert, err := receivingCertificate(key)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := tls.Dial("tcp", addr, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(append([]byte(header+"\n"), body...)); err != nil {
		return nil, err
	}
	_ = conn.CloseWrite()
	answer, err := io.ReadAll(conn)
	if err == nil && len(answer) == 0 {
		err = io.EOF
	}
	return answer, err
}

func readKey(t *testing.T, path string) ed25519.PrivateKey {
	t.Helper()
	key, err := readReceivingKey(path)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func exitCode(err error) int {
	var c *receivingCommandError
	if errors.As(err, &c) {
		return c.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

const tlsSender = "sender@outside.test"

var tlsMessage = []byte("From: sender@outside.test\r\nTo: one@example.test\r\nSubject: over tls\r\n\r\nbody\r\n")

// bind and accept travel over TLS, keep their exit codes, and the delivery belongs to
// the pinned gateway, never to maddy-local. Another gateway cannot touch it (R4).
func TestReceivingTLSForwardsUnderThePinnedGateway(t *testing.T) {
	f := multiReceivingFixture(t)
	ctx := context.Background()
	addr, _ := f.serve(t)
	keyA, _ := f.key(t, "a", "mx-a")
	f.remote(t, keyA, []string{addr})
	if err := runReceivingCommand([]string{"bind", "tx-1", tlsSender, "one@example.test"}, nil); err != nil {
		t.Fatal("bind:", err)
	}
	if err := runReceivingCommand([]string{"accept", "tx-1", tlsSender}, bytes.NewReader(tlsMessage)); err != nil {
		t.Fatal("accept:", err)
	}
	d, err := f.r.holding.Get(ctx, "mx-a", "tx-1")
	if err != nil || d.State != "pending" || !bytes.Equal(d.Raw, tlsMessage) {
		t.Fatalf("held %q (%v), want the exact message pending under mx-a", d.State, err)
	}
	if d, err := f.r.holding.Get(ctx, receivingGateway, "tx-1"); err == nil && d.State != "" {
		t.Fatalf("delivery also under %s: %q", receivingGateway, d.State)
	}
	if code := exitCode(runReceivingCommand([]string{"bind", "tx-2", tlsSender, "nobody@example.test"}, nil)); code != 3 {
		t.Fatalf("unrouted bind answered %d, want 3", code)
	}

	// mx-b cannot accept into mx-a's transaction: its own namespace has no staged delivery.
	keyB, _ := f.key(t, "b", "mx-b")
	f.remote(t, keyB, []string{addr})
	if err := runReceivingCommand([]string{"bind", "tx-3", tlsSender, "one@example.test"}, nil); err != nil {
		t.Fatal(err)
	}
	f.remote(t, keyA, []string{addr})
	if code := exitCode(runReceivingCommand([]string{"accept", "tx-3", tlsSender}, bytes.NewReader(tlsMessage))); code == 0 {
		t.Fatal("mx-a accepted mx-b's transaction")
	}
	if d, err := f.r.holding.Get(ctx, "mx-b", "tx-3"); err != nil || d.State != "staged" {
		t.Fatalf("mx-b's delivery is %q (%v), want still staged", d.State, err)
	}
}

// A key without a live pin is refused in the handshake: the connection closes with no
// answer byte, so no frame was read, and nothing is staged.
func TestReceivingTLSRefusesUnpinnedKeysBeforeAnyFrame(t *testing.T) {
	f := multiReceivingFixture(t)
	ctx := context.Background()
	addr, _ := f.serve(t)
	unknown, _ := f.key(t, "unknown", "")
	revoked, revokedPin := f.key(t, "revoked", "mx-r")
	if err := runReceivingGateway([]string{"revoke", "mx-r", revokedPin}, io.Discard); err != nil {
		t.Fatal(err)
	}
	expired, expiredPin := f.key(t, "expired", "")
	if _, err := tenancy.AddGateway(ctx, f.db, "mx-e", expiredPin, time.Now().Add(-tenancy.PinLifetime-time.Minute)); err != nil {
		t.Fatal(err)
	}
	retired, _ := f.key(t, "retired", "mx-t")
	if err := runReceivingGateway([]string{"retire", "mx-t"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	reserved, reservedPin := f.key(t, "reserved", "")
	if _, err := f.db.Exec(`INSERT INTO receiving_gateways(id, spki_sha256, created_at, expires_at) VALUES(?, ?, ?, ?)`, receivingGateway, reservedPin, time.Now().Unix(), time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	live, _ := f.key(t, "live", "mx-l")
	header := `{"v":3,"args":["bind","probe","sender@outside.test","one@example.test"],"budget_ms":10000,"length":0}`
	for name, path := range map[string]string{"unknown": unknown, "revoked": revoked, "expired": expired, "retired": retired, "reserved": reserved} {
		if answer, err := rawCall(t, addr, readKey(t, path), header, nil); err == nil {
			t.Errorf("%s key got an answer %v", name, answer)
		}
		f.remote(t, path, []string{addr})
		if code := exitCode(runReceivingCommand([]string{"bind", "probe-" + name, tlsSender, "one@example.test"}, nil)); code != 1 {
			t.Errorf("%s key: helper exit %d, want 1", name, code)
		}
	}
	for _, gateway := range []string{receivingGateway, "mx-r", "mx-e", "mx-t", "mx-l"} {
		for _, id := range []string{"probe", "probe-unknown", "probe-revoked", "probe-expired", "probe-retired", "probe-reserved"} {
			if d, err := f.r.holding.Get(ctx, gateway, id); err == nil && d.State != "" {
				t.Fatalf("%s/%s staged by a refused key", gateway, id)
			}
		}
	}
	// The same header from a live pin is read and answered.
	if answer, err := rawCall(t, addr, readKey(t, live), header, nil); err != nil || !bytes.Equal(answer, []byte{0}) {
		t.Fatalf("live pin answered %v %v", answer, err)
	}
}

// The helper refuses a replica whose key it has not pinned, before sending anything.
func TestReceivingTLSHelperRefusesAWrongServerKey(t *testing.T) {
	f := multiReceivingFixture(t)
	addr, conns := f.serve(t)
	key, _ := f.key(t, "a", "mx-a")
	f.remote(t, key, []string{addr}, strings.Repeat("0", 64))
	if code := exitCode(runReceivingCommand([]string{"bind", "wrong-server", tlsSender, "one@example.test"}, nil)); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if d, err := f.r.holding.Get(context.Background(), "mx-a", "wrong-server"); err == nil && d.State != "" {
		t.Fatal("bind reached a server whose key is not pinned")
	}
	if conns.Load() == 0 {
		t.Fatal("the helper never dialled")
	}
	// With the server's pin among others it goes through.
	f.remote(t, key, []string{addr}, strings.Repeat("0", 64), f.pin)
	if err := runReceivingCommand([]string{"bind", "right-server", tlsSender, "one@example.test"}, nil); err != nil {
		t.Fatal(err)
	}
}

// An exact accept of an archived delivery succeeds and writes nothing; a new recipient
// on the archived ID is refused.
func TestReceivingTLSReplayOfArchivedAccept(t *testing.T) {
	f := multiReceivingFixture(t)
	ctx := context.Background()
	addr, _ := f.serve(t)
	key, _ := f.key(t, "a", "mx-a")
	f.remote(t, key, []string{addr})
	if err := runReceivingCommand([]string{"bind", "tx", tlsSender, "one@example.test"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := runReceivingCommand([]string{"accept", "tx", tlsSender}, bytes.NewReader(tlsMessage)); err != nil {
		t.Fatal(err)
	}
	importer := *f.r
	importer.gateway = "mx-a"
	if err := importer.importDelivery(ctx, "tx"); err != nil {
		t.Fatal(err)
	}
	before, err := f.r.holding.Get(ctx, "mx-a", "tx")
	if err != nil || before.State != "archived" {
		t.Fatalf("%q %v", before.State, err)
	}
	if err := runReceivingCommand([]string{"accept", "tx", tlsSender}, bytes.NewReader(tlsMessage)); err != nil {
		t.Fatal("exact replay:", err)
	}
	if err := runReceivingCommand([]string{"accept", "tx", tlsSender}, bytes.NewReader(append(tlsMessage, 'x'))); err == nil {
		t.Fatal("replay with other bytes accepted")
	}
	if err := runReceivingCommand([]string{"bind", "tx", tlsSender, "two@example.test"}, nil); err == nil {
		t.Fatal("new recipient bound to an archived ID")
	}
	after, err := f.r.holding.Get(ctx, "mx-a", "tx")
	if err != nil || after.State != "archived" || fmt.Sprint(after.Bindings) != fmt.Sprint(before.Bindings) {
		t.Fatalf("archived delivery changed: %+v %v", after, err)
	}
}

// An oversize body is refused by the helper before sending; a length that does not match
// the bytes is refused by the server before accept; a v2 header over TLS is refused.
func TestReceivingTLSRefusesBadFrames(t *testing.T) {
	f := multiReceivingFixture(t)
	ctx := context.Background()
	addr, conns := f.serve(t)
	keyPath, _ := f.key(t, "a", "mx-a")
	f.remote(t, keyPath, []string{addr})
	key := readKey(t, keyPath)
	if err := runReceivingCommand([]string{"bind", "big", tlsSender, "one@example.test"}, nil); err != nil {
		t.Fatal(err)
	}
	before := conns.Load()
	huge := bytes.Repeat([]byte("x"), int(receivingLimits.MessageBytes)+1)
	if code := exitCode(runReceivingCommand([]string{"accept", "big", tlsSender}, bytes.NewReader(huge))); code != 1 {
		t.Fatalf("oversize accept exit %d, want 1", code)
	}
	if conns.Load() != before {
		t.Fatal("the helper sent an oversize message")
	}
	if err := runReceivingCommand([]string{"bind", "short", tlsSender, "one@example.test"}, nil); err != nil {
		t.Fatal(err)
	}
	for name, frame := range map[string]struct {
		header string
		body   []byte
	}{
		"short":    {fmt.Sprintf(`{"v":3,"args":["accept","short","%s"],"budget_ms":10000,"length":%d}`, tlsSender, len(tlsMessage)), tlsMessage[:10]},
		"long":     {fmt.Sprintf(`{"v":3,"args":["accept","short","%s"],"budget_ms":10000,"length":10}`, tlsSender), tlsMessage},
		"oversize": {fmt.Sprintf(`{"v":3,"args":["accept","short","%s"],"budget_ms":10000,"length":%d}`, tlsSender, receivingLimits.MessageBytes+1), tlsMessage},
		"v2":       {fmt.Sprintf(`{"v":2,"args":["accept","short","%s"],"deadline":%d,"length":%d}`, tlsSender, time.Now().Add(10*time.Second).UnixNano(), len(tlsMessage)), tlsMessage},
		"extra":    {fmt.Sprintf(`{"v":3,"args":["accept","short","%s"],"budget_ms":10000,"length":%d,"gateway":"mx-b"}`, tlsSender, len(tlsMessage)), tlsMessage},
		"budget":   {fmt.Sprintf(`{"v":3,"args":["accept","short","%s"],"budget_ms":0,"length":%d}`, tlsSender, len(tlsMessage)), tlsMessage},
		"config":   {`{"v":3,"args":["config","x"],"budget_ms":10000,"length":0}`, nil},
	} {
		if answer, err := rawCall(t, addr, key, frame.header, frame.body); err != nil || !bytes.Equal(answer, []byte{1}) {
			t.Errorf("%s: answer %v %v, want exit 1", name, answer, err)
		}
	}
	if d, err := f.r.holding.Get(ctx, "mx-a", "short"); err != nil || d.State != "staged" {
		t.Fatalf("a bad frame changed the delivery: %q %v", d.State, err)
	}
}

// A replica that accepts the connection and never answers gives exit 1 within the
// helper's own budget.
func TestReceivingTLSHungReplica(t *testing.T) {
	f := multiReceivingFixture(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{f.cert}, ClientAuth: tls.RequireAnyClientCert})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c) }() // reads, never answers
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	key, _ := f.key(t, "a", "mx-a")
	f.remote(t, key, []string{ln.Addr().String()})
	remote, err := loadReceivingRemote()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	start := time.Now()
	err = remote.forward(ctx, []string{"bind", "hung", tlsSender, "one@example.test"}, nil)
	if code := exitCode(err); code != 1 || time.Since(start) > 7500*time.Millisecond {
		t.Fatalf("exit %d after %v, want 1 within the 7s budget", code, time.Since(start))
	}
}

// bind and accept of one transaction may reach different replicas: they share the store.
func TestReceivingTLSBindAndAcceptOnDifferentReplicas(t *testing.T) {
	f := multiReceivingFixture(t)
	a, connsA := f.serve(t)
	b, connsB := f.serve(t)
	key, _ := f.key(t, "a", "mx-a")
	f.remote(t, key, []string{a})
	if err := runReceivingCommand([]string{"bind", "split", tlsSender, "one@example.test"}, nil); err != nil {
		t.Fatal(err)
	}
	f.remote(t, key, []string{b})
	if err := runReceivingCommand([]string{"accept", "split", tlsSender}, bytes.NewReader(tlsMessage)); err != nil {
		t.Fatal(err)
	}
	if connsA.Load() != 1 || connsB.Load() != 1 {
		t.Fatalf("replica A served %d, B %d; want one each", connsA.Load(), connsB.Load())
	}
	if d, err := f.r.holding.Get(context.Background(), "mx-a", "split"); err != nil || d.State != "pending" {
		t.Fatalf("%q %v", d.State, err)
	}
}

// Every exit code a replica answers reaches Maddy unchanged, and the hosted profile maps
// 0-11 exactly as the single-tenant one does; only it has 12 and 13. The profile is
// fetched with the config command and holds the replica's domains.
func TestReceivingTLSExitCodesAndHostedProfile(t *testing.T) {
	f := multiReceivingFixture(t)
	key, _ := f.key(t, "a", "mx-a")
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{f.cert}, ClientAuth: tls.RequireAnyClientCert})
	if err != nil {
		t.Fatal(err)
	}
	var next atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, c)
			_, _ = c.Write([]byte{byte(next.Load())})
			_ = c.Close()
		}
	}()
	f.remote(t, key, []string{ln.Addr().String()})
	for code := range 14 {
		next.Store(int32(code))
		if got := exitCode(runReceivingCommand([]string{"bind", "codes", tlsSender, "one@example.test"}, nil)); got != code {
			t.Errorf("replica answered %d, helper exited %d", code, got)
		}
	}
	_ = ln.Close()

	addr, _ := f.serve(t)
	f.remote(t, key, []string{addr})
	cert, certKey, leaf := receivingTestCertificate(t, t.TempDir())
	var hosted bytes.Buffer
	if err := runReceivingConfig([]string{"127.0.0.1:2525", leaf.DNSNames[0], cert, certKey}, &hosted); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var single bytes.Buffer
	if err := f.r.writeConfig(context.Background(), executable, "127.0.0.1:2525", leaf.DNSNames[0], cert, certKey, ` "{source_ip}" "{source_host}"`, &single); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hosted.String(), "code 12 reject 451 4.2.1") || !strings.Contains(hosted.String(), "code 13 reject 450 4.7.1") {
		t.Fatal("hosted profile lacks exit codes 12 and 13")
	}
	if got := strings.Replace(hosted.String(), hostedBindCodes, "", 1); got != single.String() {
		t.Fatalf("hosted profile differs beyond codes 12 and 13:\n%s\n---\n%s", got, single.String())
	}
	if !strings.Contains(hosted.String(), "destination example.test {") {
		t.Fatal("hosted profile lacks the replica's domain")
	}
}

// Single-tenant: every new piece refuses, the helper never falls back to the socket or to
// running in process, and protocol 3 is not spoken on the socket.
func TestReceivingTLSRefusedOutsideMultiMode(t *testing.T) {
	r, _ := receivingFixture(t)
	quarantineCLI(t, r)
	t.Setenv("SECRET_DIR", t.TempDir())
	t.Setenv(receivingRemoteEnv, "")
	st, err := state.New(r.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	key := filepath.Join(t.TempDir(), "gw.key")
	pin := strings.Repeat("a", 64)
	for _, mode := range []string{"", config.TenancySingle, config.TenancyMulti} {
		t.Setenv("KYPOST_TENANCY", mode)
		t.Setenv("KYPOST_STAFF_ORG_ID", "")
		if mode == config.TenancyMulti {
			t.Setenv("KYPOST_STAFF_ORG_ID", tlsStaffOrg) // multi in the environment, but no multi record
		}
		if err := runReceivingServe([]string{"--tls", "127.0.0.1:2526"}); !errors.Is(err, errReceivingSingle) {
			t.Errorf("%q: serve --tls: %v", mode, err)
		}
		for _, args := range [][]string{{"add", "mx-a", pin}, {"revoke", "mx-a", pin}, {"retire", "mx-a"}, {"server-pin"}} {
			if err := runReceivingGateway(args, io.Discard); !errors.Is(err, errReceivingSingle) {
				t.Errorf("%q: gateway %v: %v", mode, args, err)
			}
		}
		if mode != config.TenancyMulti {
			if err := runReceivingGateway([]string{"keygen", key}, io.Discard); !errors.Is(err, errReceivingSingle) {
				t.Errorf("%q: keygen: %v", mode, err)
			}
			t.Setenv(receivingRemoteEnv, filepath.Join(t.TempDir(), "remote.json"))
			if err := runReceivingCommand([]string{"bind", "x", tlsSender, "one@example.test"}, nil); !errors.Is(err, errReceivingSingle) || exitCode(err) != 1 {
				t.Errorf("%q: remote helper: %v", mode, err)
			}
			t.Setenv(receivingRemoteEnv, "")
		}
	}
	if _, err := os.Lstat(key); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("keygen wrote a key outside multi mode")
	}
	if entries, _ := os.ReadDir(config.SecretDir()); len(entries) != 0 {
		t.Fatal("a server key was created outside multi mode")
	}
	if _, err := os.Lstat(filepath.Join(r.stateDir, tenancy.File)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("tenancy.db created on a single-tenant install")
	}

	// Protocol 3 on the socket is refused like any malformed request.
	client, server := socketPair(t)
	defer client.Close()
	go r.serveConn(server)
	if _, err := client.Write([]byte(`{"v":3,"args":["bind","x","sender@outside.test","one@example.test"],"budget_ms":10000,"length":0}` + "\n")); err != nil {
		t.Fatal(err)
	}
	_ = client.CloseWrite()
	var code [1]byte
	if _, err := io.ReadFull(client, code[:]); err != nil || code[0] != 1 {
		t.Fatalf("protocol 3 on the socket answered %v %v", code, err)
	}
}

// An MX host never runs a command locally or through a local socket: with its replicas
// unreachable, a listening socket server is not used.
func TestReceivingRemoteHasNoLocalFallback(t *testing.T) {
	f := multiReceivingFixture(t)
	ln, err := listenReceiving(receivingSocket())
	if err != nil {
		t.Fatal(err)
	}
	var served atomic.Int32
	go func() {
		for {
			c, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			served.Add(1)
			f.r.serveConn(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := closed.Addr().String()
	_ = closed.Close()
	key, _ := f.key(t, "a", "mx-a")
	f.remote(t, key, []string{unreachable})
	for _, args := range [][]string{{"bind", "nofallback", tlsSender, "one@example.test"}, {"init"}} {
		if code := exitCode(runReceivingCommand(args, nil)); code != 1 {
			t.Errorf("%v: exit %d, want 1", args, code)
		}
	}
	if served.Load() != 0 {
		t.Fatal("the helper used the local socket")
	}
	if d, err := f.r.holding.Get(context.Background(), receivingGateway, "nofallback"); err == nil && d.State != "" {
		t.Fatal("the command ran locally")
	}
	// A malformed remote file is refused, not ignored.
	bad := filepath.Join(f.keys, "bad.json")
	for _, body := range []string{`{}`, `{"key":"` + key + `","servers":["8.8.8.8:25"],"server_pins":["` + f.pin + `"]}`, `{"key":"` + key + `","servers":["127.0.0.1:1"],"server_pins":["` + f.pin + `"],"local":true}`} {
		if err := os.WriteFile(bad, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(receivingRemoteEnv, bad)
		if _, err := loadReceivingRemote(); err == nil {
			t.Errorf("accepted remote file %s", body)
		}
	}
	if err := os.Chmod(bad, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadReceivingRemote(); err == nil {
		t.Error("accepted a group-readable remote file")
	}
}

// keygen writes an owner-only key once and prints its pin; server-pin prints the shared
// SECRET_DIR key's pin, created once.
func TestReceivingGatewayKeys(t *testing.T) {
	f := multiReceivingFixture(t)
	path, pin := f.key(t, "a", "")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v %v", info, err)
	}
	if keyPin(readKey(t, path)) != pin || !tenancy.ValidPin(pin) {
		t.Fatal("printed pin is not the key's")
	}
	if err := runReceivingGateway([]string{"keygen", path}, io.Discard); err == nil {
		t.Fatal("keygen replaced a key")
	}
	if err := runReceivingGateway([]string{"keygen", "relative.key"}, io.Discard); err == nil {
		t.Fatal("keygen took a relative path")
	}
	var out bytes.Buffer
	if err := runReceivingGateway([]string{"server-pin"}, &out); err != nil || strings.TrimSpace(out.String()) != f.pin {
		t.Fatalf("server-pin %q %v, want %s", out.String(), err, f.pin)
	}
	if info, err := os.Stat(filepath.Join(config.SecretDir(), receivingServerKeyFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("server key %v %v", info, err)
	}
	for _, args := range [][]string{{"add", "maddy-local", pin}, {"add", "mx-a", "nothex"}, {"add", "mx-a"}, {"retire", "MX"}, {"bogus"}} {
		if err := runReceivingGateway(args, io.Discard); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	out.Reset()
	if err := runReceivingGateway([]string{"add", "mx-a", pin}, &out); err != nil || !strings.HasPrefix(out.String(), "enrolled mx-a until ") {
		t.Fatalf("%q %v", out.String(), err)
	}
}
