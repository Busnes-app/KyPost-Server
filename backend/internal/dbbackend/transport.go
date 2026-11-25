package dbbackend

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend/pgdialect"
)

// Dead-connection bounds for a replica on another host (MH7): an idle socket is dropped
// after keepalives_idle + interval*count seconds, a socket with unacknowledged data after
// tcp_user_timeout. Until then PostgreSQL keeps a partitioned holder's advisory locks.
const (
	maxKeepaliveSeconds = 60
	maxUserTimeoutMS    = 30000
)

// ErrTransport refuses a table-mode database connection that can cross hosts unencrypted
// or hold a dead replica's locks for hours.
var ErrTransport = errors.New("table mode needs a protected database connection")

// CheckTableTransport refuses, unless every database host is loopback or a Unix socket, a
// URL whose sslmode is not verify-full and a session whose dead-connection bounds exceed
// the multi profile's. Errors never carry the URL.
func CheckTableTransport(ctx context.Context) error {
	s := current()
	if s.err != nil {
		return s.err
	}
	if s.url == "" {
		return fmt.Errorf("%w: table mode runs on PostgreSQL only", ErrTransport)
	}
	local, err := checkTransportURL(s.url)
	if err != nil || local {
		return err
	}
	db, err := root(ctx)
	if err != nil {
		return err
	}
	return scrubbed(sessionBounds(ctx, db), s.url)
}

// checkTransportURL reports a loopback-only URL, and otherwise refuses one without
// sslmode=verify-full.
func checkTransportURL(raw string) (local bool, err error) {
	if local, err = localHosts(raw); err != nil || local {
		return local, err
	}
	if mode := sslMode(raw); mode != "verify-full" {
		return false, fmt.Errorf("%w: the database URL must set sslmode=verify-full off loopback (it is %q)", ErrTransport, mode)
	}
	return false, nil
}

// sessionBounds reads the server's socket settings for this session.
func sessionBounds(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) error {
	var idle, interval, count, user int
	if err := q.QueryRowContext(ctx, `SELECT current_setting('tcp_keepalives_idle')::int, current_setting('tcp_keepalives_interval')::int, current_setting('tcp_keepalives_count')::int, current_setting('tcp_user_timeout')::int`).Scan(&idle, &interval, &count, &user); err != nil {
		return fmt.Errorf("reading the connection's keepalive settings: %w", err)
	}
	return deadConnectionBounds(idle, interval, count, user)
}

func scrubbed(err error, raw string) error {
	if err == nil || errors.Is(err, ErrTransport) {
		return err
	}
	return scrub(err, raw)
}

// deadConnectionBounds refuses an unset (0, the kernel's hours-long default) or loose bound.
func deadConnectionBounds(idle, interval, count, userMS int) error {
	if idle <= 0 || interval <= 0 || count <= 0 || idle+interval*count > maxKeepaliveSeconds {
		return fmt.Errorf("%w: set tcp_keepalives_idle, tcp_keepalives_interval and tcp_keepalives_count to drop an idle dead connection within %d s (now %d + %d x %d)", ErrTransport, maxKeepaliveSeconds, idle, interval, count)
	}
	if userMS <= 0 || userMS > maxUserTimeoutMS {
		return fmt.Errorf("%w: set tcp_user_timeout to at most %d ms (now %d)", ErrTransport, maxUserTimeoutMS, userMS)
	}
	return nil
}

// localHosts reports whether every host the URL can connect to is loopback or a socket.
func localHosts(raw string) (bool, error) {
	cfg, err := pgdialect.Config(raw)
	if err != nil {
		return false, err
	}
	hosts := []string{cfg.Host}
	for _, f := range cfg.Fallbacks {
		hosts = append(hosts, f.Host)
	}
	for _, h := range hosts {
		if !strings.HasPrefix(h, "/") && !loopback(h) {
			return false, nil
		}
	}
	return true, nil
}

func loopback(host string) bool {
	if h := strings.ToLower(host); h == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// sslMode is the URL's sslmode, else PGSSLMODE (pgx reads it), else pgx's "prefer".
func sslMode(raw string) string {
	u, err := url.Parse(raw)
	if err == nil && u.Query().Has("sslmode") {
		return u.Query().Get("sslmode")
	}
	if v := os.Getenv("PGSSLMODE"); v != "" {
		return v
	}
	return "prefer"
}
