package ingress

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"syscall"
)

// ponytail: admission reserves are conservative estimates, not a filesystem
// quota. Shared-volume writers and pinned WAL during recovery still need an
// operator volume quota; never evict accepted mail to meet a byte budget.
func (s *Store) physicalBytes() (int64, error) {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Lstat(s.path + suffix)
		if suffix != "" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > math.MaxInt64-total {
			return 0, errors.New("cannot measure receiving storage; preserve mail and repair storage before retrying")
		}
		total += info.Size()
	}
	return total, nil
}

// A checkpoint may release a recycled WAL after a backup reader has finished.
// It must run before BEGIN IMMEDIATE. A busy reader is reported by SQLite's
// result, and admission still checks actual bytes under the writer transaction.
func (s *Store) admissionTx(ctx context.Context) (*sql.Tx, error) {
	used, err := s.physicalBytes()
	if err != nil {
		return nil, err
	}
	if used >= s.physicalLimit-2*s.limits.MessageBytes-(2<<20) && s.checkpointHeadroom() == nil {
		var busy, pages, checkpointed int
		if err := s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &pages, &checkpointed); err != nil {
			return nil, errors.New("receiving checkpoint failed; preserve mail and repair storage before retrying")
		}
		if busy != 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	// Insufficient or unreadable checkpoint headroom skips reclamation. Exact
	// replays may still succeed; checkAdmission refuses growth under the txn.
	return s.db.BeginTx(ctx, nil)
}

// Call only inside an immediate writer transaction, before genuine growth.
// Exact receipt/binding replays and recovery operations bypass new admission.
func (s *Store) checkAdmission(ctx context.Context, tx *sql.Tx, payload int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var pageSize int64
	if err := tx.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return err
	}
	if pageSize < 512 || pageSize > 65536 || pageSize&(pageSize-1) != 0 || payload < 0 || payload > s.limits.MessageBytes {
		return errors.New("invalid receiving storage geometry; preserve mail and repair storage")
	}
	growth := 32*pageSize + 2*payload
	used, err := s.physicalBytes()
	if err != nil {
		return err
	}
	if used > s.physicalLimit-growth {
		return fmt.Errorf("%w; receiving database/WAL reserve exhausted", ErrCapacity)
	}
	if used >= s.physicalLimit-2*s.limits.MessageBytes-(2<<20) {
		if err := s.checkpointHeadroom(); err != nil {
			return err
		}
	}
	return receivingFreeSpace(filepath.Dir(s.path), uint64((16<<20)+growth))
}

// Checkpoint copies pages to the main file before releasing WAL blocks. Reserve
// the whole WAL length as conservative copy growth, plus recovery headroom.
func (s *Store) checkpointHeadroom() error {
	info, err := os.Lstat(s.path + "-wal")
	if errors.Is(err, os.ErrNotExist) {
		return receivingFreeSpace(filepath.Dir(s.path), 16<<20)
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 {
		return errors.New("cannot measure receiving checkpoint growth; preserve mail and repair storage before retrying")
	}
	return receivingFreeSpace(filepath.Dir(s.path), uint64(info.Size())+(16<<20))
}

func receivingFreeSpace(dir string, required uint64) error {
	var disk syscall.Statfs_t
	if err := syscall.Statfs(dir, &disk); err != nil || disk.Bsize <= 0 {
		return errors.New("cannot measure receiving free space; preserve mail and repair storage before retrying")
	}
	// Division avoids overflowing the kernel's block-count multiplication.
	blockSize := uint64(disk.Bsize)
	blocks := required / blockSize
	if required%blockSize != 0 {
		blocks++
	}
	if disk.Bavail < blocks {
		return fmt.Errorf("%w; receiving free-space reserve exhausted; free space before retrying", ErrCapacity)
	}
	return nil
}
