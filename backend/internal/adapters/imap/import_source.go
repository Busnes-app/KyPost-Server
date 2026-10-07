package imap

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// ImportSource is a read-only IMAP session on an account a user imports
// from. Its whole vocabulary is STARTTLS, LOGIN, LIST, EXAMINE, FETCH, UID
// FETCH of BODY.PEEK[] and LOGOUT, so nothing on the remote server changes.
//
// It does not use go-imap: that library dials the host name itself (again on
// every automatic reconnect), so it cannot be held to the address the caller's
// SSRF guard approved; it has no STARTTLS; certificate verification is a
// process-wide variable; and it buffers whole responses before any size check.
type ImportSource struct {
	raw    net.Conn
	conn   io.ReadWriter
	r      *bufio.Reader
	tag    int
	budget int   // bytes left in this response outside literals
	lits   int64 // literal bytes left in this response
	left   int64 // bytes the whole session may still read
	stop   func() bool
}

// budgeted counts every byte read from the connection, before or after TLS,
// whatever the response it belongs to.
type budgeted struct {
	r io.Reader
	s *ImportSource
}

func (b budgeted) Read(p []byte) (int, error) {
	if b.s.left <= 0 {
		return 0, ErrImportBudget
	}
	if int64(len(p)) > b.s.left {
		p = p[:b.s.left]
	}
	n, err := b.r.Read(p)
	b.s.left -= int64(n)
	return n, err
}

// RemoteFolder is a selectable folder: Name exactly as listed (what EXAMINE
// takes), Path its decoded hierarchy, Attrs its lowercased attributes.
type RemoteFolder struct {
	Name  string
	Path  []string
	Attrs []string
}

// RemoteMessage is one message's metadata. Received is its INTERNALDATE, zero
// when the server sent none or an unreadable one.
type RemoteMessage struct {
	UID           uint32
	Size          int64
	Seen, Flagged bool
	Received      time.Time
}

var (
	ErrImportTLS      = errors.New("a verified TLS connection to the server could not be established")
	ErrImportLogin    = errors.New("the server refused the sign-in")
	ErrImportRefused  = errors.New("the server refused the request")
	ErrImportProtocol = errors.New("the server answered in a way the import does not understand")
	ErrImportTooLarge = errors.New("the server sent a message larger than it announced")
	ErrImportFolders  = errors.New("the account has more than 1000 folders")
	// ErrImportBudget: the session read everything OpenImportSource allowed.
	ErrImportBudget = errors.New("the server sent more than the import allows")
	// ErrImportGone: the message was deleted on the server during the import.
	ErrImportGone = errors.New("the message is no longer on the server")
)

const (
	importLineMax    = 64 << 10 // bytes of one response outside its literals
	importMetaLit    = 64 << 10 // largest literal outside a body fetch
	importMaxFolders = 1000
	importMaxDepth   = 8
)

// Per-step deadlines; the caller's context bounds the whole session.
var (
	importStepTimeout  = 30 * time.Second
	importFetchTimeout = 2 * time.Minute
)

