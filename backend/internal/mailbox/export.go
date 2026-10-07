package mailbox

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ExportMessage is one stored message as an export writes it: the exact raw
// bytes plus the envelope sender (empty when no delivery receipt holds one)
// and the stored date.
type ExportMessage struct {
	ID     int64
	Folder string
	Sender string
	At     time.Time
	Raw    []byte
}

const exportPage = 200

// ExportFolders resolves an export's folders: every folder for "", else the
// one named folder, which must exist.
func (c *Client) ExportFolders(ctx context.Context, folder string) ([]string, error) {
	defer runtime.KeepAlive(c)
	if err := c.checkAccess(ctx); err != nil {
		return nil, err
	}
	all, err := c.store.Folders(ctx)
	if err != nil || folder == "" {
		return all, err
	}
	if folder, err = normalizeFolder(folder); err != nil {
		return nil, err
	}
	for _, name := range all {
		if name == folder {
			return []string{folder}, nil
		}
	}
	return nil, ErrNotFound
}

// Export calls fn for every live message in folders, oldest first, holding
// one message in memory at a time. A message moved or deleted mid-export is
// skipped. Admission is rechecked on every page.
func (c *Client) Export(ctx context.Context, folders []string, fn func(ExportMessage) error) error {
	defer runtime.KeepAlive(c)
	for _, folder := range folders {
		for after := int64(0); ; {
			if err := c.checkAccess(ctx); err != nil {
				return err
			}
			page, err := c.store.exportPage(ctx, folder, after)
			if err != nil {
				return err
			}
			for _, m := range page {
				after = m.ID
				if m.Raw, err = c.store.Raw(ctx, folder, m.ID); errors.Is(err, ErrNotFound) {
					continue
				} else if err != nil {
					return err
				}
				if err = fn(m); err != nil {
					return err
				}
			}
			if len(page) < exportPage {
				break
			}
		}
	}
	return nil
}

func (s *Store) exportPage(ctx context.Context, folder string, after int64) ([]ExportMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.id,m.at_utc,coalesce((SELECT envelope FROM receipts WHERE message_id=m.id LIMIT 1),'') FROM messages m WHERE m.folder=? AND m.raw IS NOT NULL AND m.id>? ORDER BY m.id LIMIT ?`, folder, after, exportPage)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExportMessage{}
	for rows.Next() {
		m := ExportMessage{Folder: folder}
		var at, envelope string
		if err = rows.Scan(&m.ID, &at, &envelope); err != nil {
			return nil, err
		}
		m.At, _ = time.Parse(time.RFC3339, at)
		if envelope != "" {
			var r Receipt
			if err = json.Unmarshal([]byte(envelope), &r); err != nil {
				return nil, err
			}
			m.Sender = r.Sender
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// WriteMboxrd writes one message in mboxrd form: a "From " separator with the
// envelope sender (MAILER-DAEMON when absent or unfit for the line) and the
// stored date in UTC, the raw bytes with one '>' added to every line matching
// ^>*From , then a blank line. Line endings stay as stored (normally CRLF);
// the separator and blank line are LF, so unquoting returns the exact bytes
// of a message that ends in a newline.
func WriteMboxrd(w io.Writer, m ExportMessage) error {
	sender := m.Sender
	if sender == "" || len(sender) > 320 || strings.IndexFunc(sender, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		sender = "MAILER-DAEMON"
	}
	at := m.At
	if at.IsZero() {
		at = time.Unix(0, 0)
	}
	if _, err := fmt.Fprintf(w, "From %s %s\n", sender, at.UTC().Format(time.ANSIC)); err != nil {
		return err
	}
	for raw := m.Raw; len(raw) > 0; {
		line := raw
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			line = raw[:i+1]
		}
		raw = raw[len(line):]
		if bytes.HasPrefix(bytes.TrimLeft(line, ">"), []byte("From ")) {
			if _, err := io.WriteString(w, ">"); err != nil {
				return err
			}
		}
		if _, err := w.Write(line); err != nil {
			return err
		}
	}
	end := "\n"
	if !bytes.HasSuffix(m.Raw, []byte("\n")) {
		end = "\n\n"
	}
	_, err := io.WriteString(w, end)
	return err
}

// WriteEML adds one message to a zip as <folder>/<id>.eml holding the exact
// raw bytes. The ID is unique within the mailbox, so sanitized folders that
// collide never collide file names.
func WriteEML(z *zip.Writer, m ExportMessage) error {
	f, err := z.CreateHeader(&zip.FileHeader{Name: ZipFolder(m.Folder) + fmt.Sprintf("/%010d.eml", m.ID), Method: zip.Deflate, Modified: m.At.UTC()})
	if err != nil {
		return err
	}
	_, err = f.Write(m.Raw)
	return err
}

// ZipFolder maps a folder path to a relative zip directory: each segment
// keeps letters, digits, space, '-', '_' and '.', replaces anything else, is
// cut to 64 bytes and loses leading/trailing dots and spaces; an empty or
// dot-only segment becomes "_". No result is absolute or climbs out.
func ZipFolder(folder string) string {
	parts := strings.Split(folder, "/")
	for i, part := range parts {
		part = strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune(" -_.", r) {
				return r
			}
			return '_'
		}, part)
		for len(part) > 64 {
			_, size := utf8.DecodeLastRuneInString(part)
			part = part[:len(part)-size]
		}
		if part = strings.Trim(part, ". "); part == "" {
			part = "_"
		}
		parts[i] = part
	}
	return strings.Join(parts, "/")
}
