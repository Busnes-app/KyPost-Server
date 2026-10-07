package mailbox

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"path"
	"runtime"
	"slices"
	"strings"
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
)

const (
	// ImportFolderDefault receives an import that names no folder.
	ImportFolderDefault = "Imported"
	// maxImportZipEntries matches the mailbox's 10,000-record limit.
	maxImportZipEntries = 10000
	// importZipRatio bounds an entry's inflation over its compressed size
	// (but at least 1 MiB): a zip bomb is skipped after that much work.
	importZipRatio = 100
)

// ImportFolder normalizes an import target ("" is Imported) and creates it,
// under an existing parent, when it is missing.
func (c *Client) ImportFolder(ctx context.Context, folder string) (string, error) {
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
	return folder, c.store.createFolder(ctx, parent, folder)
}

// ImportMessage stores raw in folder as seen mail: no receipt, so receiving
// dedupe is untouched, and seen, so the poller (rules, sorter, notifications,
// incoming encryption) never takes it. ErrDuplicate and ErrUnimportable are
// per-message; anything else stops the import.
func (c *Client) ImportMessage(ctx context.Context, folder string, raw []byte) error {
	defer runtime.KeepAlive(c)
	if err := c.checkAccess(ctx); err != nil {
		return err
	}
	if len(raw) == 0 || int64(len(raw)) > c.store.limits.MessageBytes {
		return ErrUnimportable
	}
	if h, _, err := rawMetadata(raw); err != nil || len(h) == 0 {
		return ErrUnimportable
	}
	_, err := c.store.append(ctx, folder, bytes.NewReader(raw), "", "", "", false, true)
	return err
}

// ReadImport calls fn with each message of an uploaded file, detected by
// content: a zip, an mbox (starts "From "), else one EML. fn must not retain
// the slice; an error from it stops the read. Messages over limit bytes and
// zip entries refused by the guards are not passed to fn but counted in
// skipped; maxExpanded bounds a zip's total inflated bytes.
func ReadImport(f io.ReaderAt, size, limit, maxExpanded int64, fn func([]byte) error) (skipped int, err error) {
	head := make([]byte, 5)
	n, _ := f.ReadAt(head, 0)
	switch head = head[:n]; {
	case bytes.HasPrefix(head, []byte("PK\x03\x04")) || bytes.HasPrefix(head, []byte("PK\x05\x06")):
		return readImportZip(f, size, limit, maxExpanded, fn)
	case bytes.Equal(head, []byte("From ")):
		return readMbox(io.NewSectionReader(f, 0, size), limit, fn)
	case size > limit:
		return 1, nil
	}
	raw, err := io.ReadAll(io.NewSectionReader(f, 0, size))
	if err != nil {
		return 0, err
	}
	return 0, fn(raw)
}

// readMbox splits at "From " lines that start the file or follow a blank
// line, drops the blank line before each separator, and removes one '>' from
// every ^>+From line (mboxrd; for mboxo this undoes its ">From " quoting).
// Line endings stay as in the file, CRLF or LF. One message is in memory.
func readMbox(r io.Reader, limit int64, fn func([]byte) error) (int, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var msg []byte
	skipped, started, over := 0, false, false
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
		if over || int64(len(msg)) > limit {
			skipped++
			return nil
		}
		return fn(msg)
	}
	for {
		chunk, err := br.ReadSlice('\n')
		if err != nil && err != io.EOF && !errors.Is(err, bufio.ErrBufferFull) {
			return skipped, err
		}
		ended := len(chunk) > 0 && chunk[len(chunk)-1] == '\n'
		switch {
		case len(chunk) == 0:
		case lineStart && prevBlank && bytes.HasPrefix(chunk, []byte("From ")):
			if e := flush(); e != nil {
				return skipped, e
			}
			msg, started, over = msg[:0], true, false
			for errors.Is(err, bufio.ErrBufferFull) { // a separator longer than the buffer
				_, err = br.ReadSlice('\n')
			}
			if err == io.EOF {
				return skipped, flush()
			}
			if err != nil {
				return skipped, err
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
			return skipped, flush()
		}
	}
}

// readImportZip passes every *.eml entry, in directory order, read into
// memory one at a time. Entry names are never used as paths; other entries
// are skipped. A bomb (over importZipRatio), oversized, unreadable or corrupt
// entry is skipped; too many entries or too many expanded bytes refuse the
// whole archive.
func readImportZip(f io.ReaderAt, size, limit, maxExpanded int64, fn func([]byte) error) (int, error) {
	// ponytail: archive/zip reads the whole central directory before the entry
	// cap applies, about 4x the upload in memory at worst; the upload is capped
	// at the mailbox quota. A streaming directory walk is the upgrade if that
	// quota grows large.
	zr, err := zip.NewReader(f, size)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return 0, ErrImportArchive
	}
	if len(zr.File) > maxImportZipEntries {
		return 0, ErrImportArchive
	}
	skipped, expanded := 0, int64(0)
	var buf bytes.Buffer
	for _, e := range zr.File {
		if strings.HasSuffix(e.Name, "/") {
			continue
		}
		if !strings.EqualFold(path.Ext(e.Name), ".eml") {
			skipped++
			continue
		}
		budget := limit
		if e.CompressedSize64 < uint64(limit)/importZipRatio {
			budget = max(1<<20, int64(e.CompressedSize64)*importZipRatio)
		}
		budget = min(budget, limit)
		rc, err := e.Open()
		if err != nil {
			skipped++
			continue
		}
		buf.Reset()
		n, err := io.Copy(&buf, io.LimitReader(rc, budget+1))
		_ = rc.Close()
		if expanded += n; expanded > maxExpanded {
			return skipped, ErrImportArchive
		}
		if err != nil || n > budget {
			skipped++
			continue
		}
		if err = fn(buf.Bytes()); err != nil {
			return skipped, err
		}
	}
	return skipped, nil
}
