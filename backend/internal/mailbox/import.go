package mailbox

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"path"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Self-service import of uploaded mail: an mbox (mboxrd or mboxo), one EML or
// a zip of EML files, stored as seen mail without a delivery receipt.

var (
	// ErrDuplicate: the folder already holds a live message with these bytes.
	ErrDuplicate = errors.New("message already in this folder")
	// ErrUnimportable: this message is empty, too large or not RFC 5322; skip it.
	ErrUnimportable = errors.New("message cannot be imported")
	// ErrImportArchive: the zip as a whole is refused.
	ErrImportArchive = errors.New("the zip is not readable, has more than 10,000 entries, or expands beyond the mailbox's storage")
	// ErrImportTooMany: the file holds more messages than one job processes.
	ErrImportTooMany = errors.New("the file holds more messages than one import processes")
)

const (
	// ImportFolderDefault receives an import that names no folder.
	ImportFolderDefault = "Imported"
	// maxImportZipEntries matches the mailbox's 10,000-record limit.
	maxImportZipEntries = 10000
	// importZipRatio bounds an entry's inflation over its compressed size
	// (but at least 1 MiB): a zip bomb is skipped after that much work.
	importZipRatio = 100
	// maxImportZipDirectory bounds the central directory archive/zip parses
	// into memory (several times its size) before any entry cap applies; 10,000
	// entries with long names fit well within it.
	maxImportZipDirectory = 8 << 20
)

// newZipReader is zip.NewReader; tests check it is never reached for a
// directory zipDirectory refuses.
var newZipReader = zip.NewReader

// ImportFolder normalizes an import target ("" is Imported), which must exist
// or be creatable under an existing parent, and creates it when create is set.
func (c *Client) ImportFolder(ctx context.Context, folder string, create bool) (string, error) {
	defer runtime.KeepAlive(c)
	if err := c.checkAccess(ctx); err != nil {
		return "", err
	}
	if strings.TrimSpace(folder) == "" {
		folder = ImportFolderDefault
	}
	folder, err := normalizeFolder(folder)
	if err != nil {
		return "", err
	}
	all, err := c.store.Folders(ctx)
	if err != nil || slices.Contains(all, folder) {
		return folder, err
	}
	parent, name := "", folder
	if i := strings.LastIndexByte(folder, '/'); i >= 0 {
		parent, name = folder[:i], folder[i+1:]
	}
	if err = leaf(name); err != nil {
		return "", err
	}
	if !create {
		if parent != "" && !slices.Contains(all, parent) {
			return "", ErrNotFound
		}
		return folder, nil
	}
	return folder, c.store.createFolder(ctx, parent, folder)
}

// ImportPath maps a remote folder, split at its hierarchy delimiter, under
// parent: '/' and '.' inside a component become '_'. It refuses
// (ErrUnsafeMailbox) a parent or component that is still not a folder name
// ImportFolder could create.
func ImportPath(parent string, remote []string) (string, error) {
	parts := strings.Split(strings.TrimSpace(parent), "/")
	for _, p := range remote {
		parts = append(parts, strings.TrimSpace(strings.NewReplacer("/", "_", ".", "_").Replace(p)))
	}
	for _, p := range parts {
		if err := leaf(p); err != nil {
			return "", err
		}
	}
	return normalizeFolder(strings.Join(parts, "/"))
}

// ImportValid refuses (ErrUnimportable) a message that is empty, over limit
// bytes or has no RFC 5322 header. It takes no lock and touches no store.
func ImportValid(raw []byte, limit int64) error {
	if len(raw) == 0 || int64(len(raw)) > limit {
		return ErrUnimportable
	}
	if h, _, err := rawMetadata(raw); err != nil || len(h) == 0 {
		return ErrUnimportable
	}
	return nil
}

// ImportMeta is an imported message's state at its source. The zero value
// (a file import) is seen, not starred and dated by its Date header.
type ImportMeta struct {
	Unseen, Starred bool
	Received        time.Time
}

