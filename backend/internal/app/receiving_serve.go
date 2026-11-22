package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
)

// The SMTP engine starts `receiving bind` per recipient and `receiving
// accept` per message. Each would open the authority and decode every account
// for one message, so `receiving serve` keeps one runtime warm and the
// commands hand it their checked arguments and message. Without a server the
// commands run in process, unchanged.

const (
	receivingProtocol      = 2
	receivingServeHandlers = 32
	receivingHeaderBytes   = 64 << 10
)

type receivingRequest struct {
	V        int      `json:"v"`
	Args     []string `json:"args"`
	Deadline int64    `json:"deadline"` // Unix nanoseconds: the command's own 30-second budget
	// Length is the message's byte count. EOF alone cannot tell a finished
	// message from a helper killed mid-write, so the server requires exactly it.
	Length int64 `json:"length"`
}

func receivingSocket() string {
	return filepath.Join(config.StateDir(), "receiving", ingress.ServerSocketFile)
}

// forwardReceiving sends a checked bind or accept to the receiving server.
// served is false when no server answered the dial: the caller then runs the
// command itself, with *input replaced by the message already read. Once a
// request is sent, a lost answer is a temporary refusal, never a local retry.
func forwardReceiving(ctx context.Context, args []string, input *io.Reader) (err error, served bool) {
	var body []byte
	if args[0] == "accept" {
		raw, err := readReceivingMessage(ctx, *input)
		if err != nil {
			*input = errReader{err}
			return nil, false
		}
		body, *input = raw, bytes.NewReader(raw)
	}
	deadline, _ := ctx.Deadline()
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", receivingSocket())
	if err != nil {
		return nil, false
	}
	defer conn.Close()
	refused := &receivingCommandError{err: errors.New("receiving server did not answer"), code: 1}
	// The server holds the same deadline; the margin lets its answer arrive.
	if conn.SetDeadline(deadline.Add(5*time.Second)) != nil {
		return refused, true
	}
	header, err := json.Marshal(receivingRequest{V: receivingProtocol, Args: args, Deadline: deadline.UnixNano(), Length: int64(len(body))})
	if err != nil {
		return refused, true
	}
	if _, err := conn.Write(append(append(header, '\n'), body...)); err != nil {
		return refused, true
	}
	if conn.(*net.UnixConn).CloseWrite() != nil {
		return refused, true
	}
	var code [1]byte
	if _, err := io.ReadFull(conn, code[:]); err != nil {
		return refused, true
	}
	if code[0] == 0 {
		return nil, true
	}
	return &receivingCommandError{err: errors.New("refused by the receiving server"), code: int(code[0])}, true
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// runReceivingServe opens the receiving runtime once, as the daemon's import
// loop does, and answers forwarded commands until SIGTERM.
func runReceivingServe(args []string) error {
	if len(args) == 2 && args[0] == "--tls" {
		return runReceivingServeTLS(args[1])
	}
	if len(args) != 0 {
		return errors.New("usage: receiving serve")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	open, cancel := context.WithTimeout(ctx, 30*time.Second)
	r, err := openReceivingRuntime(open, false)
	cancel()
	if err != nil {
		return err
	}
	defer r.holding.Close()
	ln, err := listenReceiving(receivingSocket())
	if err != nil {
		return err
	}
	slog.Info("receiving server listening", "actor", receivingGateway, "task_id", "native-receiving")
	return serveReceiving(ctx, ln, func(conn net.Conn) { r.serveConn(conn.(*net.UnixConn)) })
}

// serveReceiving answers connections until ctx ends, at most receivingServeHandlers at once.
func serveReceiving(ctx context.Context, ln net.Listener, handle func(net.Conn)) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	slots := make(chan struct{}, receivingServeHandlers)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			handle(conn)
		})
	}
}

