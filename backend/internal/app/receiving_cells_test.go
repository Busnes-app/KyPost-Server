//go:build linux

package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// MH10: an MX host forwards relay-domain mail to the cell its region code names, over the
// pinned transport. The cells, codes and endpoints here are fixtures; the production table
// stays empty.

// cellHost is one cell: its receiving runtime behind a pinned TLS listener on loopback.
type cellHost struct {
	r       *receivingRuntime
	created []users.User
	addr    string
	pin     string
	conns   atomic.Int32
	limit   atomic.Int32 // above this many connections the cell drops them unanswered; 0 never
	masked  string
}

func newCellHost(t *testing.T, masked string) *cellHost {
	t.Helper()
	r, created := importFixture(t, receivingOrgA, receivingOrgB)
	setRelay(t, r, "example.test")
	ctx := context.Background()
	if _, err := r.life.AddNativeAlias(ctx, r.stateDir, created[0].ID, masked); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	setAddressSettings(t, r, created[0].ID, func(s *config.UserSettings) {
		s.MaskedAddresses = []config.MaskedAddress{{Address: masked, State: "active", CreatedAt: now, ExpiresAt: now + 3600}}
	})
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := receivingCertificate(key)
	if err != nil {
		t.Fatal(err)
	}
	c := &cellHost{r: r, created: created, pin: keyPin(key), masked: masked}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = serveReceiving(serveCtx, ln, func(conn net.Conn) {
			if n := c.conns.Add(1); c.limit.Load() > 0 && n > c.limit.Load() {
				_ = conn.Close()
				return
			}
			r.serveTLS(conn, r.tenancy, cert)
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	c.addr = ln.Addr().String()
	return c
}

// enrol pins an MX key as gateway id in this cell (RX R4, one row per cell).
func (c *cellHost) enrol(t *testing.T, id, pin string) {
	t.Helper()
	if _, err := tenancy.AddGateway(context.Background(), c.r.tenancy, id, pin, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func (c *cellHost) state(t *testing.T, gateway, id string) string {
	t.Helper()
	return stateOf(t, c.r, gateway, id)
}

// cellsHelper sets up this process as cell A's MX host, forwarding to cell B as well.
func cellsHelper(t *testing.T) (a, b *cellHost, mxKey, mxPin string, remote func(key string)) {
	t.Helper()
	t.Setenv("KYPOST_TENANCY", config.TenancyMulti)
	setObjectStorageEnv(t)
	t.Setenv("KY_CELLS", "cell-a")
	t.Setenv("KYPOST_STAFF_ORG_ID", tlsStaffOrg)
	t.Setenv("PUSH_RELAY_URL", "")
	t.Setenv("APNS_RELAY_URL", "")
	previous := relayCellFor
	relayCellFor = func(code byte) (string, bool) {
		cell, ok := map[byte]string{'a': "cell-a", 'b': "cell-b", 'c': "cell-c"}[code]
		return cell, ok
	}
	t.Cleanup(func() { relayCellFor = previous })
	a, b = newCellHost(t, "calm.raven.ak7q2xm@example.test"), newCellHost(t, "quiet.otter.bk7q2xm@example.test")
	keys := t.TempDir()
	if err := os.Chmod(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	keygen := func(name string) (string, string) {
		path := filepath.Join(keys, name+".key")
		var out bytes.Buffer
		if err := runReceivingGateway([]string{"keygen", path}, &out); err != nil {
			t.Fatal(err)
		}
		return path, strings.TrimSpace(out.String())
	}
	mxKey, mxPin = keygen("mx-a")
	a.enrol(t, "mx-a", mxPin)
	b.enrol(t, "mx-a", mxPin)
	remote = func(key string) {
		raw, err := json.Marshal(map[string]any{"key": key, "servers": []string{a.addr}, "server_pins": []string{a.pin},
			"relay_domain": "example.test", "cells": map[string]any{"cell-b": map[string]any{"servers": []string{b.addr}, "server_pins": []string{b.pin}}}})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(keys, fmt.Sprintf("remote-%d.json", time.Now().UnixNano()))
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("KYPOST_RECEIVING_REMOTE", path)
	}
	remote(mxKey)
	return a, b, mxKey, mxPin, remote
}

func helperBind(id, rcpt, source string) int {
	return exitCode(runReceivingCommand([]string{"bind", id, tlsSender, rcpt, source}, nil))
}

func helperAccept(id string) int {
	return exitCode(runReceivingCommand([]string{"accept", id, tlsSender}, bytes.NewReader(tlsMessage)))
}

// An address of cell B received at cell A's MX host lands in B, and only B gets the
// message. One message to recipients in A and B lands in both before 250.
func TestCrossCellRelayDelivers(t *testing.T) {
	a, b, _, _, _ := cellsHelper(t)
	if code := helperBind("tx-b", b.masked, probeSource); code != 0 {
		t.Fatalf("bind B's address: exit %d", code)
	}
	if a.state(t, "mx-a", "tx-b") != "gone" || b.state(t, "mx-a", "tx-b") != "staged" {
		t.Fatalf("bind landed A %s, B %s; want only B", a.state(t, "mx-a", "tx-b"), b.state(t, "mx-a", "tx-b"))
	}
	if code := helperAccept("tx-b"); code != 0 {
		t.Fatalf("accept: exit %d", code)
	}
	if a.state(t, "mx-a", "tx-b") != "gone" || b.state(t, "mx-a", "tx-b") != "pending" {
		t.Fatalf("accept left A %s, B %s; want B pending and nothing in A", a.state(t, "mx-a", "tx-b"), b.state(t, "mx-a", "tx-b"))
	}
	// A +tag on B's masked address still routes by its code, and B resolves it.
	tagged := strings.Replace(b.masked, "@", "+shop@", 1)
	if code := helperBind("tx-tag", tagged, probeSource); code != 0 || b.state(t, "mx-a", "tx-tag") != "staged" || a.state(t, "mx-a", "tx-tag") != "gone" {
		t.Fatalf("bind %s: exit %d, A %s, B %s; want it staged in B only", tagged, code, a.state(t, "mx-a", "tx-tag"), b.state(t, "mx-a", "tx-tag"))
	}

	for _, rcpt := range []string{a.masked, b.masked, "one@example.test"} {
		if code := helperBind("tx-ab", rcpt, probeSource); code != 0 {
			t.Fatalf("bind %s: exit %d", rcpt, code)
		}
	}
	if code := helperAccept("tx-ab"); code != 0 {
		t.Fatalf("accept to both cells: exit %d", code)
	}
	for name, c := range map[string]*cellHost{"A": a, "B": b} {
		d, err := c.r.holding.Get(context.Background(), "mx-a", "tx-ab")
		if err != nil || d.State != "pending" || !bytes.Equal(d.Raw, tlsMessage) {
			t.Fatalf("cell %s holds %q (%v), want the message pending", name, d.State, err)
		}
	}
	if got := len(mustDelivery(t, b, "tx-ab").Bindings); got != 1 {
		t.Fatalf("cell B bound %d recipients, want only its own", got)
	}
}

func mustDelivery(t *testing.T, c *cellHost, id string) (d struct{ Bindings []string }) {
	t.Helper()
	got, err := c.r.holding.Get(context.Background(), "mx-a", id)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range got.Bindings {
		d.Bindings = append(d.Bindings, x.Address)
	}
	return d
}

// A failure in B after A committed answers 451 (exit 1): the sender retries, and A keeps a
// second copy, at least once.
func TestCrossCellRelayFailureInOneCellIsTemporary(t *testing.T) {
	a, b, _, _, _ := cellsHelper(t)
	for _, rcpt := range []string{a.masked, b.masked} {
		if code := helperBind("tx-fail", rcpt, probeSource); code != 0 {
			t.Fatalf("bind %s: exit %d", rcpt, code)
		}
	}
	b.limit.Store(b.conns.Load() + 1) // B answers held, then drops the accept
	if code := helperAccept("tx-fail"); code != 1 {
		t.Fatalf("accept with B failing: exit %d, want 1 (451)", code)
	}
	if a.state(t, "mx-a", "tx-fail") != "pending" || b.state(t, "mx-a", "tx-fail") != "staged" {
		t.Fatalf("A %s, B %s; want A committed and B still staged", a.state(t, "mx-a", "tx-fail"), b.state(t, "mx-a", "tx-fail"))
	}
	// B down before held: nothing is sent anywhere, and the answer is still 451.
	if code := helperBind("tx-down", a.masked, probeSource); code != 0 {
		t.Fatal(code)
	}
	b.limit.Store(b.conns.Load())
	if code := helperAccept("tx-down"); code != 1 || a.state(t, "mx-a", "tx-down") != "staged" {
		t.Fatalf("B unreachable: exit %d, A %s; want 1 and A untouched", code, a.state(t, "mx-a", "tx-down"))
	}
}

// A relay address whose code names no cell this host forwards to goes to the own cell, which
// refuses it as unknown and counts it against the source's budget; B sees none of it.
func TestCrossCellRelayUnknownCodeIsCountedHere(t *testing.T) {
	a, b, _, _, _ := cellsHelper(t)
	const source = "192.0.2.77"
	before := b.conns.Load()
	for i := range probeLimit {
		rcpt := []string{"lost.lynx.z7q2xm4@example.test", "far.fox.c7q2xm4@example.test", "odd.owl.b7q2@example.test"}[i%3]
		if code := helperBind(fmt.Sprint("u", i), rcpt, source); code != 3 {
			t.Fatalf("unknown code %d (%s): exit %d, want 3", i, rcpt, code)
		}
	}
	if code := helperBind("u-live", a.masked, source); code != 13 {
		t.Fatalf("past the budget: exit %d, want 13 (the refusals were counted in A)", code)
	}
	if b.conns.Load() != before {
		t.Fatalf("cell B was contacted %d times for codes it does not own", b.conns.Load()-before)
	}
}

// A cell that has not pinned the MX host refuses it in the handshake: B's address answers
// 451 and nothing is staged in B, while A, which pinned it, still takes its own mail.
func TestCrossCellRelayUnpinnedGatewayRefused(t *testing.T) {
	a, b, _, _, remote := cellsHelper(t)
	path := filepath.Join(filepath.Dir(os.Getenv("KYPOST_RECEIVING_REMOTE")), "mx-x.key")
	var out bytes.Buffer
	if err := runReceivingGateway([]string{"keygen", path}, &out); err != nil {
		t.Fatal(err)
	}
	a.enrol(t, "mx-x", strings.TrimSpace(out.String()))
	remote(path)
	before := b.conns.Load()
	if code := helperBind("tx-x", b.masked, probeSource); code != 1 {
		t.Fatalf("bind into a cell without the pin: exit %d, want 1 (451)", code)
	}
	if b.conns.Load() == before || b.state(t, "mx-x", "tx-x") != "gone" {
		t.Fatalf("B: %d connections, %s; want a refused handshake and nothing staged", b.conns.Load()-before, b.state(t, "mx-x", "tx-x"))
	}
	if code := helperBind("tx-x", a.masked, probeSource); code != 0 || a.state(t, "mx-x", "tx-x") != "staged" {
		t.Fatalf("A's own address: exit %d, %s", code, a.state(t, "mx-x", "tx-x"))
	}
}

// held answers 0 for a transaction the caller's gateway holds and 3 for one it does not, and
// a remote file with cells is checked like the own cell's servers.
func TestCrossCellRemoteFile(t *testing.T) {
	a, _, mxKey, _, _ := cellsHelper(t)
	c, err := loadReceivingRemote()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if code, _, err := c.call(ctx, []string{"held", "nothing-here"}, nil); err != nil || code != 3 {
		t.Fatalf("held, nothing bound: %d %v", code, err)
	}
	if code := helperBind("tx-held", a.masked, probeSource); code != 0 {
		t.Fatal(code)
	}
	if code, _, err := c.call(ctx, []string{"held", "tx-held"}, nil); err != nil || code != 0 {
		t.Fatalf("held, bound: %d %v", code, err)
	}
	cell := func(servers []string, pins []string) map[string]any {
		return map[string]any{"servers": servers, "server_pins": pins}
	}
	good := cell([]string{"10.0.0.2:4650"}, []string{a.pin})
	for name, file := range map[string]map[string]any{
		"cells without relay_domain": {"cells": map[string]any{"cell-b": good}},
		"relay_domain without cells": {"relay_domain": "example.test"},
		"bad relay_domain":           {"relay_domain": "Example.Test", "cells": map[string]any{"cell-b": good}},
		"bad cell slug":              {"relay_domain": "example.test", "cells": map[string]any{"Cell-B": good}},
		"public server":              {"relay_domain": "example.test", "cells": map[string]any{"cell-b": cell([]string{"203.0.113.5:4650"}, []string{a.pin})}},
		"no pins":                    {"relay_domain": "example.test", "cells": map[string]any{"cell-b": cell([]string{"10.0.0.2:4650"}, nil)}},
		"bad pin":                    {"relay_domain": "example.test", "cells": map[string]any{"cell-b": cell([]string{"10.0.0.2:4650"}, []string{"zz"})}},
		"unknown field":              {"relay_domain": "example.test", "cells": map[string]any{"cell-b": map[string]any{"servers": []string{"10.0.0.2:4650"}, "server_pins": []string{a.pin}, "x": 1}}},
		"null cell":                  {"relay_domain": "example.test", "cells": map[string]any{"cell-b": nil}},
	} {
		file["key"], file["servers"], file["server_pins"] = mxKey, []string{a.addr}, []string{a.pin}
		raw, _ := json.Marshal(file)
		path := filepath.Join(t.TempDir(), "remote.json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("KYPOST_RECEIVING_REMOTE", path)
		if _, err := loadReceivingRemote(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
