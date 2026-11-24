package contacts

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/Busnes-app/kypost-server/backend/internal/vol"
)

// photoSweepSnapshot is a test hook after the reference snapshot.
var photoSweepSnapshot func()

// SweepPhotos reclaims unreferenced table-mode photos. Ordinary installs keep
// their existing API file sweep.
func (s *Store) SweepPhotos(ctx context.Context) error {
	if !s.document.IsTable() {
		return errors.New("table photo sweep requires table documents")
	}
	return vol.Retry(func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.refreshFromDiskLocked(); err != nil {
			return err
		}
		referenced := map[string]bool{}
		for _, c := range s.contacts {
			if !c.Deleted && c.PhotoRef != "" {
				referenced[c.PhotoRef] = true
			}
		}
		if photoSweepSnapshot != nil {
			photoSweepSnapshot()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		dir := filepath.Join(s.baseDir, "contact-photos")
		names, err := vol.ListFiles(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			if referenced[name] {
				continue
			}
			photo := vol.OpenDocument(filepath.Join(dir, name))
			if _, err := photo.Read(); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				return err
			}
			if err := photo.RemoveIfUnchanged(s.document); err != nil {
				return err
			}
		}
		return nil
	})
}
