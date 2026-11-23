package vol

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrDirectoryFull is an aggregate document-directory limit refused in the same
// transaction as the new row. Photo writers therefore share one cap across replicas.
var ErrDirectoryFull = errors.New("contact photo storage is full for this account; remove some photos and try again")

func (t *Table) directoryBytes(ctx context.Context, db keyReader, prefix string) (int64, error) {
	text := `convert_from(body,'UTF8')::jsonb->>'ciphertext'`
	if t.snapshot {
		text = `json_extract(CAST(body AS TEXT),'$.ciphertext')`
	}
	// GCM ciphertext is plaintext plus its 16-byte tag. Envelopes use standard base64.
	size := `octet_length(decode(` + text + `,'base64'))-16`
	if t.snapshot {
		size = `(length(` + text + `)*3/4-CASE WHEN substr(` + text + `,-2)='==' THEN 2 WHEN substr(` + text + `,-1)='=' THEN 1 ELSE 0 END-16)`
	}
	plainSize := `octet_length(body)`
	if t.snapshot {
		plainSize = `length(body)`
	}
	var total sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT sum(CASE WHEN owner IS NULL THEN `+plainSize+` ELSE `+size+` END) FROM documents WHERE root=? AND path>? AND path<?`, t.root, prefix+"/", prefix+"0").Scan(&total)
	return total.Int64, err
}

// DirectoryBytes totals the stored plaintext lengths without fetching photo bodies
// across the network. File stores retain their existing directory metadata calculation.
func DirectoryBytes(dir string) (int64, error) {
	doc := OpenDocument(filepath.Join(dir, ".quota"))
	if table, ok := doc.volume.(*Table); ok {
		return table.directoryBytes(context.Background(), table.db, filepath.ToSlash(filepath.Dir(doc.path)))
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err == nil {
			total += info.Size()
		}
	}
	return total, nil
}

func (d *Document) WriteWithDirectoryLimit(body []byte, limit int64) error {
	if table, ok := d.volume.(*Table); ok {
		rev, err := table.write(context.Background(), d.path, body, d.revision, limit)
		if err == nil {
			d.revision = rev
		}
		return err
	}
	return d.Write(body)
}

// ListFiles lists direct children only, as the photo and pickup stores do on disk.
func ListFiles(dir string) ([]string, error) {
	doc := OpenDocument(filepath.Join(dir, ".listing"))
	if doc.file != "" {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		var names []string
		for _, entry := range entries {
			if !entry.IsDir() {
				names = append(names, entry.Name())
			}
		}
		return names, nil
	}
	prefix := filepath.ToSlash(filepath.Dir(doc.path))
	paths, err := doc.volume.List(context.Background(), prefix)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, path := range paths {
		name := strings.TrimPrefix(path, prefix+"/")
		if !strings.Contains(name, "/") {
			names = append(names, name)
		}
	}
	return names, nil
}

func (d *Document) Remove() error {
	if d.file != "" {
		return os.Remove(d.file)
	}
	return d.volume.Remove(context.Background(), d.path, d.revision)
}

// RemoveFile performs an intentional unconditional removal, such as credential
// revocation. Sweepers use Document.Remove with their observed revision instead.
func RemoveFile(path string) error {
	d := OpenDocument(path)
	if d.file != "" {
		return os.Remove(d.file)
	}
	return Retry(func() error {
		if _, err := d.Read(); err != nil {
			return err
		}
		return d.Remove()
	})
}