// listenReceiving binds path owner-only, replacing a stale socket but never a
// live server.
func listenReceiving(path string) (*net.UnixListener, error) {
	if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
		_ = conn.Close()
		return nil, errors.New("a receiving server is already listening")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	ln.SetUnlinkOnClose(true)
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// serveConn answers one command with its exit code: the bytes a helper would
// have read, the checks it would have made, under the same deadline.
func (r *receivingRuntime) serveConn(conn *net.UnixConn) {
	defer conn.Close()
	if !sameUser(conn) {
		return
	}
	r.answer(conn, receivingProtocol)
}

// answer reads one request of protocol (2 on the socket, 3 over TLS) and writes its exit
// code, then for a protocol 3 config its reply.
func (r *receivingRuntime) answer(conn net.Conn, protocol int) {
	code := byte(1)
	var reply []byte
	defer func() {
		if p := recover(); p != nil {
			slog.Error("receiving command panicked; answered as a temporary refusal", "actor", receivingGateway, "panic", fmt.Sprint(p))
			code, reply = 1, nil
		}
		_, _ = conn.Write(append([]byte{code}, reply...))
	}()
	limit := time.Now().Add(30 * time.Second)
	if conn.SetDeadline(limit.Add(time.Second)) != nil {
		return
	}
	in := bufio.NewReaderSize(conn, receivingHeaderBytes)
	line, err := in.ReadSlice('\n')
	if err != nil {
		return
	}
	args, deadline, length, ok := parseReceivingRequest(line, protocol, limit)
	if !ok {
		return
	}
	if args[0] == "config" {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		if _, ok := readReceivingBody(in, "bind", length); ok {
			code, reply = r.configReply(ctx)
		}
		return
	}
	if err := checkReceivingArgs(args); err != nil {
		code = byte(receivingExit(err))
		return
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	body, ok := readReceivingBody(in, args[0], length)
	if !ok {
		return // a short, long or misdeclared message is never committed
	}
	err = r.run(ctx, args, bytes.NewReader(body))
	code = byte(receivingExit(err))
	if err == nil {
		code = 0
	}
}

// parseReceivingRequest checks a header line. Protocol 2 carries an absolute deadline;
// protocol 3 carries budget_ms, the time left, because hosts do not share a clock, and
// adds config. The server caps every command at limit.
func parseReceivingRequest(line []byte, protocol int, limit time.Time) (args []string, deadline time.Time, length int64, ok bool) {
	if protocol == receivingProtocol {
		var req receivingRequest
		if json.Unmarshal(line, &req) != nil || req.V != receivingProtocol || len(req.Args) == 0 || (req.Args[0] != "bind" && req.Args[0] != "accept") {
			return nil, time.Time{}, 0, false
		}
		deadline = time.Unix(0, req.Deadline)
		if deadline.After(limit) {
			deadline = limit
		}
		return req.Args, deadline, req.Length, true
	}
	var req receivingRemoteRequest
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || req.V != receivingRemoteProtocol || req.BudgetMS < 1 || req.BudgetMS > 30_000 || len(req.Args) == 0 ||
		(req.Args[0] != "bind" && req.Args[0] != "accept" && (req.Args[0] != "config" || len(req.Args) != 1)) {
		return nil, time.Time{}, 0, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, time.Time{}, 0, false
	}
	deadline = time.Now().Add(time.Duration(req.BudgetMS) * time.Millisecond)
	if deadline.After(limit) {
		deadline = limit
	}
	return req.Args, deadline, req.Length, true
}

// readReceivingBody reads exactly the declared message and then requires EOF:
// accept declares 1 to MessageBytes bytes, bind none.
func readReceivingBody(in io.Reader, command string, length int64) ([]byte, bool) {
	if command == "accept" && (length < 1 || length > receivingLimits.MessageBytes) || command == "bind" && length != 0 {
		return nil, false
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(in, body); err != nil {
		return nil, false
	}
	if n, err := in.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		return nil, false
	}
	return body, true
}

func sameUser(conn *net.UnixConn) bool {
	raw, err := conn.SyscallConn()
	if err != nil {
		return false
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) { cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil || credErr != nil {
		return false
	}
	return cred.Uid == uint32(os.Getuid())
}
