package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

// documentTableMode reports table mode: multi, or the explicit volume_stores record. A
// normal single install reads only that record.
func documentTableMode(ctx context.Context, stateDir, mode string) (bool, error) {
	if !dbbackend.Postgres() {
		if mode == config.TenancyMulti {
			return false, errors.New("table-mode documents require PostgreSQL")
		}
		return false, nil
	}
	rootPath := filepath.Join(stateDir, "state.db")
	exists, err := dbbackend.Exists(rootPath)
	if err != nil {
		return false, err
	}
	marker := ""
	if exists {
		st, err := dbbackend.Open(rootPath, dbbackend.Options{Kind: "state", ReadOnly: true})
		if err != nil {
			return false, err
		}
		err = st.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='volume_stores'`).Scan(&marker)
		closeErr := st.Close()
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		if closeErr != nil {
			return false, closeErr
		}
	}
	if marker != "" && marker != "postgres" {
		return false, errors.New("invalid volume_stores setting")
	}
	return mode == config.TenancyMulti || marker != "", nil
}

// Start selects table mode before any document store opens. A normal single install
// reads only its existing mode record and creates no volume store or master key. Table
// mode requires every provisioned key (MH5) and creates none until release.
func startDocuments(ctx context.Context, configDir, stateDir, secretDir, mode string) (func(), error) {
	noop := func() {}
	table, err := documentTableMode(ctx, stateDir, mode)
	if err != nil {
		return nil, err
	}
	if !table {
		return noop, nil
	}
	// Historical MH2 stores and capsules retain file account authority. MH8 owns
	// migration; never replace retained accounts with an empty/new table at startup.
	if _, err := os.Lstat(filepath.Join(configDir, "users.json")); err == nil {
		return nil, errors.New("table-mode startup found retained users.json; preserve it and complete the offline account migration before switching authority")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	keys := provisionedKeys(secretDir)
	for _, name := range slices.Sorted(maps.Keys(keys)) {
		if info, err := os.Stat(keys[name]); err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("table mode requires a provisioned %s in SECRET_DIR on every host", name)
		}
	}
	provisioned := append(slices.Collect(maps.Values(keys)), secretDir)
	for _, env := range []string{"PUSH_RELAY_KEY_FILE", "APNS_RELAY_KEY_FILE"} {
		if p := strings.TrimSpace(os.Getenv(env)); p != "" {
			provisioned = append(provisioned, p)
		}
	}
	unprovision := cryptutil.ProvisionKeys(provisioned...)
	started := false
	defer func() {
		if !started {
			unprovision()
		}
	}()
	master, err := cryptutil.LoadKey(keys["documents.key"])
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
	if mode != config.TenancyMulti {
		all, err := accounts.List()
		if err != nil {
			db.Close()
			return nil, err
		}
		if users.FirstAdminFrom(all).ID == "" {
			db.Close()
			return nil, errors.New("migrated table-mode accounts have no active administrator; preserve retained data and restore complete account authority before startup")
		}
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
	fence := state.VolFence(stateDir)
	releaseConfig, err := vol.MountFenced(configDir, configVol, fence)
	if err != nil {
		db.Close()
		return nil, err
	}
	releaseState, err := vol.MountFenced(stateDir, stateVol, fence)
	if err != nil {
		releaseConfig()
		db.Close()
		return nil, err
	}
	mounted, started = true, true
	return func() { releaseState(); releaseConfig(); releaseAccounts(); _ = db.Close(); unprovision() }, nil
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

// provisionedKeys are the key files, by name, that table mode requires on every host and
// fingerprints (replica.Fingerprint): the runtime would otherwise create each lazily, one
// per host. A *_FILE variable moves a key as it does in single mode. The bulk-backup restic
// key stays optional: LoadOrCreateKey refuses to create it when bulk backups first run.
func provisionedKeys(secretDir string) map[string]string {
	file := func(env, name string) string {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
		return filepath.Join(secretDir, name)
	}
	keys := map[string]string{
		"totp-secret.key":     file("TOTP_SECRET_KEY_FILE", "totp-secret.key"),
		"pow.key":             file("POW_SECRET_FILE", "pow.key"),
		"imap-config.key":     file("IMAP_CONFIG_KEY_FILE", "imap-config.key"),
		"pgp-private-key.key": file("PGP_PRIVATE_KEY_FILE", "pgp-private-key.key"),
		"pickup-store.key":    file("PICKUP_STORE_KEY_FILE", "pickup-store.key"),
		"pairing.key":         file("PAIRING_SECRET_FILE", "pairing.key"),
		"relay-identity.key":  file("RELAY_IDENTITY_KEY_FILE", "relay-identity.key"),
	}
	for _, name := range []string{"documents.key", "scheduled-send.key", "imap-import.key", "native-relay.key", tenancy.TombstoneKeyFile, "notifications-vapid-private.pem"} {
		keys[name] = filepath.Join(secretDir, name)
	}
	return keys
}
