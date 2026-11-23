package vol

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// A mount is installed during table-mode startup and removed after its users stop.
// Per-host scratch and ordinary single-tenant installs retain their existing file I/O.
type mount struct {
	root   string
	volume Vol
}

var mounts struct {
	sync.RWMutex
	entries []*mount
}

// Mount attaches an already opened volume to one physical root. Store constructors
// resolve that root once. The release function affects only this mount.
func Mount(root string, volume Vol) (func(), error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if volume == nil {
		return nil, errors.New("vol: missing mounted volume")
	}
	m := &mount{root: abs, volume: volume}
	mounts.Lock()
	mounts.entries = append(mounts.entries, m)
	mounts.Unlock()
	return func() {
		mounts.Lock()
		defer mounts.Unlock()
		for i, entry := range mounts.entries {
			if entry == m {
				mounts.entries = append(mounts.entries[:i], mounts.entries[i+1:]...)
				return
			}
		}
	}, nil
}

// Document holds one store's chosen volume, path and last-read revision. Its owner
// serializes access within a process, as existing stores do with their mutexes.
type Document struct {
	volume   Vol
	path     string
	file     string
	revision int64
}

func OpenDocument(path string) *Document {
	abs, err := filepath.Abs(path)
	if err != nil {
		return &Document{volume: File{Dir: filepath.Dir(path)}, path: filepath.Base(path), file: path}
	}
	mounts.RLock()
	defer mounts.RUnlock()
	for i := len(mounts.entries) - 1; i >= 0; i-- {
		m := mounts.entries[i]
		rel, err := filepath.Rel(m.root, abs)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return &Document{volume: m.volume, path: filepath.ToSlash(rel)}
		}
	}
	return &Document{volume: File{Dir: filepath.Dir(path)}, path: filepath.Base(path), file: path}
}

// Mkdir preserves each file store's original directory mode. Table documents create
// no local directory, even on a replica with empty private roots.
func (d *Document) Mkdir(mode os.FileMode) error {
	if d.file == "" {
		return nil
	}
	return os.MkdirAll(filepath.Dir(d.file), mode)
}

func (d *Document) Read() ([]byte, error) {
	body, rev, err := d.volume.Read(context.Background(), d.path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		d.revision = rev
	}
	return body, err
}

func (d *Document) Write(body []byte) error {
	if d.file != "" {
		return fsutil.AtomicWriteFile(d.file, body, 0o600)
	}
	rev, err := d.volume.Write(context.Background(), d.path, body, d.revision)
	if err == nil {
		d.revision = rev
	}
	return err
}

func (d *Document) Lock() (func(), error) {
	if d.file != "" {
		return fsutil.LockFile(d.file)
	}
	return func() {}, nil
}

// Retry repeats the entire store operation after a failed CAS. Its next read discards
// the failed attempt's in-memory edits and applies the mutation to the winning version.
func Retry(operation func() error) error {
	for range maxRetries {
		err := operation()
		if !errors.Is(err, ErrStale) {
			return err
		}
	}
	return ErrBusy
}

// LoadJSON preserves file-mode initialization and refresh behavior. A table document
// removed by another replica clears its cache, so an old record cannot remain served.
func LoadJSON[V any](d *Document, apply func(V), onMissing func() error) error {
	if d.file != "" {
		return fsutil.LoadJSONFile(d.file, apply, onMissing)
	}
	b, err := d.Read()
	if errors.Is(err, os.ErrNotExist) {
		if onMissing != nil {
			return onMissing()
		}
		var zero V
		apply(zero)
		return nil
	}
	if err != nil {
		return err
	}
	var value V
	if err := json.Unmarshal(b, &value); err != nil {
		return err
	}
	apply(value)
	return nil
}

func PersistJSON[V any](d *Document, value V) error {
	if d.file != "" {
		return fsutil.PersistJSONFile(d.file, value)
	}
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return d.Write(b)
}

// Replace applies a whole-document replacement at this attempt's revision.
func (d *Document) Replace(body []byte) error {
	if d.file != "" {
		return d.Write(body)
	}
	return Retry(func() error {
		if _, err := d.Read(); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return d.Write(body)
	})
}

// Version distinguishes a shared table revision from a host-local file stamp.
func (d *Document) Version() (revision int64, table bool, err error) {
	if d.file != "" {
		return 0, false, nil
	}
	_, err = d.Read()
	return d.revision, true, err
}

// Update runs a store's complete read-modify-write callback, retrying a stale CAS.
// Existing file stores retain their original inter-process lock.
func (d *Document) Update(operation func() error) error {
	return Retry(func() error {
		release, err := d.Lock()
		if err != nil {
			return err
		}
		defer release()
		return operation()
	})
}

func (d *Document) IsTable() bool { return d.file == "" }
