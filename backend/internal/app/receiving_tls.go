package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	mathrand "math/rand/v2"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
)

// The hosted receiving tier's transport (docs/superpowers/specs/2026-11-21-hosted-receiving-tier-design.md,
// "The authenticated call" and "Keys"). Stateless MX hosts forward bind, accept and config
// to `receiving serve --tls` on the replicas over TLS 1.3 with pinned keys both ways. The
// gateway ID is the MX host's pin row, never the request. Multi-tenant installs only.

const (
	receivingRemoteProtocol = 3
	receivingServerKeyFile  = "receiving-tls.key"
	receivingConfigBytes    = 64 << 10
)

// receivingRemoteRequest is protocol 3: budget_ms, the time left, replaces protocol 2's
// absolute deadline because hosts do not share a clock.
type receivingRemoteRequest struct {
	V        int      `json:"v"`
	Args     []string `json:"args"`
	BudgetMS int64    `json:"budget_ms"`
	Length   int64    `json:"length"`
}

var errReceivingSingle = errors.New("the hosted receiving transport needs a multi-tenant install (KYPOST_TENANCY=multi)")

// receivingMultiEnv refuses unless KYPOST_TENANCY=multi. An MX host has no state, so this
// is all it can check.
func receivingMultiEnv() error {
	mode, _, err := config.Tenancy()
	if err != nil {
		return err
	}
	if mode != config.TenancyMulti {
		return errReceivingSingle
	}
	return nil
}

// receivingMultiStore opens tenancy.db on a replica whose install records multi mode with
// this process's staff org (K1). A single-tenant install has neither.
func receivingMultiStore() (*sql.DB, error) {
	mode, staff, err := config.Tenancy()
	if err != nil {
		return nil, err
	}
	if mode != config.TenancyMulti {
		return nil, errReceivingSingle
	}
	st, err := state.OpenReadOnly(config.StateDir())
	if err != nil {
		return nil, err
	}
	recMode, recStaff, err := st.TenancyRecord()
	_ = st.Close()
	if err != nil {
		return nil, err
	}
	if recMode != config.TenancyMulti || recStaff != staff {
		return nil, errReceivingSingle
	}
	return tenancy.Open(config.StateDir(), false)
}

// spkiPin is a key's pin: SHA-256 of its DER SubjectPublicKeyInfo, lowercase hex.
func spkiPin(spki []byte) string {
	sum := sha256.Sum256(spki)
	return hex.EncodeToString(sum[:])
}

func keyPin(key ed25519.PrivateKey) string {
	der, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		panic(err) // an Ed25519 public key always marshals
	}
	return spkiPin(der)
}

