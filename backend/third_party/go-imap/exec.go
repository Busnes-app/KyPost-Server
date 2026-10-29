package imap

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	retry "github.com/StirlingMarketingGroup/go-retry"
	"github.com/rs/xid"
)

// Response limits, 0 meaning none (KyPost patch). A server past one fails the
// command with ErrResponseTooLarge instead of making this process allocate.
var (
	MaxLiteralSize  int   // one {N} literal
	MaxLineSize     int   // one line outside literals
	MaxResponseSize int64 // everything one command reads
)

var ErrResponseTooLarge = errors.New("imap: server response exceeds the configured limit")

// readLine is ReadBytes('\n') bounded by MaxLineSize. Like ReadBytes it joins
// the fragments once, so a long line costs about twice its size, not five.
func readLine(r *bufio.Reader) ([]byte, error) {
	var frags [][]byte
	n := 0
	for {
		frag, err := r.ReadSlice('\n')
		n += len(frag)
		if MaxLineSize > 0 && n > MaxLineSize {
			return nil, ErrResponseTooLarge
		}
		if err != bufio.ErrBufferFull {
			return bytes.Join(append(frags, frag), nil), err
		}
		frags = append(frags, bytes.Clone(frag))
	}
}

// readLiterals reads IMAP literal continuations appended to a response line.
// It repeatedly checks for {NNN} or {NNN+} patterns at the end of the line,
// reads the literal data, and appends it.
func readLiterals(r *bufio.Reader, line []byte) ([]byte, error) {
	for {
		a := atom.Find(dropNl(line))
		if a == nil {
			return line, nil
		}
		sizeStr := string(a[1 : len(a)-1])
		// LITERAL+ (RFC 7888) uses {NNN+} syntax; strip trailing '+'
		sizeStr = strings.TrimSuffix(sizeStr, "+")
		n, err := strconv.Atoi(sizeStr)
		if err != nil {
			return nil, err
		}
		if MaxLiteralSize > 0 && n > MaxLiteralSize {
			return nil, ErrResponseTooLarge
		}

		buf := make([]byte, n)
		_, err = io.ReadFull(r, buf)
		if err != nil {
			return nil, err
		}
		line = append(line, buf...)

		buf, err = readLine(r)
		if err != nil {
			return nil, err
		}
		line = append(line, buf...)
	}
}

// traceCommand logs credential commands by verb only: an escaped password or
// an OAuth token would slip past substring redaction.
func traceCommand(tag, command string) string {
	if verb, _, _ := strings.Cut(command, " "); strings.EqualFold(verb, "LOGIN") || strings.EqualFold(verb, "AUTHENTICATE") {
		return tag + " " + verb + " ****"
	}
	return strings.TrimSpace(tag + " " + command)
}

// execOnce runs a single attempt of an IMAP command
func (d *Dialer) execOnce(command string, buildResponse bool, processLine func(line []byte) error) (resp strings.Builder, err error) {
	tag := []byte(strings.ToUpper(xid.New().String()))

	if CommandTimeout != 0 {
		_ = d.conn.SetDeadline(time.Now().Add(CommandTimeout))
		defer func() { _ = d.conn.SetDeadline(time.Time{}) }()
	}

	c := fmt.Sprintf("%s %s\r\n", tag, command)

	if Verbose {
		debugLog(d.ConnNum, d.Folder, "sending command", "command", traceCommand(string(tag), command))
	}

	if _, err := d.conn.Write([]byte(c)); err != nil {
		return resp, err
	}

	budget := &io.LimitedReader{R: d.conn, N: MaxResponseSize}
	if MaxResponseSize <= 0 {
		budget.N = math.MaxInt64
	}
	defer func() {
		if err != nil && budget.N <= 0 {
			err = ErrResponseTooLarge
		}
	}()
	r := bufio.NewReader(budget)
	if buildResponse {
		resp = strings.Builder{}
	}

	var readErr error
	var line []byte
	for readErr == nil {
		line, readErr = readLine(r)
		if readErr == ErrResponseTooLarge {
			return resp, readErr
		}
		var litErr error
		line, litErr = readLiterals(r, line)
		if litErr != nil {
			return resp, litErr
		}

		if Verbose && !SkipResponses {
			debugLog(d.ConnNum, d.Folder, "server response", "response", string(dropNl(line)))
		}

		// XID tags are 20 uppercase base32hex characters (0-9, A-V).
		taglen := len(tag)
		oklen := 3
		if len(line) >= taglen+oklen && bytes.Equal(line[:taglen], tag) {
			if !bytes.Equal(line[taglen+1:taglen+oklen], []byte("OK")) {
				return resp, fmt.Errorf("imap command failed: %s", excerpt(line[taglen+oklen+1:]))
			}
			break
		}

		if processLine != nil {
			if err := processLine(line); err != nil {
				return resp, err
			}
		}
		if buildResponse {
			resp.Write(line)
		}
	}
	return resp, readErr
}

// Exec executes an IMAP command with retry logic and response building
func (d *Dialer) Exec(command string, buildResponse bool, retryCount int, processLine func(line []byte) error) (response string, err error) {
	var resp strings.Builder
	err = retry.Retry(func() (err error) {
		resp, err = d.execOnce(command, buildResponse, processLine)
		return err
	}, retryCount, func(err error) error {
		if Verbose {
			warnLog(d.ConnNum, d.Folder, "command failed, closing connection", "error", err)
		}
		_ = d.Close()
		return nil
	}, func() error {
		return d.Reconnect()
	})
	if err != nil {
		errorLog(d.ConnNum, d.Folder, "command retries exhausted", "error", err)
		return "", err
	}

	if buildResponse {
		if resp.Len() != 0 {
			lastResp = resp.String()
			return lastResp, nil
		}
		return "", nil
	}
	return response, err
}
