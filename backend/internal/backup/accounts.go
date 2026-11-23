package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"

	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

// Snapshot accounts come from exactly one authority. Early MH2 snapshots retain
// users.json; MH3 table snapshots carry account records only in volumes.db.
func snapshotAccountBytes(dir string) ([]byte, error) {
	raw, fileErr := os.ReadFile(filepath.Join(dir, "config/users.json"))
	if fileErr != nil && !errors.Is(fileErr, os.ErrNotExist) {
		return nil, fileErr
	}
	path := filepath.Join(dir, "state", volumes.File)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return raw, fileErr
	} else if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var present int
	if err = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='accounts'`).Scan(&present); err != nil {
		return nil, err
	}
	if present == 0 {
		return raw, fileErr
	}
	accounts, err := users.ReadAccountSnapshot(context.Background(), db)
	if err != nil {
		return nil, err
	}
	if fileErr == nil {
		if len(accounts) != 0 {
			return nil, errors.New("snapshot contains both file and table account authority")
		}
		return raw, nil
	}
	return json.Marshal(struct {
		Version int          `json:"version"`
		Users   []users.User `json:"users"`
	}{1, accounts})
}