func encodeReceivingKey(key ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// readReceivingKey reads an owner-only Ed25519 key file written by encodeReceivingKey.
func readReceivingKey(path string) (ed25519.PrivateKey, error) {
	bad := errors.New("receiving transport key must be an owner-only Ed25519 PKCS #8 PEM file")
	if !filepath.IsAbs(path) {
		return nil, bad
	}
	raw, err := receivingTLSFile(path, true)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, bad
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(ed25519.PrivateKey)
	if err != nil || !ok {
		return nil, bad
	}
	return key, nil
}

// receivingCertificate wraps key in a self-signed certificate. Peers check only its pin,
// never its names, dates or issuer, so the certificate is built in memory and never stored.
func receivingCertificate(key ed25519.PrivateKey) (tls.Certificate, error) {
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "kypost-receiving"},
		NotBefore:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:     time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// receivingServerKey is the replicas' one listener key in the shared SECRET_DIR. The
// first replica to start creates it exclusively; the others read the winner's.
func receivingServerKey() (ed25519.PrivateKey, error) {
	path := filepath.Join(config.SecretDir(), receivingServerKeyFile)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return readReceivingKey(path)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	encoded, err := encodeReceivingKey(key)
	if err != nil {
		return nil, err
	}
	if _, err := fsutil.CreateExclusive(path, encoded, 0o600); err != nil {
		return nil, err
	}
	return readReceivingKey(path)
}

// privateAddress accepts a literal private-network or loopback IP with a nonzero port.
func privateAddress(v string) (netip.AddrPort, bool) {
	a, err := netip.ParseAddrPort(v)
	ip := a.Addr()
	return a, err == nil && a.Port() != 0 && ip.Zone() == "" && (ip.IsPrivate() || ip.IsLoopback())
}

func runReceivingGateway(args []string, output io.Writer) (result error) {
	usage := errors.New("usage: receiving gateway keygen <key-file> | add <id> <spki-sha256> | revoke <id> <spki-sha256> | retire <id> | server-pin")
	if len(args) == 0 {
		return usage
	}
	switch {
	case args[0] == "keygen" && len(args) == 2:
		// On the MX host: the private key never leaves it.
		if err := receivingMultiEnv(); err != nil {
			return err
		}
		if !filepath.IsAbs(args[1]) {
			return errors.New("receiving gateway keygen needs an absolute key file path")
		}
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		encoded, err := encodeReceivingKey(key)
		if err != nil {
			return err
		}
		created, err := fsutil.CreateExclusive(args[1], encoded, 0o600)
		if err != nil {
			return err
		}
		if !created {
			return errors.New("the key file already exists; choose a new path")
		}
		_, err = fmt.Fprintln(output, keyPin(key))
		return err
	case args[0] == "server-pin" && len(args) == 1:
		db, err := receivingMultiStore()
		if err != nil {
			return err
		}
		_ = db.Close()
		key, err := receivingServerKey()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, keyPin(key))
		return err
	case (args[0] == "add" || args[0] == "revoke") && len(args) == 3, args[0] == "retire" && len(args) == 2:
	default:
		return usage
	}
	id, pin := args[1], ""
	if len(args) == 3 {
		pin = args[2]
	}
	// Checked before the audit line, so it names only a valid ID and a pin's prefix.
	if !tenancy.ValidGatewayID(id) {
		return tenancy.ErrGatewayID
	}
	if len(args) == 3 && !tenancy.ValidPin(pin) {
		return tenancy.ErrGatewaySPKI
	}
	defer func() {
		status := "changed"
		if result != nil {
			status = "refused"
		}
		slog.Info("receiving gateway", "actor", "operator", "task_id", "native-receiving", "action", args[0], "target", id, "result", status, "correlation_id", pin[:min(len(pin), 16)])
	}()
	db, err := receivingMultiStore()
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := time.Now()
	switch args[0] {
	case "add":
		expires, err := tenancy.AddGateway(ctx, db, id, pin, now)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "enrolled %s until %s\n", id, expires.UTC().Format(time.DateOnly))
		return err
	case "revoke":
		return tenancy.RevokeGateway(ctx, db, id, pin, now)
	}
	return tenancy.RetireGateway(ctx, db, id, now)
}

// runReceivingServeTLS answers MX hosts on a private address until SIGTERM.
func runReceivingServeTLS(address string) error {
	db, err := receivingMultiStore()
	if err != nil {
		return err
	}
	defer db.Close()
	addr, ok := privateAddress(address)
	if !ok {
		return errors.New("receiving serve --tls needs a literal private-network IP and nonzero port")
	}
	key, err := receivingServerKey()
	if err != nil {
		return err
	}
	cert, err := receivingCertificate(key)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	open, cancel := context.WithTimeout(ctx, 30*time.Second)
	r, err := openReceivingRuntime(open, false)
	cancel()
	if err != nil {
		return err
	}
	defer r.close()
	ln, err := net.Listen("tcp", addr.String())
	if err != nil {
		return err
	}
	slog.Info("receiving server listening", "actor", "receiving-tls", "task_id", "native-receiving")
	return serveReceiving(ctx, ln, func(conn net.Conn) { r.serveTLS(conn, db, cert) })
}

