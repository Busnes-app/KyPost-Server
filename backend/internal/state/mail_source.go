package state

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
)

var ErrMailSource = errors.New("mail source differs from durable account state; restore the original source or use a reviewed migration; source switching is disabled")

func validMailSource(source string) bool {
	if source == "imap" {
		return true
	}
	hash, err := hex.DecodeString(strings.TrimPrefix(source, "native:"))
	return strings.HasPrefix(source, "native:") && err == nil && len(hash) == 32
}

// NewNative is reserved for verified provisioning of a new account. An absent
// database in an existing directory is not evidence that no IDs were issued.
// Never use this constructor to migrate or repair an existing account.
func NewNative(baseDir, source string) (*Store, error) {
	if source == "imap" || !validMailSource(source) {
		return nil, ErrMailSource
	}
	if err := os.MkdirAll(filepath.Dir(baseDir), 0700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(baseDir, 0700); err != nil {
		return nil, err
	}
	// A schema left for this path is not a new account either.
	if dbbackend.Postgres() {
		if held, err := dbbackend.Exists(filepath.Join(baseDir, "state.db")); err != nil || held {
			return nil, errors.Join(ErrMailSource, err)
		}
	}
	return newWithMailSource(baseDir, source, false)
}

// OpenNative opens only an existing source-bound native state database.
// SQLite mode=rw refuses loss at the open boundary; validate the source before
// schema migration and never initialize or import legacy state here.
func OpenNative(baseDir, source string) (*Store, error) {
	if source == "imap" || !validMailSource(source) {
		return nil, ErrMailSource
	}
	return newWithMailSource(baseDir, source, true)
}

// BindMailSource admits only the source initialized at account creation/open.
// Old and recreated unbound state defaults to IMAP, even when it is empty.
func (s *Store) BindMailSource(source string) error {
	if !validMailSource(source) {
		return ErrMailSource
	}
	var current string
	if err := s.db.QueryRow("SELECT value FROM meta WHERE key='mail_source'").Scan(&current); err != nil {
		return err
	}
	if current != source {
		return ErrMailSource
	}
	return nil
}
