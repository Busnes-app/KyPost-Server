package vol

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// Exists retains existence-only marker refusal on files, including symlinks and
// unreadable markers. Table read errors also refuse callers rather than imply absence.
func Exists(path string) (bool, error) {
	d := OpenDocument(path)
	if d.file != "" {
		_, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}
	_, err := d.Read()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (d *Document) SyncDir() error {
	if d.file == "" {
		return nil
	}
	return fsutil.SyncDir(filepath.Dir(d.file))
}

// MoveTo atomically publishes a platform marker at its new name. A table move
// deletes only the source revision the caller checked. Derived ciphertext cannot
// move this way: its authentication binds its original path and owner.
func (d *Document) MoveTo(path string) error {
	to := OpenDocument(path)
	if d.file != "" && to.file != "" {
		return os.Rename(d.file, to.file)
	}
	source, ok := d.volume.(*Table)
	target, targetOK := to.volume.(*Table)
	if !ok || !targetOK || source.db != target.db || source.root != target.root {
		return ErrPath
	}
	if own, err := owner(to.path); err != nil || own != "" {
		return ErrPath
	}
	ctx := context.Background()
	tx, err := source.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var body []byte
	err = tx.QueryRowContext(ctx, `DELETE FROM documents WHERE root=? AND path=? AND revision=? AND owner IS NULL RETURNING body`, source.root, d.path, d.revision).Scan(&body)
	if err != nil {
		return errors.Join(ErrStale, err)
	}
	revision, now := source.expressions()
	// Platform markers are wholly replaced; every publication still gets a fresh revision.
	q := `INSERT INTO documents(root,path,owner,body,revision,updated_at) VALUES(?,?,NULL,?,` + revision + `,` + now + `) ON CONFLICT(root,path) DO UPDATE SET body=excluded.body,revision=excluded.revision,updated_at=excluded.updated_at WHERE documents.owner IS NULL`
	result, err := tx.ExecContext(ctx, q, source.root, to.path, body)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errOwner
	}
	return tx.Commit()
}