// serveTLS authenticates one MX host by its pin, then answers one command under that
// pin's gateway ID. A key without a live pin is refused in the handshake, before any
// frame is read.
func (r *receivingRuntime) serveTLS(raw net.Conn, db *sql.DB, cert tls.Certificate) {
	var gateway string
	conn := tls.Server(raw, &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAnyClientCert,
		// No resumption: every connection presents its key and meets a fresh pin check.
		SessionTicketsDisabled: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return tenancy.ErrGatewayUnpinned
			}
			// Re-read for every connection, so a revocation applies to the next command.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			id, expires, err := tenancy.GatewayFor(ctx, db, spkiPin(cs.PeerCertificates[0].RawSubjectPublicKeyInfo), time.Now())
			if err != nil {
				return err
			}
			if time.Until(expires) < tenancy.PinWarning {
				slog.Warn("receiving gateway pin expires soon; enrol the next key", "actor", id, "task_id", "native-receiving", "until_ms", expires.UnixMilli())
			}
			gateway = id
			return nil
		},
	})
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.HandshakeContext(ctx); err != nil {
		slog.Warn("receiving connection refused before any request", "actor", "receiving-tls", "task_id", "native-receiving", "result", "refused")
		return
	}
	session := *r
	session.gateway = gateway
	session.answer(conn, receivingRemoteProtocol)
}

// configReply is the config command's answer: the destination domains, never addresses.
func (r *receivingRuntime) configReply(ctx context.Context) (byte, []byte) {
	domains, err := r.destinations(ctx)
	result := "served"
	if err != nil {
		result = "refused"
	}
	slog.Info("receiving configuration", "actor", r.gatewayID(), "task_id", "native-receiving", "action", "config", "result", result)
	if err != nil {
		return 1, nil
	}
	reply, err := json.Marshal(receivingConfigReply{Destinations: domains})
	if err != nil || len(reply) > receivingConfigBytes {
		return 1, nil
	}
	return 0, reply
}

type receivingConfigReply struct {
	Destinations []string `json:"destinations"`
}

// receivingRemote is an MX host's KYPOST_RECEIVING_REMOTE file: its own key, the replicas'
// private addresses and the replicas' server pins (current and next).
type receivingRemote struct {
	Key        string   `json:"key"`
	Servers    []string `json:"servers"`
	ServerPins []string `json:"server_pins"`
	tls        *tls.Config
}

// loadReceivingRemote reads KYPOST_RECEIVING_REMOTE. Unset is nil: the command runs as it
// always has. Set, it must be valid on a multi install, and there is no local fallback.
func loadReceivingRemote() (*receivingRemote, error) {
	path := os.Getenv("KYPOST_RECEIVING_REMOTE")
	if path == "" {
		return nil, nil
	}
	if err := receivingMultiEnv(); err != nil {
		return nil, err
	}
	bad := errors.New("KYPOST_RECEIVING_REMOTE must name an owner-only JSON file with key, 1-16 private servers and 1-4 server_pins")
	if !filepath.IsAbs(path) {
		return nil, bad
	}
	raw, err := receivingTLSFile(path, true)
	if err != nil {
		return nil, err
	}
	var c receivingRemote
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&c) != nil || len(c.Servers) < 1 || len(c.Servers) > 16 || len(c.ServerPins) < 1 || len(c.ServerPins) > 4 {
		return nil, bad
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, bad
	}
	for _, s := range c.Servers {
		if _, ok := privateAddress(s); !ok {
			return nil, bad
		}
	}
	for _, p := range c.ServerPins {
		if !tenancy.ValidPin(p) {
			return nil, bad
		}
	}
	key, err := readReceivingKey(c.Key)
	if err != nil {
		return nil, err
	}
	cert, err := receivingCertificate(key)
	if err != nil {
		return nil, err
	}
	c.tls = pinVerifiedClientTLS(cert, c.ServerPins)
	return &c, nil
}

