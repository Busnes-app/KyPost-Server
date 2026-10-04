package fsutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// WithFileLock runs fn while holding an exclusive advisory lock covering
// path, serializing a whole read-modify-write cycle against every other
// process using this helper for the same path.
//
// A sync.Mutex is not enough: supervisord runs `--mode server` and
// `--mode daemon` as two processes that both write users.json, every per-user
// state file, and the WKD claims file. Re-reading before writing narrows the
// lost-update window; only holding a lock across the whole cycle closes it.
//
// The lock must be on the sibling "<path>.lock", never on path itself — the
// stores publish by rename (AtomicWriteFile), so a lock on the original inode
// detaches the moment a write lands. The lock file is never unlinked, for the
// same reason: two processes would end up holding locks on two inodes.
//
// Readers do not need this. Rename-publish means a reader sees either the
// whole previous file or the whole next one.
func WithFileLock(path string, fn func() error) error {
	release, err := LockFile(path)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

// LockFile is WithFileLock for callers whose return signature does not fit a
// func() error closure. The caller must defer the returned release
// immediately. Prefer WithFileLock where the closure form fits; it cannot be
// misused by forgetting the defer.
func LockFile(path string) (release func(), err error) {
	return LockFileContext(context.Background(), path)
}

// LockFileContext cancels contention without abandoning a goroutine holding a
// future lock. Filesystem open/fsync itself still requires a healthy volume.
func LockFileContext(ctx context.Context, path string) (release func(), err error) {
	// Per-user state dirs are created lazily by the stores' own save paths, so
	// on a first-ever write the directory may not exist yet.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create dir for lock file %s: %w", path, err)
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file for %s: %w", path, err)
	}
	for {
		if err = ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		flags := syscall.LOCK_EX
		if ctx.Done() != nil {
			flags |= syscall.LOCK_NB
		}
		err = syscall.Flock(int(f.Fd()), flags)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if err = ctx.Err(); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