// ImportMessage stores raw in folder with meta's state: no receipt, so
// receiving dedupe is untouched, and recorded as imported, so the poller
// (rules, sorter, notifications, incoming encryption) never takes it, even
// unread. ErrDuplicate and ErrUnimportable are per-message; anything else
// stops the import.
func (c *Client) ImportMessage(ctx context.Context, folder string, raw []byte, meta ImportMeta) error {
	defer runtime.KeepAlive(c)
	if err := ImportValid(raw, c.store.limits.MessageBytes); err != nil {
		return err
	}
	if err := c.checkAccess(ctx); err != nil {
		return err
	}
	_, err := c.store.append(ctx, folder, bytes.NewReader(raw), "", "", "", false, &meta)
	return err
}

// ReadImport calls fn with each valid message (ImportValid with limit) of an
// uploaded file, detected by content: a zip, an mbox (starts "From "), else
// one EML. fn must not retain the slice; an error from it stops the read.
// Invalid messages and zip entries refused by the guards are counted in
// skipped instead; maxExpanded bounds a zip's total inflated bytes. More than
// maxMessages messages, passed or skipped, stop it with ErrImportTooMany.
func ReadImport(f io.ReaderAt, size, limit, maxExpanded int64, maxMessages int, fn func([]byte) error) (skipped int, err error) {
	seen := 0
	each := func(raw []byte, ok bool) error {
		if seen++; seen > maxMessages {
			return ErrImportTooMany
		}
		if !ok || ImportValid(raw, limit) != nil {
			skipped++
			return nil
		}
		return fn(raw)
	}
	head := make([]byte, 5)
	n, _ := f.ReadAt(head, 0)
	switch head = head[:n]; {
	case bytes.HasPrefix(head, []byte("PK\x03\x04")) || bytes.HasPrefix(head, []byte("PK\x05\x06")):
		err = readImportZip(f, size, limit, maxExpanded, each)
		return skipped, err
	case bytes.Equal(head, []byte("From ")):
		err = readMbox(io.NewSectionReader(f, 0, size), limit, each)
		return skipped, err
	case size > limit:
		err = each(nil, false)
		return skipped, err
	}
	raw, err := io.ReadAll(io.NewSectionReader(f, 0, size))
	if err != nil {
		return skipped, err
	}
	err = each(raw, true)
	return skipped, err
}

// readMbox splits at "From " lines that start the file or follow a blank
// line, drops the blank line before each separator, and removes one '>' from
// every ^>+From line (mboxrd; for mboxo this undoes its ">From " quoting).
// Line endings stay as in the file, CRLF or LF. One message is in memory;
// each is passed to each with ok false when it outgrew limit.
func readMbox(r io.Reader, limit int64, each func([]byte, bool) error) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var msg []byte
	started, over := false, false
	lineStart, prevBlank := true, true
	flush := func() error {
		if !started {
			return nil
		}
		if bytes.HasSuffix(msg, []byte("\n\r\n")) {
			msg = msg[:len(msg)-2]
		} else if bytes.HasSuffix(msg, []byte("\n\n")) {
			msg = msg[:len(msg)-1]
		}
		return each(msg, !over)
	}
	for {
		chunk, err := br.ReadSlice('\n')
		if err != nil && err != io.EOF && !errors.Is(err, bufio.ErrBufferFull) {
			return err
		}
		ended := len(chunk) > 0 && chunk[len(chunk)-1] == '\n'
		switch {
		case len(chunk) == 0:
		case lineStart && prevBlank && bytes.HasPrefix(chunk, []byte("From ")):
			if e := flush(); e != nil {
				return e
			}
			msg, started, over = msg[:0], true, false
			for errors.Is(err, bufio.ErrBufferFull) { // a separator longer than the buffer
				_, err = br.ReadSlice('\n')
			}
			if err == io.EOF {
				return flush()
			}
			if err != nil {
				return err
			}
			lineStart, prevBlank = true, false
			continue
		case started && !over:
			line := chunk
			if lineStart {
				if t := bytes.TrimLeft(line, ">"); len(t) < len(line) && bytes.HasPrefix(t, []byte("From ")) {
					line = line[1:]
				}
			}
			// Two bytes over the limit leave room for the separator's blank line.
			if over = int64(len(msg)+len(line)) > limit+2; over {
				msg = msg[:0]
			} else {
				msg = append(msg, line...)
			}
		}
		if len(chunk) > 0 {
			prevBlank = lineStart && ended && len(bytes.TrimRight(chunk, "\r\n")) == 0
			lineStart = ended
		}
		if err == io.EOF {
			return flush()
		}
	}
}