// pinVerifiedClientTLS is the helper's TLS: no CA and no name, because the server's key
// must instead match one of pins (RX R3). Go calls VerifyConnection even when chain
// verification is skipped, and a handshake whose server key is not pinned fails.
func pinVerifiedClientTLS(cert tls.Certificate, pins []string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, //nolint:gosec // replaced by the pin check in VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !slices.Contains(pins, spkiPin(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)) {
				return errors.New("receiving server key is not pinned")
			}
			return nil
		},
	}
}

// call sends one request to a replica. It tries the replicas in random order, each with
// one second to connect and authenticate, but only before sending: once the header is
// written, a lost answer is an error, never a retry. The server's budget leaves five
// seconds of ctx for its answer to arrive.
func (c *receivingRemote) call(ctx context.Context, args []string, body []byte) (byte, []byte, error) {
	lost := errors.New("receiving server did not answer")
	deadline, ok := ctx.Deadline()
	if !ok {
		return 1, nil, lost
	}
	for _, i := range mathrand.Perm(len(c.Servers)) {
		dial, cancel := context.WithTimeout(ctx, time.Second)
		conn, err := (&tls.Dialer{Config: c.tls}).DialContext(dial, "tcp", c.Servers[i])
		cancel()
		if err != nil {
			continue
		}
		defer conn.Close()
		budget := time.Until(deadline) - 5*time.Second
		if budget < time.Millisecond || conn.SetDeadline(deadline) != nil {
			return 1, nil, lost
		}
		header, err := json.Marshal(receivingRemoteRequest{V: receivingRemoteProtocol, Args: args, BudgetMS: budget.Milliseconds(), Length: int64(len(body))})
		if err != nil {
			return 1, nil, lost
		}
		if _, err := conn.Write(append(append(header, '\n'), body...)); err != nil {
			return 1, nil, lost
		}
		if conn.(*tls.Conn).CloseWrite() != nil {
			return 1, nil, lost
		}
		var code [1]byte
		if _, err := io.ReadFull(conn, code[:]); err != nil {
			return 1, nil, lost
		}
		if args[0] != "config" || code[0] != 0 {
			return code[0], nil, nil
		}
		reply, err := io.ReadAll(io.LimitReader(conn, receivingConfigBytes+1))
		if err != nil || len(reply) > receivingConfigBytes {
			return 1, nil, lost
		}
		return 0, reply, nil
	}
	return 1, nil, errors.New("no receiving server accepted the connection")
}

// forward sends a checked bind or accept to a replica. An MX host holds no state, so
// init is refused and nothing ever runs locally.
func (c *receivingRemote) forward(ctx context.Context, args []string, input io.Reader) error {
	if args[0] == "init" {
		return &receivingCommandError{err: errors.New("receiving init runs on a replica, never on an MX host"), code: 1}
	}
	var body []byte
	if args[0] == "accept" {
		raw, err := readReceivingMessage(ctx, input)
		if err != nil {
			return &receivingCommandError{err: err, code: receivingExit(err)}
		}
		body = raw
	}
	code, _, err := c.call(ctx, args, body)
	switch {
	case err != nil:
		return &receivingCommandError{err: err, code: 1}
	case code != 0:
		return &receivingCommandError{err: errors.New("refused by the receiving server"), code: int(code)}
	}
	return nil
}

// destinations fetches the replicas' destination domains for the MX host's profile.
func (c *receivingRemote) destinations(ctx context.Context) ([]string, error) {
	code, raw, err := c.call(ctx, []string{"config"}, nil)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, errors.New("the receiving server refused the configuration request")
	}
	var reply receivingConfigReply
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&reply) != nil || len(reply.Destinations) == 0 {
		return nil, errors.New("the receiving server sent an invalid configuration")
	}
	for _, d := range reply.Destinations {
		if checkReceivingHost(d) != nil {
			return nil, errors.New("the receiving server sent an invalid destination domain")
		}
	}
	return reply.Destinations, nil
}
