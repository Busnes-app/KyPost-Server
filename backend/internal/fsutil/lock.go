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

// LockFile holds an exclusive advisory lock covering path, serializing a whole
// read-modify-write cycle against every other process using it for the same
// path. The caller must defer the returned release immediately.
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
func LockFile(path string) (release func(), err error) {
	return LockFileContext(context.Background(), path)
}

// LockFileContext cancels contention without abandoning a goroutine holding a
// future lock. Filesystem open/fsync itself still requires a healthy volume.
func LockFileContext(ctx context.Context, path string) (release func(), err error) {
	return lockSibling(ctx, path, ".lock", syscall.LOCK_EX)
}

// LockFileShared holds path's lock together with other LockFileShared
// holders, excluding LockFileGated. Use it only for a read-only section.
func LockFileShared(ctx context.Context, path string) (release func(), err error) {
	return lockGated(ctx, path, syscall.LOCK_SH)
}

// LockFileGated is LockFileContext for a path that also has LockFileShared
// holders. Both first pass "<path>.gate", which a waiting writer keeps, so a
// stream of overlapping readers cannot starve it: flock itself has no writer
// preference. A LockFileContext on the same path still excludes both.
func LockFileGated(ctx context.Context, path string) (release func(), err error) {
	return lockGated(ctx, path, syscall.LOCK_EX)
}

func lockGated(ctx context.Context, path string, how int) (func(), error) {
	pass, err := lockSibling(ctx, path, ".gate", how)
	if err != nil {
		return nil, err
	}
	defer pass()
	return lockSibling(ctx, path, ".lock", how)
}

func lockSibling(ctx context.Context, path, suffix string, how int) (release func(), err error) {
	// Per-user state dirs are created lazily by the stores' own save paths, so
	// on a first-ever write the directory may not exist yet.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create dir for lock file %s: %w", path, err)
	}
	f, err := os.OpenFile(path+suffix, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file for %s: %w", path, err)
	}
	// Never a blocking flock: it holds an OS thread for the whole wait, and
	// enough waiters behind a busy fence take the process past its task
	// limit, where Go aborts. Poll instead, backing off to 16 ms.
	wait := time.Millisecond
	for {
		if err = ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		err = syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		timer := time.NewTimer(wait)
		wait = min(2*wait, 16*time.Millisecond)
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
