package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

// Start selects table mode before any document store opens. A normal single install
// reads only its existing mode record and creates no volume store or master key.
func startDocuments(ctx context.Context, configDir, stateDir, secretDir, mode string) (func(), error) {
	noop := func() {}
	if !dbbackend.Postgres() {
		if mode == config.TenancyMulti {
			return nil, errors.New("table-mode documents require PostgreSQL")
		}
		return noop, nil
	}
	rootPath := filepath.Join(stateDir, "state.db")
	exists, err := dbbackend.Exists(rootPath)
	if err != nil {
		return nil, err
	}
	marker := ""
	if exists {
		st, err := dbbackend.Open(rootPath, dbbackend.Options{Kind: "state", ReadOnly: true})
		if err != nil {
			return nil, err
		}
		err = st.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='volume_stores'`).Scan(&marker)
		closeErr := st.Close()
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	if mode != config.TenancyMulti && marker == "" {
		return noop, nil
	}
	if marker != "" && marker != "postgres" {
		return nil, errors.New("invalid volume_stores setting")
	}
	master, err := cryptutil.LoadKey(filepath.Join(secretDir, "documents.key"))
	if err != nil {
		return nil, errors.New("table-mode documents require a provisioned documents.key")
	}
	db, err := volumes.Open(stateDir, mode == config.TenancyMulti)
	if err != nil {
		return nil, err
	}
	releaseAccounts, err := users.MountAccounts(configDir, stateDir)
	if err != nil {
		db.Close()
		return nil, err
	}
	mounted := false
	defer func() {
		if !mounted {
			releaseAccounts()
		}
	}()
	accounts, err := users.OpenExisting(ctx, configDir)
	if err != nil {
		db.Close()
		return nil, err
	}
	options := documentTableOptions(configDir, mode, master, accounts)
	configVol, err := vol.NewTable(db, "config", options)
	if err != nil {
		db.Close()
		return nil, err
	}
	stateVol, err := vol.NewTable(db, "state", options)
	if err != nil {
		db.Close()
		return nil, err
	}
	releaseConfig, err := vol.Mount(configDir, configVol)
	if err != nil {
		db.Close()
		return nil, err
	}
	releaseState, err := vol.Mount(stateDir, stateVol)
	if err != nil {
		releaseConfig()
		db.Close()
		return nil, err
	}
	mounted = true
	return func() { releaseState(); releaseConfig(); releaseAccounts(); _ = db.Close() }, nil
}

// TableOptions derives ownership from retained identities, never request fields.
// MH3 replaces file accounts and adds its same-transaction owner-row guard.
func documentTableOptions(configDir, mode string, master []byte, accounts *users.Store) vol.TableOptions {
	account := accounts.Get

	return vol.TableOptions{MasterKey: master,
		Org: func(_ context.Context, id string) (string, error) {
			u, err := account(id)
			if err != nil {
				return "", err
			}
			if mode == config.TenancyMulti && !config.ValidOrgID(u.OrgID) {
				return "", errors.New("document owner has no org binding")
			}
			return u.OrgID, nil
		},
		Owner: func(_ context.Context, path string, body []byte) (string, error) {
			if strings.HasPrefix(path, "pickup/") {
				var record struct {
					Sender string `json:"senderUserId"`
				}
				if json.Unmarshal(body, &record) != nil || !fsutil.SafePathComponent(record.Sender) {
					return "", errors.New("invalid pickup document owner")
				}
				_, err := account(record.Sender)
				return record.Sender, err
			}
			if strings.HasPrefix(path, "mailboxes/") {
				rest := strings.TrimPrefix(path, "mailboxes/")
				id, _, ok := strings.Cut(rest, "/")
				if !ok || !fsutil.SafePathComponent(id) {
					return "", vol.ErrPath
				}
				a, found, err := sso.NewLifecycleStore(configDir).NativeMailboxAssignment(id)
				if err != nil {
					return "", err
				}
				if !found {
					return "", os.ErrNotExist
				}
				u, err := accounts.GetBySSOSubIssuer(a.Owner.Issuer, a.Owner.Subject)
				return u.ID, err
			}
			return "", vol.ErrPath
		},
	}
}
