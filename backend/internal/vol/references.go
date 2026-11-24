package vol

import (
	"context"
	"slices"
)

// WriteWithReferences commits the body only while every referenced document
// exists for the same owner in this volume. File writes retain their behavior.
func (d *Document) WriteWithReferences(body []byte, references []string) error {
	if d.file != "" || len(references) == 0 {
		return d.Write(body)
	}
	table, ok := d.volume.(*Table)
	if !ok {
		return ErrPath
	}
	own, err := owner(d.path)
	if err != nil || own == "" {
		return errOwner
	}
	paths := make([]string, 0, len(references))
	for _, path := range references {
		ref := OpenDocument(path)
		target, ok := ref.volume.(*Table)
		if !ok || target.db != table.db || target.root != table.root {
			return ErrPath
		}
		if err := CheckPath(ref.path); err != nil {
			return err
		}
		refOwner, err := owner(ref.path)
		if err != nil || refOwner != own {
			return errOwner
		}
		paths = append(paths, ref.path)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	revision, err := table.write(context.Background(), d.path, body, d.revision, 0, paths...)
	if err == nil {
		d.revision = revision
	}
	return err
}

// RemoveIfUnchanged deletes the observed document only while its owner's guard
// document still has the observed revision. A changed reference set retries the
// whole sweep; a new photo incarnation never matches the old global revision.
func (d *Document) RemoveIfUnchanged(guard *Document) error {
	table, ok := d.volume.(*Table)
	if !ok {
		return ErrPath
	}
	guarded, ok := guard.volume.(*Table)
	if !ok || table.db != guarded.db || table.root != guarded.root {
		return ErrPath
	}
	if err := CheckPath(d.path); err != nil {
		return err
	}
	if err := CheckPath(guard.path); err != nil {
		return err
	}
	own, err := owner(d.path)
	if err != nil || own == "" {
		return errOwner
	}
	guardOwner, err := owner(guard.path)
	if err != nil || own != guardOwner {
		return errOwner
	}
	if d.revision <= 0 {
		return ErrStale
	}
	result, err := table.db.ExecContext(context.Background(), `DELETE FROM documents WHERE root=? AND path=? AND revision=? AND owner=? AND COALESCE((SELECT CASE WHEN guard.owner=? THEN guard.revision ELSE -1 END FROM documents AS guard WHERE guard.root=? AND guard.path=?),0)=?`, table.root, d.path, d.revision, own, own, table.root, guard.path, guard.revision)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		err = ErrStale
	}
	return err
}
