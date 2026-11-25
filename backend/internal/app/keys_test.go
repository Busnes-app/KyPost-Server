package app

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

// provisionTableKeys writes every key table mode requires that is not there yet, as an
// operator would.
func provisionTableKeys(t *testing.T, secretDir string) {
	t.Helper()
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, path := range provisionedKeys(secretDir) {
		if _, err := os.Lstat(path); err == nil {
			continue
		}
		body := []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte(name[:1]), 32)))
		if strings.HasSuffix(name, ".pem") {
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			der, err := x509.MarshalECPrivateKey(key)
			if err != nil {
				t.Fatal(err)
			}
			body = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Table mode starts only with every key provisioned, names a missing one, and creates no
// key while it runs; a single install still creates keys lazily.
func TestTableModeRequiresProvisionedKeysAndCreatesNone(t *testing.T) {
	dbbackend.RequirePostgres(t)
	root := t.TempDir()
	configDir, stateDir, secretDir := filepath.Join(root, "config"), filepath.Join(root, "state"), filepath.Join(root, "private")
	st, err := state.New(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSetting("volume_stores", "postgres"); err != nil {
		t.Fatal(err)
	}
	db, err := volumes.Open(stateDir, true)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	provisionTableKeys(t, secretDir)
	releaseAccounts, err := users.MountAccounts(configDir, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := users.OpenExisting(context.Background(), configDir)
	if err == nil {
		_, err = accounts.CreateSSOUser("retained-admin", users.RoleAdmin, "", "", "")
	}
	releaseAccounts()
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range provisionedKeys(secretDir) {
		held, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		release, err := startDocuments(context.Background(), configDir, stateDir, secretDir, config.TenancySingle)
		if err == nil {
			release()
			t.Fatal("table mode started without", name)
		}
		if !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), string(held)) {
			t.Fatal("refusal does not name the key alone", name, err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) || users.TableMode(configDir) {
			t.Fatal("refused start created a key or left a mount", name, err)
		}
		if err := os.WriteFile(path, held, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	release, err := startDocuments(context.Background(), configDir, stateDir, secretDir, config.TenancySingle)
	if err != nil {
		t.Fatal(err)
	}
	lazy := filepath.Join(secretDir, "scheduled-send.key")
	if err := os.Remove(lazy); err != nil {
		t.Fatal(err)
	}
	if _, err := cryptutil.LoadOrCreateKey(lazy); !errors.Is(err, cryptutil.ErrKeyNotProvisioned) {
		t.Fatal("table mode created a key at runtime", err)
	}
	if _, err := cryptutil.LoadOrCreateKey(provisionedKeys(secretDir)["pow.key"]); err != nil {
		t.Fatal("a provisioned key did not load", err)
	}
	release()
	if _, err := cryptutil.LoadOrCreateKey(lazy); err != nil {
		t.Fatal("after release a key is created lazily again", err)
	}
}
