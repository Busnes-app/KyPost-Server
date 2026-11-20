// Package pgtest holds test helpers for PostgreSQL outages: a TCP relay that can freeze.
package pgtest

import (
	"net"
	"net/url"
	"sync"
	"testing"
)

// Relay forwards loopback connections to a PostgreSQL server. Freeze stops forwarding bytes
// while keeping every connection open, as a hung database or network would; Thaw resumes.
type Relay struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	frozen bool
	thaw   chan struct{}
	conns  []net.Conn
}

// NewRelay listens on loopback and relays to target (host:port) until the test ends.
func NewRelay(t testing.TB, target string) *Relay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &Relay{ln: ln, target: target, thaw: make(chan struct{})}
	close(r.thaw)
	go r.accept()
	t.Cleanup(r.Close)
	return r
}

// Addr is the relay's host:port.
func (r *Relay) Addr() string { return r.ln.Addr().String() }

// URL rewrites a postgres:// URL's host to the relay.
func (r *Relay) URL(t testing.TB, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = r.Addr()
	return u.String()
}

// Target is the host:port of a postgres:// URL.
func Target(t testing.TB, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Port() == "" {
		return u.Hostname() + ":5432"
	}
	return u.Host
}

// Freeze stops forwarding until Thaw.
func (r *Relay) Freeze() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.frozen {
		r.frozen = true
		r.thaw = make(chan struct{})
	}
}

// Thaw resumes forwarding.
func (r *Relay) Thaw() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		r.frozen = false
		close(r.thaw)
	}
}

// Close stops listening and drops every relayed connection.
func (r *Relay) Close() {
	_ = r.ln.Close()
	r.Thaw()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conns {
		_ = c.Close()
	}
}

func (r *Relay) gate() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.thaw
}

func (r *Relay) accept() {
	for {
		in, err := r.ln.Accept()
		if err != nil {
			return
		}
		out, err := net.Dial("tcp", r.target)
		if err != nil {
			_ = in.Close()
			continue
		}
		r.mu.Lock()
		r.conns = append(r.conns, in, out)
		r.mu.Unlock()
		go r.pipe(in, out)
		go r.pipe(out, in)
	}
}

func (r *Relay) pipe(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			<-r.gate()
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
