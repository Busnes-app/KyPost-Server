package dbbackend

import (
	"context"
	"errors"
	"testing"
)

// Off loopback, table mode needs sslmode=verify-full; loopback and Unix sockets are exempt.
func TestTableTransportNeedsVerifyFull(t *testing.T) {
	t.Setenv("PGSSLMODE", "")
	for raw, refused := range map[string]bool{
		"postgres://u@db.internal/kypost":                           true,
		"postgres://u@db.internal/kypost?sslmode=disable":           true,
		"postgres://u@db.internal/kypost?sslmode=require":           true,
		"postgres://u@db.internal/kypost?sslmode=verify-ca":         true,
		"postgres://u@db.internal/kypost?sslmode=verify-full":       false,
		"postgres://u@127.0.0.1/kypost?sslmode=disable":             false,
		"postgres://u@localhost/kypost":                             false,
		"postgres://u@[::1]/kypost":                                 false,
		"postgres:///kypost?host=/var/run/postgresql":               false,
		"postgres://u@127.0.0.1,db.internal/kypost?sslmode=disable": true,
	} {
		_, err := checkTransportURL(raw)
		if refused != errors.Is(err, ErrTransport) {
			t.Errorf("%s: refused=%v, err=%v", raw, refused, err)
		}
	}
	t.Setenv("PGSSLMODE", "verify-full")
	if _, err := checkTransportURL("postgres://u@db.internal/kypost"); err != nil {
		t.Errorf("PGSSLMODE=verify-full: %v", err)
	}
}

// The multi profile drops a dead connection within the bounds; unset or loose bounds refuse.
func TestDeadConnectionBounds(t *testing.T) {
	for _, c := range []struct {
		idle, interval, count, user int
		ok                          bool
	}{
		{10, 5, 4, 30000, true},
		{7200, 75, 9, 30000, false}, // the Linux defaults
		{0, 5, 4, 30000, false},
		{10, 5, 4, 0, false}, // retransmission timeouts, about 15 minutes
		{10, 5, 4, 30001, false},
		{30, 10, 4, 30000, false},
	} {
		if err := deadConnectionBounds(c.idle, c.interval, c.count, c.user); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

// The bounds are read from the server for this session, as a replica's connection sees them.
func TestSessionBoundsReadTheServer(t *testing.T) {
	RequirePostgres(t)
	ctx := context.Background()
	db, err := root(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	set := func(q string) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{`SET tcp_keepalives_idle=10`, `SET tcp_keepalives_interval=5`, `SET tcp_keepalives_count=4`, `SET tcp_user_timeout=30000`} {
		set(q)
	}
	if err := sessionBounds(ctx, conn); err != nil {
		t.Fatal("the multi profile's settings:", err)
	}
	set(`SET tcp_user_timeout=0`)
	if err := sessionBounds(ctx, conn); !errors.Is(err, ErrTransport) {
		t.Fatal("no tcp_user_timeout:", err)
	}
}
