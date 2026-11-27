package imap

import (
	"bufio"
	"bytes"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rs/xid"
)

var appendUIDPattern = regexp.MustCompile(`(?i)\[APPENDUID\s+\d+\s+(\d+)\]`)

// parseAppendUID extracts the assigned message UID from an RFC 4315
// `[APPENDUID <uidvalidity> <uid>]` response code in a tagged OK line.
// Returns 0 when the server did not include an APPENDUID code.
func parseAppendUID(line []byte) int {
	m := appendUIDPattern.FindSubmatch(line)
	if len(m) != 2 {
		return 0
	}
	v, err := strconv.ParseUint(string(m[1]), 10, 32)
	if err != nil || v == 0 || v > uint64(math.MaxInt) {
		return 0
	}
	return int(v)
}

// waitForTaggedOKLine reads lines from r until it finds the tagged response matching tag.
// It returns the matched line if the response is OK, or an error otherwise.
func (d *Dialer) waitForTaggedOKLine(r *bufio.Reader, tag []byte) ([]byte, error) {
	taglen := len(tag)
	for {
		line, err := readLine(r)
		if err != nil {
			_ = d.Close()
			return nil, fmt.Errorf("imap append read response: %w", err)
		}

		if Verbose && !SkipResponses {
			debugLog(d.ConnNum, d.Folder, "server response", "response", string(dropNl(line)))
		}

		if len(line) >= taglen+3 && bytes.Equal(line[:taglen], tag) {
			if !bytes.Equal(line[taglen+1:taglen+3], []byte("OK")) {
				return nil, fmt.Errorf("imap append failed: %s", excerpt(dropNl(line[taglen+1:])))
			}
			return line, nil
		}
	}
}

// Append uploads a message to the specified folder.
//
// The flags parameter specifies initial flags for the message (e.g., `\Seen`, `\Draft`).
// Pass nil or an empty slice for no flags. The date parameter sets the internal date;
// pass a zero time.Time to let the server use the current time.
//
// The message parameter should be a complete RFC 2822 message (headers + body).
//
// Note: Append does not retry on failure because the IMAP APPEND protocol uses a
// two-phase literal transfer. A partial send cannot be safely retried without
// risking duplicate messages.
//
// Example:
//
//	msg := []byte("From: a@b.com\r\nTo: c@d.com\r\nSubject: Hi\r\n\r\nHello!")
//	err := conn.Append("INBOX", []string{`\Seen`}, time.Time{}, msg)
func (d *Dialer) Append(folder string, flags []string, date time.Time, message []byte) error {
	_, err := d.AppendUID(folder, flags, date, message)
	return err
}

// AppendUID uploads a message to folder like Append and returns the server's
// RFC 4315 APPENDUID when present in the tagged OK response (0 otherwise).
func (d *Dialer) AppendUID(folder string, flags []string, date time.Time, message []byte) (int, error) {
	// Build the APPEND command prefix
	flagStr := ""
	if len(flags) > 0 {
		flagStr = " (" + strings.Join(flags, " ") + ")"
	}
	dateStr := ""
	if !date.IsZero() {
		dateStr = fmt.Sprintf(` "%s"`, date.Format(TimeFormat))
	}

	cmd := fmt.Sprintf(`APPEND "%s"%s%s {%d}`,
		AddSlashes.Replace(folder), flagStr, dateStr, len(message))

	tag := []byte(strings.ToUpper(xid.New().String()))

	if CommandTimeout != 0 {
		_ = d.conn.SetDeadline(time.Now().Add(CommandTimeout))
		defer func() { _ = d.conn.SetDeadline(time.Time{}) }()
	}

	if Verbose {
		debugLog(d.ConnNum, d.Folder, "sending command", "command", string(tag)+" "+cmd)
	}

	// Phase 1: Send the APPEND command with literal size
	_, err := fmt.Fprintf(d.conn, "%s %s\r\n", tag, cmd)
	if err != nil {
		return 0, fmt.Errorf("imap append write command: %w", err)
	}

	// Phase 2: Wait for continuation response (+)
	r := bufio.NewReader(d.conn)
	line, err := readLine(r)
	if err != nil {
		_ = d.Close()
		return 0, fmt.Errorf("imap append read continuation: %w", err)
	}

	if Verbose && !SkipResponses {
		debugLog(d.ConnNum, d.Folder, "server response", "response", string(dropNl(line)))
	}

	if !bytes.HasPrefix(bytes.TrimSpace(line), []byte("+")) {
		return 0, fmt.Errorf("imap append: expected continuation (+), got: %s", excerpt(dropNl(line)))
	}

	// Phase 3: Send the literal message bytes
	_, err = d.conn.Write(message)
	if err != nil {
		_ = d.Close()
		return 0, fmt.Errorf("imap append write literal: %w", err)
	}
	_, err = d.conn.Write([]byte("\r\n"))
	if err != nil {
		_ = d.Close()
		return 0, fmt.Errorf("imap append write crlf: %w", err)
	}

	// Phase 4: Read the tagged response and extract APPENDUID if present.
	okLine, err := d.waitForTaggedOKLine(r, tag)
	if err != nil {
		return 0, err
	}
	return parseAppendUID(okLine), nil
}
