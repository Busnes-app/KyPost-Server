package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Commands hand bind and accept to a running server and keep their exit
// codes; without one they run in process.
func TestReceivingServerAnswersForwardedCommands(t *testing.T) {
	r, _ := receivingFixture(t)
	quarantineCLI(t, r)
	ln, err := listenReceiving(receivingSocket())
	if err != nil {
		t.Fatal(err)
	}
	var served atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			served.Add(1)
			r.serveConn(conn)
		}
	}()
	var once sync.Once
	stop := func() { once.Do(func() { _ = ln.Close(); <-done }) }
	t.Cleanup(stop)

	// The fixture's DNS proof answers only in the server's runtime, so these
	// succeed only if the server ran them.
	const sender = "sender@outside.test"
	if err := runReceivingCommand([]string{"bind", "forwarded", sender, "one@example.test"}, nil); err != nil {
		t.Fatal("forwarded bind:", err)
	}
	raw := []byte("From: sender@outside.test\r\nTo: one@example.test\r\nSubject: forwarded\r\n\r\nbody\r\n")
	if err := runReceivingCommand([]string{"accept", "forwarded", sender}, bytes.NewReader(raw)); err != nil {
		t.Fatal("forwarded accept:", err)
	}
	d, err := r.holding.Get(context.Background(), receivingGateway, "forwarded")
	if err != nil || d.State != "pending" || !bytes.Equal(d.Raw, raw) {
		t.Fatalf("held delivery %q %v, want the exact message pending", d.State, err)
	}
	var refused *receivingCommandError
	if err := runReceivingCommand([]string{"bind", "unrouted", sender, "nobody@example.test"}, nil); !errors.As(err, &refused) || refused.ExitCode() != 3 {
		t.Fatal("forwarded refusal lost its exit code:", err)
	}
	if n := served.Load(); n != 3 {
		t.Fatalf("server answered %d commands, want 3", n)
	}

	stop()
	if _, err := os.Lstat(receivingSocket()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("socket left behind:", err)
	}
	// In process the real DNS proof fails, so the command refuses by itself.
	if err := runReceivingCommand([]string{"bind", "in-process", sender, "one@example.test"}, nil); err == nil {
		t.Fatal("in-process bind used the fixture's DNS proof")
	}
	if n := served.Load(); n != 3 {
		t.Fatal("a command reached a stopped server")
	}
}

// A panicking command answers a temporary refusal and leaves the server up.
func TestReceivingServerPanicIsATemporaryRefusal(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	conns := make([]*net.UnixConn, 2)
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "socketpair")
		c, err := net.FileConn(f)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = c.(*net.UnixConn)
	}
	defer conns[0].Close()
	served := make(chan struct{})
	go func() {
		defer close(served)
		(&receivingRuntime{}).serveConn(conns[1]) // no holding store: run panics
	}()
	if _, err := conns[0].Write([]byte(`{"v":2,"args":["bind","id","sender@outside.test","one@example.test"],"deadline":0,"length":0}` + "\n")); err != nil {
		t.Fatal(err)
	}
	_ = conns[0].CloseWrite()
	var code [1]byte
	if _, err := conns[0].Read(code[:]); err != nil || code[0] != 1 {
		t.Fatalf("answer %v %v, want exit 1", code, err)
	}
	<-served
}

// A message is committed only when exactly its declared bytes arrive: a helper
// killed mid-write closes the connection the same way a finished one does.
func TestReceivingServerRefusesATruncatedMessage(t *testing.T) {
	r, _ := receivingFixture(t)
	ctx := context.Background()
	const sender = "sender@outside.test"
	raw := []byte("From: sender@outside.test\r\nTo: one@example.test\r\nSubject: whole\r\n\r\nbody\r\n")
	send := func(id string, length int, body []byte) byte {
		t.Helper()
		if err := r.bind(ctx, id, sender, "one@example.test"); err != nil {
			t.Fatal(err)
		}
		client, server := socketPair(t)
		defer client.Close()
		go r.serveConn(server)
		header, err := json.Marshal(receivingRequest{V: receivingProtocol, Args: []string{"accept", id, sender}, Deadline: time.Now().Add(10 * time.Second).UnixNano(), Length: int64(length)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Write(append(append(header, '\n'), body...)); err != nil {
			t.Fatal(err)
		}
		_ = client.CloseWrite()
		var code [1]byte
		if _, err := io.ReadFull(client, code[:]); err != nil {
			t.Fatal(err)
		}
		return code[0]
	}
	for name, c := range map[string]struct {
		length int
		body   []byte
	}{
		"short": {len(raw), raw[:len(raw)/2]},
		"long":  {len(raw) / 2, raw},
		"empty": {0, nil},
	} {
		if code := send(name, c.length, c.body); code != 1 {
			t.Fatalf("%s: answered %d, want a temporary refusal", name, code)
		}
		if d, err := r.holding.Get(ctx, receivingGateway, name); err != nil || d.State == "pending" || len(d.Raw) != 0 {
			t.Fatalf("%s: held %q with %d bytes (%v), want nothing committed", name, d.State, len(d.Raw), err)
		}
	}
	if code := send("whole", len(raw), raw); code != 0 {
		t.Fatalf("whole message answered %d", code)
	}
	if d, err := r.holding.Get(ctx, receivingGateway, "whole"); err != nil || d.State != "pending" || !bytes.Equal(d.Raw, raw) {
		t.Fatalf("whole message held %q (%v)", d.State, err)
	}
}

func socketPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	conns := make([]*net.UnixConn, 2)
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "socketpair")
		c, err := net.FileConn(f)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = c.(*net.UnixConn)
	}
	return conns[0], conns[1]
}

func TestReceivingServerRefusesALiveSocket(t *testing.T) {
	path := t.TempDir() + "/s.sock"
	ln, err := listenReceiving(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, err := listenReceiving(path); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatal("second server took a live socket:", err)
	}
}
