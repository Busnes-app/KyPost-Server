// Package vol is the multi-host volume seam (docs/superpowers/specs/2026-11-22-multi-host-replicas-design.md,
// "The documents table"): one interface over a store's files, chosen once at open. File is
// today's fsutil calls; Table is volumes.db's documents table, selected by table mode.
package vol

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"unicode/utf8"
)

// Vol reads and writes documents by root-relative, slash-separated path. A revision is the
// one a Read returned; 0 means absent. Read of an absent document is fs.ErrNotExist.
type Vol interface {
	Read(ctx context.Context, path string) (body []byte, rev int64, err error)
	// Write stores body if the document is still at rev and returns its new revision.
	Write(ctx context.Context, path string, body []byte, rev int64) (int64, error)
	// Remove deletes the document if it is still at rev (> 0).
	Remove(ctx context.Context, path string, rev int64) error
	// List returns every document path under the directory prefix, sorted.
	List(ctx context.Context, prefix string) ([]string, error)
}

// Bounds. The owner approved 64 MiB for table documents on 2026-11-23.
// Offline migration must refuse larger files; ordinary file stores keep their limits.
const (
	MaxPath    = 512
	MaxBody    = 64 << 20
	maxRetries = 5
)

var (
	// ErrPath is a path the boundary refuses: absolute, empty, an empty, "." or ".."
	// segment, a NUL or backslash, invalid UTF-8, or over MaxPath bytes.
	ErrPath = errors.New("vol: invalid document path")
	// ErrTooLarge is a body over MaxBody.
	ErrTooLarge = errors.New("vol: document too large")
	// ErrStale is a write whose revision is no longer the document's.
	ErrStale = errors.New("vol: stale revision")
	// ErrBusy is Update giving up after maxRetries stale writes; callers answer 503.
	ErrBusy = errors.New("vol: document busy, retry later")

	errNegative = errors.New("vol: negative revision")
)

// CheckPath validates a document path before any I/O. Errors never repeat the path, which
// carries user and mailbox IDs.
func CheckPath(p string) error {
	if p == "" || len(p) > MaxPath || !utf8.ValidString(p) || strings.ContainsAny(p, "\x00\\") {
		return ErrPath
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return ErrPath
		}
	}
	return nil
}

func checkWrite(path string, body []byte, rev int64) error {
	if err := CheckPath(path); err != nil {
		return err
	}
	if len(body) > MaxBody {
		return ErrTooLarge
	}
	if rev < 0 {
		return errNegative
	}
	return nil
}

// Update is a read-modify-write: change gets the current body (nil when absent) and returns
// the new one; a stale write is retried, at most maxRetries times, then ErrBusy. Nothing is
// written partially.
func Update(ctx context.Context, v Vol, path string, change func(body []byte) ([]byte, error)) error {
	for range maxRetries {
		body, rev, err := v.Read(ctx, path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		next, err := change(body)
		if err != nil {
			return err
		}
		if _, err := v.Write(ctx, path, next, rev); !errors.Is(err, ErrStale) {
			return err
		}
	}
	return ErrBusy
}
