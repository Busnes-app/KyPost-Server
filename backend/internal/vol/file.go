package vol

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// File keeps documents as files under Dir, exactly as the stores write them today. It does
// not check revisions: a store's own flock orders its read-modify-write on one volume. A
// present file reads as revision 1.
type File struct{ Dir string }

func (f File) Read(_ context.Context, path string) ([]byte, int64, error) {
	if err := CheckPath(path); err != nil {
		return nil, 0, err
	}
	b, err := os.ReadFile(filepath.Join(f.Dir, filepath.FromSlash(path)))
	if err != nil {
		return nil, 0, err
	}
	return b, 1, nil
}

func (f File) Write(_ context.Context, path string, body []byte, rev int64) (int64, error) {
	if err := checkWrite(path, body, rev); err != nil {
		return 0, err
	}
	return 1, fsutil.AtomicWriteFile(filepath.Join(f.Dir, filepath.FromSlash(path)), body, 0o600)
}

func (f File) Remove(_ context.Context, path string, _ int64) error {
	if err := CheckPath(path); err != nil {
		return err
	}
	return os.Remove(filepath.Join(f.Dir, filepath.FromSlash(path)))
}

func (f File) List(_ context.Context, prefix string) ([]string, error) {
	if err := CheckPath(prefix); err != nil {
		return nil, err
	}
	root := filepath.Join(f.Dir, filepath.FromSlash(prefix))
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if p == root && errors.Is(err, fs.ErrNotExist) {
			return fs.SkipAll
		}
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(f.Dir, p)
		out = append(out, filepath.ToSlash(rel))
		return err
	})
	slices.Sort(out)
	return out, err
}