// readImportZip passes every *.eml entry, in directory order, read into
// memory one at a time. Entry names are never used as paths; other entries
// are skipped. A bomb (over importZipRatio), oversized, unreadable or corrupt
// entry is skipped; too many entries or too many expanded bytes refuse the
// whole archive.
func readImportZip(f io.ReaderAt, size, limit, maxExpanded int64, each func([]byte, bool) error) error {
	if !zipDirectory(f, size) {
		return ErrImportArchive
	}
	zr, err := newZipReader(f, size)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return ErrImportArchive
	}
	if len(zr.File) > maxImportZipEntries {
		return ErrImportArchive
	}
	expanded := int64(0)
	var buf bytes.Buffer
	for _, e := range zr.File {
		if strings.HasSuffix(e.Name, "/") {
			continue
		}
		if !strings.EqualFold(path.Ext(e.Name), ".eml") {
			if err = each(nil, false); err != nil {
				return err
			}
			continue
		}
		budget := limit
		if e.CompressedSize64 < uint64(limit)/importZipRatio {
			budget = min(limit, max(1<<20, int64(e.CompressedSize64)*importZipRatio))
		}
		buf.Reset()
		rc, err := e.Open()
		ok := err == nil
		if ok {
			var n int64
			n, err = io.Copy(&buf, io.LimitReader(rc, budget+1))
			_ = rc.Close()
			if expanded += n; expanded > maxExpanded {
				return ErrImportArchive
			}
			ok = err == nil && n <= budget
		}
		if err = each(buf.Bytes(), ok); err != nil {
			return err
		}
	}
	return nil
}

// zipDirectory bounds what archive/zip will parse, finding the end record
// exactly as it does. archive/zip reads central-directory headers from the
// directory offset until one fails to parse, ignoring the recorded size, so
// it is safe only when the directory ends exactly where the (zip64) end
// record starts: the next "header" is then that record and parsing stops
// within the file's last 64 KiB. That also refuses prepended data. The
// recorded size and entry count must fit the caps.
func zipDirectory(f io.ReaderAt, size int64) bool {
	end, buf := int64(-1), []byte(nil)
	for _, n := range []int64{1024, 65 * 1024} {
		n = min(n, size)
		buf = make([]byte, n)
		if _, err := f.ReadAt(buf, size-n); err != nil && err != io.EOF {
			return false
		}
		if p := zipEndInBlock(buf); p >= 0 {
			end, buf = size-n+int64(p), buf[p:]
			break
		}
		if n == size {
			break
		}
	}
	if end < 0 {
		return false
	}
	le := binary.LittleEndian
	records, dirSize, dirOffset := uint64(le.Uint16(buf[10:])), uint64(le.Uint32(buf[12:])), uint64(le.Uint32(buf[16:]))
	if records == 0xffff || dirSize == 0xffff || dirOffset == 0xffffffff {
		loc := make([]byte, 20)
		if end < 20 {
			return false
		}
		if _, err := f.ReadAt(loc, end-20); err != nil || le.Uint32(loc) != 0x07064b50 || le.Uint32(loc[4:]) != 0 || le.Uint32(loc[16:]) != 1 {
			return false
		}
		p := le.Uint64(loc[8:])
		rec := make([]byte, 56)
		if p > uint64(end) {
			return false
		}
		if _, err := f.ReadAt(rec, int64(p)); err != nil || le.Uint32(rec) != 0x06064b50 {
			return false
		}
		end, records, dirSize, dirOffset = int64(p), le.Uint64(rec[32:]), le.Uint64(rec[40:]), le.Uint64(rec[48:])
	}
	return records <= maxImportZipEntries && dirSize <= maxImportZipDirectory && dirOffset <= uint64(end) && dirOffset+dirSize == uint64(end)
}

// zipEndInBlock is archive/zip's findSignatureInBlock.
func zipEndInBlock(b []byte) int {
	for i := len(b) - 22; i >= 0; i-- {
		if b[i] == 'P' && b[i+1] == 'K' && b[i+2] == 0x05 && b[i+3] == 0x06 {
			if int(b[i+20])|int(b[i+21])<<8+22+i > len(b) {
				return -1
			}
			return i
		}
	}
	return -1
}