// OpenImportSource speaks TLS over conn, implicit or after STARTTLS, verifying
// the certificate for serverName against roots (nil: the system's), and signs
// in. password is not retained. The session reads at most maxBytes in all
// (ErrImportBudget). Cancelling ctx closes the connection.
func OpenImportSource(ctx context.Context, conn net.Conn, serverName string, startTLS bool, roots *x509.CertPool, username string, password []byte, maxBytes int64) (*ImportSource, error) {
	s := &ImportSource{raw: conn, conn: conn, left: maxBytes}
	s.r = bufio.NewReader(budgeted{conn, s})
	s.stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
	if err := s.open(ctx, serverName, startTLS, roots, username, password); err != nil {
		s.stop()
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return s, nil
}

func (s *ImportSource) open(ctx context.Context, serverName string, startTLS bool, roots *x509.CertPool, username string, password []byte) error {
	_ = s.raw.SetDeadline(time.Now().Add(importStepTimeout))
	if startTLS {
		if err := s.greeting(); err != nil {
			return err
		}
		if err := s.run(importStepTimeout, importMetaLit, nil, "STARTTLS"); err != nil {
			return ErrImportTLS
		}
		// Bytes already buffered would be read as if they came over TLS.
		if s.r.Buffered() != 0 {
			return ErrImportProtocol
		}
	}
	tc := tls.Client(s.raw, &tls.Config{ServerName: serverName, RootCAs: roots, MinVersion: tls.VersionTLS12})
	if err := tc.HandshakeContext(ctx); err != nil {
		return ErrImportTLS
	}
	s.conn, s.r = tc, bufio.NewReader(budgeted{tc, s})
	if !startTLS {
		if err := s.greeting(); err != nil {
			return err
		}
	}
	return s.login(username, password)
}

// greeting accepts only "* OK": PREAUTH before STARTTLS would skip TLS.
func (s *ImportSource) greeting() error {
	tag, fields, err := s.response(importMetaLit)
	if err != nil {
		return err
	}
	if tag != "*" || len(fields) == 0 || !strings.EqualFold(atomOf(fields[0]), "OK") {
		return ErrImportRefused
	}
	return nil
}

// login sends each credential quoted, or as a synchronizing literal when it
// cannot be quoted, and wipes the command bytes it built.
func (s *ImportSource) login(username string, password []byte) error {
	_ = s.raw.SetDeadline(time.Now().Add(importStepTimeout))
	s.tag++
	tag := "k" + strconv.Itoa(s.tag)
	if err := s.write([]byte(tag + " LOGIN ")); err != nil {
		return err
	}
	for i, arg := range [][]byte{[]byte(username), password} {
		if i == 1 {
			if err := s.write([]byte(" ")); err != nil {
				return err
			}
		}
		if err := s.astring(arg); err != nil {
			return err
		}
	}
	if err := s.write([]byte("\r\n")); err != nil {
		return err
	}
	if err := s.complete(tag, importMetaLit, nil); errors.Is(err, ErrImportRefused) {
		return ErrImportLogin
	} else if err != nil {
		return err
	}
	return nil
}

func (s *ImportSource) astring(b []byte) error {
	quotable := len(b) > 0 && len(b) < 1000
	for _, c := range b {
		quotable = quotable && c >= 0x20 && c < 0x7f
	}
	if quotable {
		buf := make([]byte, 0, len(b)+8)
		buf = append(buf, '"')
		for _, c := range b {
			if c == '"' || c == '\\' {
				buf = append(buf, '\\')
			}
			buf = append(buf, c)
		}
		buf = append(buf, '"')
		err := s.write(buf)
		clear(buf)
		return err
	}
	if len(b) == 0 || len(b) > 4096 {
		return ErrImportLogin
	}
	if err := s.write([]byte("{" + strconv.Itoa(len(b)) + "}\r\n")); err != nil {
		return err
	}
	tag, _, err := s.response(importMetaLit)
	if err != nil {
		return err
	}
	if tag != "+" {
		return ErrImportLogin
	}
	return s.write(b)
}

// Folders lists every selectable folder (no \Noselect or \NonExistent) whose
// name is safe to send back (no control character, backslash or quote) and at
// most 255 bytes.
func (s *ImportSource) Folders() ([]RemoteFolder, error) {
	var out []RemoteFolder
	err := s.run(importStepTimeout, importMetaLit, func(f []any) error {
		if len(f) != 4 || !strings.EqualFold(atomOf(f[0]), "LIST") {
			return nil
		}
		attrs, _ := f[1].([]any)
		name := textOf(f[3])
		folder := RemoteFolder{Name: name}
		for _, a := range attrs {
			a := strings.ToLower(atomOf(a))
			if a == `\noselect` || a == `\nonexistent` {
				return nil
			}
			folder.Attrs = append(folder.Attrs, a)
		}
		if len(name) > 255 || ValidateMailboxName(name) != nil {
			return nil
		}
		decoded, ok := decodeMUTF7(name)
		if !ok {
			decoded = name
		}
		if delim := textOf(f[2]); delim != "" {
			folder.Path = strings.Split(decoded, delim)
		} else {
			folder.Path = []string{decoded}
		}
		if len(out) == importMaxFolders {
			return ErrImportFolders
		}
		out = append(out, folder)
		return nil
	}, `LIST "" "*"`)
	return out, err
}

// Examine opens name read-only and returns how many messages it holds.
func (s *ImportSource) Examine(name string) (int, error) {
	if err := ValidateMailboxName(name); err != nil {
		return 0, err
	}
	exists := 0
	err := s.run(importStepTimeout, importMetaLit, func(f []any) error {
		if len(f) == 2 && strings.EqualFold(atomOf(f[1]), "EXISTS") {
			n, err := strconv.Atoi(atomOf(f[0]))
			if err != nil || n < 0 {
				return ErrImportProtocol
			}
			exists = n
		}
		return nil
	}, `EXAMINE "`+name+`"`)
	return exists, err
}

// Messages returns the metadata of sequence numbers lo..hi of the examined
// folder, in order, without bodies.
func (s *ImportSource) Messages(lo, hi int) ([]RemoteMessage, error) {
	if lo < 1 || hi < lo {
		return nil, nil
	}
	bySeq := map[int]RemoteMessage{}
	err := s.run(importStepTimeout, importMetaLit, func(f []any) error {
		seq, items := fetchItems(f)
		uid, err := strconv.ParseUint(atomOf(items["UID"]), 10, 32)
		if seq < lo || seq > hi || err != nil || uid == 0 {
			return nil // unsolicited, or a flags update
		}
		m := RemoteMessage{UID: uint32(uid), Size: -1}
		if n, err := strconv.ParseInt(atomOf(items["RFC822.SIZE"]), 10, 64); err == nil {
			m.Size = n
		}
		flags, _ := items["FLAGS"].([]any)
		for _, fl := range flags {
			m.Seen = m.Seen || strings.EqualFold(atomOf(fl), `\Seen`)
			m.Flagged = m.Flagged || strings.EqualFold(atomOf(fl), `\Flagged`)
		}
		m.Received, _ = time.Parse("_2-Jan-2006 15:04:05 -0700", textOf(items["INTERNALDATE"]))
		bySeq[seq] = m
		return nil
	}, "FETCH "+strconv.Itoa(lo)+":"+strconv.Itoa(hi)+" (UID RFC822.SIZE FLAGS INTERNALDATE)")
	if err != nil {
		return nil, err
	}
	out := make([]RemoteMessage, 0, len(bySeq))
	for seq := lo; seq <= hi; seq++ {
		if m, ok := bySeq[seq]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// Fetch returns the exact bytes of uid with BODY.PEEK[], which leaves \Seen
// alone. A literal over limit is refused before it is read
// (ErrImportTooLarge); the session is then unusable.
func (s *ImportSource) Fetch(uid uint32, limit int64) ([]byte, error) {
	var raw []byte
	found := false
	err := s.run(importFetchTimeout, limit, func(f []any) error {
		_, items := fetchItems(f)
		if atomOf(items["UID"]) != strconv.FormatUint(uint64(uid), 10) {
			return nil
		}
		switch v := items["BODY[]"].(type) {
		case []byte:
			raw, found = v, true
		case string:
			raw, found = []byte(v), true
		}
		return nil
	}, "UID FETCH "+strconv.FormatUint(uint64(uid), 10)+" (BODY.PEEK[])")
	if err == nil && !found {
		err = ErrImportGone
	}
	return raw, err
}

// Close signs out, briefly, and closes the connection.
func (s *ImportSource) Close() error {
	_ = s.run(2*time.Second, importMetaLit, nil, "LOGOUT")
	s.stop()
	return s.raw.Close()
}

// run sends one command and reads to its completion, handing each untagged
// response to each; an error from each stops reading and is returned.
func (s *ImportSource) run(timeout time.Duration, literal int64, each func([]any) error, command string) error {
	_ = s.raw.SetDeadline(time.Now().Add(timeout))
	s.tag++
	tag := "k" + strconv.Itoa(s.tag)
	if err := s.write([]byte(tag + " " + command + "\r\n")); err != nil {
		return err
	}
	return s.complete(tag, literal, each)
}

func (s *ImportSource) complete(tag string, literal int64, each func([]any) error) error {
	for {
		got, fields, err := s.response(literal)
		switch {
		case err != nil:
			return err
		case got == tag:
			if len(fields) > 0 && strings.EqualFold(atomOf(fields[0]), "OK") {
				return nil
			}
			return ErrImportRefused
		case got != "*":
			return ErrImportProtocol
		case len(fields) > 0 && strings.EqualFold(atomOf(fields[0]), "BYE"):
			return ErrImportRefused
		case each != nil:
			if err = each(fields); err != nil {
				return err
			}
		}
	}
}

func (s *ImportSource) write(b []byte) error {
	_, err := s.conn.Write(b)
	return err
}

// response reads one response line: its tag ("*", "+" or a command tag) and
// its fields. A status word (OK, NO, BAD, BYE, PREAUTH) ends the parsed
// fields; its human-readable text is read and dropped.
func (s *ImportSource) response(literal int64) (string, []any, error) {
	s.budget, s.lits = importLineMax, literal+importMetaLit
	tag, err := s.atom()
	if err != nil {
		return "", nil, err
	}
	if tag == "+" {
		return tag, nil, s.discardLine()
	}
	var fields []any
	for {
		s.spaces()
		b, err := s.peek()
		if err != nil {
			return "", nil, err
		}
		if b == '\r' || b == '\n' {
			return tag, fields, s.discardLine()
		}
		v, err := s.value(0, literal)
		if err != nil {
			return "", nil, err
		}
		fields = append(fields, v)
		switch strings.ToUpper(atomOf(v)) {
		case "OK", "NO", "BAD", "BYE", "PREAUTH":
			if len(fields) == 1 {
				return tag, fields, s.discardLine()
			}
		}
	}
}

// value parses an atom or NIL (string, nil), a quoted string (string), a
// literal ([]byte) or a parenthesized list ([]any). An atom keeps a bracketed
// section whole, so BODY[] is one atom.
func (s *ImportSource) value(depth int, literal int64) (any, error) {
	b, err := s.peek()
	if err != nil {
		return nil, err
	}
	switch b {
	case '(':
		if depth == importMaxDepth {
			return nil, ErrImportProtocol
		}
		_, _ = s.byte()
		list := []any{}
		for {
			s.spaces()
			if b, err = s.peek(); err != nil {
				return nil, err
			}
			if b == ')' {
				_, _ = s.byte()
				return list, nil
			}
			v, err := s.value(depth+1, literal)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
	case '"':
		_, _ = s.byte()
		var out []byte
		for {
			c, err := s.byte()
			if err != nil {
				return nil, err
			}
			switch c {
			case '"':
				return string(out), nil
			case '\r', '\n':
				return nil, ErrImportProtocol
			case '\\':
				if c, err = s.byte(); err != nil {
					return nil, err
				}
			}
			out = append(out, c)
		}
	case '{':
		_, _ = s.byte()
		n := int64(0)
		for {
			c, err := s.byte()
			if err != nil {
				return nil, err
			}
			if c == '}' {
				break
			}
			if c < '0' || c > '9' || n > 1<<40 {
				return nil, ErrImportProtocol
			}
			n = n*10 + int64(c-'0')
		}
		if err := s.discardLine(); err != nil {
			return nil, err
		}
		if n > literal || n > s.lits {
			return nil, ErrImportTooLarge
		}
		s.lits -= n
		out := make([]byte, n)
		if _, err := io.ReadFull(s.r, out); err != nil {
			return nil, err
		}
		return out, nil
	case ')', '\r', '\n':
		return nil, ErrImportProtocol
	}
	a, err := s.atom()
	if err != nil || a == "" {
		return nil, ErrImportProtocol
	}
	if strings.EqualFold(a, "NIL") {
		return nil, nil
	}
	return a, nil
}

func (s *ImportSource) atom() (string, error) {
	var out []byte
	bracket := false
	for {
		b, err := s.peek()
		if err != nil {
			return "", err
		}
		if b == '\r' || b == '\n' || !bracket && strings.IndexByte(` (){"`, b) >= 0 {
			return string(out), nil
		}
		_, _ = s.byte()
		bracket = bracket && b != ']' || b == '['
		out = append(out, b)
	}
}

func (s *ImportSource) spaces() {
	for b, err := s.peek(); err == nil && b == ' '; b, err = s.peek() {
		_, _ = s.byte()
	}
}

// discardLine reads through the next LF, within the line budget.
func (s *ImportSource) discardLine() error {
	for {
		c, err := s.byte()
		if err != nil || c == '\n' {
			return err
		}
	}
}

func (s *ImportSource) peek() (byte, error) {
	b, err := s.r.Peek(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

// byte reads one byte outside a literal, refusing a line over importLineMax.
func (s *ImportSource) byte() (byte, error) {
	if s.budget--; s.budget < 0 {
		return 0, ErrImportProtocol
	}
	return s.r.ReadByte()
}

// fetchItems reads "<seq> FETCH (name value ...)" into upper-cased names.
func fetchItems(f []any) (int, map[string]any) {
	if len(f) != 3 || !strings.EqualFold(atomOf(f[1]), "FETCH") {
		return 0, nil
	}
	seq, err := strconv.Atoi(atomOf(f[0]))
	list, ok := f[2].([]any)
	if err != nil || !ok || len(list)%2 != 0 {
		return 0, nil
	}
	items := make(map[string]any, len(list)/2)
	for i := 0; i < len(list); i += 2 {
		items[strings.ToUpper(atomOf(list[i]))] = list[i+1]
	}
	return seq, items
}

func atomOf(v any) string {
	s, _ := v.(string)
	return s
}

func textOf(v any) string {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return atomOf(v)
}

// decodeMUTF7 decodes an RFC 3501 modified UTF-7 mailbox name.
func decodeMUTF7(s string) (string, bool) {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e {
			return "", false
		}
		if c != '&' {
			out.WriteByte(c)
			continue
		}
		end := strings.IndexByte(s[i+1:], '-')
		if end < 0 {
			return "", false
		}
		enc := s[i+1 : i+1+end]
		i += end + 1
		if enc == "" {
			out.WriteByte('&')
			continue
		}
		raw, err := base64.RawStdEncoding.DecodeString(strings.ReplaceAll(enc, ",", "/"))
		if err != nil || len(raw)%2 != 0 {
			return "", false
		}
		units := make([]uint16, len(raw)/2)
		for k := range units {
			units[k] = uint16(raw[2*k])<<8 | uint16(raw[2*k+1])
		}
		out.WriteString(string(utf16.Decode(units)))
	}
	return out.String(), true
}
