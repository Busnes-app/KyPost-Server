package app

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/mldsa"
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
	"maps"
	"math/big"
	mathrand "math/rand/v2"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/Busnes-app/ky-primitives/pqtls"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
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

// validateReceivingKey accepts an ML-DSA-65 private key (default for new keys) or a
// legacy Ed25519 private key (retained for rolling re-pin migrations).
func validateReceivingKey(parsed any) (crypto.Signer, bool) {
	switch k := parsed.(type) {
	case *mldsa.PrivateKey:
		if k == nil || k.PublicKey().Parameters() != mldsa.MLDSA65() {
			return nil, false
		}
		return k, true
	case ed25519.PrivateKey:
		if len(k) != ed25519.PrivateKeySize {
			return nil, false
		}
		return k, true
	default:
		return nil, false
	}
}

func keyPin(key crypto.Signer) string {
	der, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		panic(err) // validated ML-DSA-65 and Ed25519 public keys always marshal
	}
	return spkiPin(der)
}

func encodeReceivingKey(key crypto.Signer) ([]byte, error) {
	if _, ok := validateReceivingKey(key); !ok {
		return nil, errors.New("receiving transport key must be ML-DSA-65 or legacy Ed25519")
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// readReceivingKey reads an owner-only ML-DSA-65 (or legacy Ed25519) key file written by
// encodeReceivingKey.
func readReceivingKey(path string) (crypto.Signer, error) {
	bad := errors.New("receiving transport key must be an owner-only ML-DSA-65 or Ed25519 PKCS #8 PEM file")
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
	if err != nil {
		return nil, bad
	}
	key, ok := validateReceivingKey(parsed)
	if !ok {
		return nil, bad
	}
	return key, nil
}

// receivingCertificate wraps key in a self-signed certificate. Peers check only its pin,
// never its names, dates or issuer, so the certificate is built in memory and never stored.
func receivingCertificate(key crypto.Signer) (tls.Certificate, error) {
	if _, ok := validateReceivingKey(key); !ok {
		return tls.Certificate{}, errors.New("receiving transport key must be ML-DSA-65 or legacy Ed25519")
	}
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
// first replica to start creates an ML-DSA-65 key exclusively; existing ML-DSA-65 or
// legacy Ed25519 keys are loaded as-is so upgrades remain zero-downtime until re-pinned.
func receivingServerKey() (crypto.Signer, error) {
	path := filepath.Join(config.SecretDir(), receivingServerKeyFile)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return readReceivingKey(path)
	}
	if err := cryptutil.RefuseCreate(path); err != nil {
		return nil, err
	}
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
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
		key, err := mldsa.GenerateKey(mldsa.MLDSA65())
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
	releaseDocuments, err := startDocuments(ctx, config.ConfigDir(), config.StateDir(), config.SecretDir(), config.TenancyMulti)
	if err != nil {
		return err
	}
	defer releaseDocuments()
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
	conn := tls.Server(raw, pqtls.HybridOnly(&tls.Config{
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
	}))
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

// receivingRemote is an MX host's KYPOST_RECEIVING_REMOTE file: its own key, its own cell's
// replicas (private addresses and server pins, current and next) and, on a host that
// forwards relay-domain mail across regions (MH10), the hosted relay domain and the other
// cells' replicas by cell.
type receivingRemote struct {
	Key        string   `json:"key"`
	Servers    []string `json:"servers"`
	ServerPins []string `json:"server_pins"`
	tls        *tls.Config

	RelayDomain string                         `json:"relay_domain,omitempty"`
	Cells       map[string]*receivingCellRoute `json:"cells,omitempty"`
}

// receivingCellRoute is one other cell's replicas, reached with the same key; each cell
// enrols that key as a gateway of its own (RX R4 per cell).
type receivingCellRoute struct {
	Servers    []string `json:"servers"`
	ServerPins []string `json:"server_pins"`
	tls        *tls.Config
}

// receivingEndpoint is where one call goes: a cell's replicas and its pinned TLS.
type receivingEndpoint struct {
	servers []string
	tls     *tls.Config
}

// relayCellFor is the masked relay region code table (tenancy.RelayCell); tests replace it.
var relayCellFor = tenancy.RelayCell

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
	bad := errors.New("KYPOST_RECEIVING_REMOTE must name an owner-only JSON file with key, 1-16 private servers and 1-4 server_pins, and for other cells a relay_domain and cells of the same shape")
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
	if dec.Decode(&c) != nil || !validRemoteRoute(c.Servers, c.ServerPins) || (c.RelayDomain == "") != (len(c.Cells) == 0) ||
		c.RelayDomain != "" && checkReceivingHost(c.RelayDomain) != nil || len(c.Cells) >= config.MaxCells {
		return nil, bad
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, bad
	}
	for cell, route := range c.Cells {
		if cells, err := config.Cells(cell); err != nil || len(cells) != 1 || cells[0] != cell || route == nil || !validRemoteRoute(route.Servers, route.ServerPins) {
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
	for _, route := range c.Cells {
		route.tls = pinVerifiedClientTLS(cert, route.ServerPins)
	}
	return &c, nil
}

// validRemoteRoute is one cell's replicas: 1-16 private addresses and 1-4 server pins.
func validRemoteRoute(servers, pins []string) bool {
	if len(servers) < 1 || len(servers) > 16 || len(pins) < 1 || len(pins) > 4 {
		return false
	}
	for _, s := range servers {
		if _, ok := privateAddress(s); !ok {
			return false
		}
	}
	for _, p := range pins {
		if !tenancy.ValidPin(p) {
			return false
		}
	}
	return true
}

func (c *receivingRemote) own() receivingEndpoint { return receivingEndpoint{c.Servers, c.tls} }

// route is the cell a recipient's bind goes to (MH10): on the relay domain, the cell whose
// region code is the character after adjective.animal., read without a +tag (sso.PlusBase,
// as bind resolves it); everything else, and a code that names no cell this host forwards
// to, goes to this host's own cell, which answers it. The bind still carries the recipient
// as received.
func (c *receivingRemote) route(recipient string) receivingEndpoint {
	recipient = sso.PlusBase(recipient)
	at := strings.LastIndexByte(recipient, '@')
	if len(c.Cells) == 0 || at < 0 || !strings.EqualFold(recipient[at+1:], c.RelayDomain) {
		return c.own()
	}
	local := strings.ToLower(recipient[:at])
	random := local[strings.LastIndexByte(local, '.')+1:]
	if strings.Count(local, ".") != 2 || len(random) != 7 || strings.Trim(random, tenancy.RelayAlphabet) != "" {
		return c.own()
	}
	if cell, ok := relayCellFor(random[0]); ok {
		if r, ok := c.Cells[cell]; ok {
			return receivingEndpoint{r.Servers, r.tls}
		}
	}
	return c.own()
}

// pinVerifiedClientTLS is the helper's TLS: no CA and no name, because the server's key
// must instead match one of pins (RX R3). Go calls VerifyConnection even when chain
// verification is skipped, and a handshake whose server key is not pinned fails.
func pinVerifiedClientTLS(cert tls.Certificate, pins []string) *tls.Config {
	return pqtls.HybridOnly(&tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, //nolint:gosec // replaced by the pin check in VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !slices.Contains(pins, spkiPin(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)) {
				return errors.New("receiving server key is not pinned")
			}
			return nil
		},
	})
}

// call sends one request to a replica. It tries the replicas in random order, each with
// one second to connect and authenticate, but only before sending: once the header is
// written, a lost answer is an error, never a retry. The server's budget leaves five
// seconds of ctx for its answer to arrive.
func (c *receivingRemote) call(ctx context.Context, args []string, body []byte) (byte, []byte, error) {
	return c.own().call(ctx, args, body)
}

func (e receivingEndpoint) call(ctx context.Context, args []string, body []byte) (byte, []byte, error) {
	lost := errors.New("receiving server did not answer")
	deadline, ok := ctx.Deadline()
	if !ok {
		return 1, nil, lost
	}
	for _, i := range mathrand.Perm(len(e.servers)) {
		dial, cancel := context.WithTimeout(ctx, time.Second)
		conn, err := (&tls.Dialer{Config: e.tls}).DialContext(dial, "tcp", e.servers[i])
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
	var code byte
	var err error
	switch {
	case args[0] == "bind":
		code, _, err = c.route(args[3]).call(ctx, args, body)
	case len(c.Cells) > 0:
		code, err = c.acceptAcross(ctx, args, body)
	default:
		code, _, err = c.call(ctx, args, body)
	}
	switch {
	case err != nil:
		return &receivingCommandError{err: err, code: 1}
	case code != 0:
		return &receivingCommandError{err: errors.New("refused by the receiving server"), code: int(code)}
	}
	return nil
}

// acceptAcross completes one message in every cell that bound a recipient (MH10). It asks
// each cell whether it holds the transaction (held, no body) and sends the message only to
// those, so no cell receives mail it has no recipient for. The answer is 250 only when every
// holder committed. A cell that cannot answer, or holders that disagree, answer 451: the
// sender retries, and a cell that committed keeps a second copy (at least once, as RX's lost
// answer already is). If no cell holds it, the own cell answers as it always has.
func (c *receivingRemote) acceptAcross(ctx context.Context, args []string, body []byte) (byte, error) {
	targets := []receivingEndpoint{c.own()}
	for _, cell := range slices.Sorted(maps.Keys(c.Cells)) {
		targets = append(targets, receivingEndpoint{c.Cells[cell].Servers, c.Cells[cell].tls})
	}
	var holders []receivingEndpoint
	for _, t := range targets {
		code, _, err := t.call(ctx, []string{"held", args[1]}, nil)
		switch {
		case err != nil:
			return 1, err
		case code == 0:
			holders = append(holders, t)
		case code != 3:
			return 1, errors.New("a receiving cell could not say whether it holds the message")
		}
	}
	if len(holders) == 0 {
		holders = targets[:1]
	}
	var first byte
	for i, h := range holders {
		code, _, err := h.call(ctx, args, body)
		if err != nil {
			code = 1
		}
		if i == 0 {
			first = code
		} else if code != first {
			return 1, errors.New("receiving cells answered the message differently")
		}
	}
	return first, nil
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
