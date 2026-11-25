package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/ky-primitives/recoverykey"
	"github.com/Busnes-app/kypost-server/backend/internal/backup"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

// Restore a real sealed MH2-shaped capsule: documents, file accounts and a
// per-user device store. Runtime refusal must preserve the restored authority.
func TestHistoricalDocumentCapsuleRetainsAccountsThroughStartupRefusal(t *testing.T) {
	dbbackend.RequirePostgres(t)
	ctx := context.Background()
	retained := []users.User{
		{ID: "retained-admin", Username: "retained-admin", Role: users.RoleAdmin, Active: true},
		{ID: "retained-member", Username: "retained-member", Role: users.RoleUser, Active: true, SSOIssuer: "https://identity.example", SSOSub: "retained-subject"},
	}
	accountBytes, err := json.Marshal(struct {
		Version int          `json:"version"`
		Users   []users.User `json:"users"`
	}{1, retained})
	if err != nil {
		t.Fatal(err)
	}
	var files []recoveryclient.File
	var devices []state.NativeDevice
	t.Run("historical SQLite payload", func(t *testing.T) {
		dbbackend.SetURLForTest(t, "")
		root := t.TempDir()
		st, err := state.New(filepath.Join(root, "state"))
		if err != nil {
			t.Fatal(err)
		}
		if err = st.SetSetting("volume_stores", "postgres"); err != nil {
			t.Fatal(err)
		}
		if err = st.Close(); err != nil {
			t.Fatal(err)
		}
		db, err := volumes.Open(filepath.Join(root, "state"), true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO documents(root,path,revision,body,updated_at) VALUES('config','TUNING.md',1,?,1)`, []byte("retained platform tuning")); err != nil {
			t.Fatal(err)
		}
		// MH2 predates account rows. Import upgrades the schema on a private copy.
		if _, err = db.Exec(`DROP TABLE accounts`); err != nil {
			t.Fatal(err)
		}
		if err = db.Close(); err != nil {
			t.Fatal(err)
		}
		deviceStore, err := state.New(filepath.Join(root, "state", "users", retained[1].ID))
		if err != nil {
			t.Fatal(err)
		}
		if err = deviceStore.UpsertNativeDevice(state.NativeDevice{DeviceID: "retained-device", Platform: "android", UserID: retained[1].ID, PushToken: "synthetic-retained-token"}); err != nil {
			t.Fatal(err)
		}
		devices, err = deviceStore.ListNativeDevicesStrict()
		if err != nil {
			t.Fatal(err)
		}
		if err = deviceStore.Close(); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"state/state.db", "state/volumes.db", "state/users/retained-member/state.db"} {
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, recoveryclient.File{Path: path, Data: raw, Mode: 0600})
		}
	})
	files = append(files, recoveryclient.File{Path: "config/users.json", Data: accountBytes, Mode: 0600}, recoveryclient.File{Path: "private/documents.key", Data: []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32))), Mode: 0600})
	key, err := recoverykey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	shares, err := recoverykey.Split(key, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := recoveryclient.Seal(recoveryclient.Payload{ServiceName: backup.AppName, AppVersion: "historical-MH2", Files: files}, recoveryclient.RecoveryKey{Public: key.Public(), Threshold: 2, TotalShares: 3})
	if err != nil {
		t.Fatal(err)
	}
	capsulePath := filepath.Join(t.TempDir(), "historical.kycap")
	if err = os.WriteFile(capsulePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	configDir, stateDir, secretDir := filepath.Join(target, "config"), filepath.Join(target, "state"), filepath.Join(target, "private")
	t.Setenv("CONFIG_DIR", configDir)
	t.Setenv("STATE_DIR", stateDir)
	dbbackend.FreshDatabaseForTest(t)
	var out bytes.Buffer
	if err = runRestore([]string{capsulePath, target}, strings.NewReader(shares[0].String()+"\n"+shares[2].String()+"\n"), &out); err != nil {
		t.Fatal("historical restore", err)
	}
	before, err := dbbackend.RowCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		release, err := startDocuments(ctx, configDir, stateDir, secretDir, config.TenancySingle)
		if err == nil {
			release()
			t.Fatal("restored file authority silently replaced by table accounts")
		}
		if !strings.Contains(err.Error(), "users.json") || users.TableMode(configDir) {
			t.Fatal("unsafe refusal", err)
		}
		if actual, err := os.ReadFile(filepath.Join(configDir, "users.json")); err != nil || !bytes.Equal(actual, accountBytes) {
			t.Fatal("restored accounts changed", err)
		}
		if after, err := dbbackend.RowCounts(ctx); err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("refusal/retry changed databases", err)
		}
	}
	deviceStore, err := state.New(filepath.Join(stateDir, "users", retained[1].ID))
	if err != nil {
		t.Fatal(err)
	}
	defer deviceStore.Close()
	if actual, err := deviceStore.ListNativeDevicesStrict(); err != nil || !reflect.DeepEqual(actual, devices) {
		t.Fatal("legacy device authority changed", actual, err)
	}
	// Simulate the completed, stopped MH8 authority conversion, not a runtime
	// migration: routing columns retain every account/issuer and the file is kept.
	db, err := volumes.Open(stateDir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, u := range retained {
		record, _ := json.Marshal(u)
		var issuer, subject any
		if u.SSOIssuer != "" {
			issuer, subject = u.SSOIssuer, u.SSOSub
		}
		if _, err = db.Exec(`INSERT INTO accounts(id,username,issuer,subject,record) VALUES(?,?,?,?,?)`, u.ID, u.Username, issuer, subject, record); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.Rename(filepath.Join(configDir, "users.json"), filepath.Join(configDir, "users.json.retained")); err != nil {
		t.Fatal(err)
	}
	provisionTableKeys(t, secretDir)
	release, err := startDocuments(ctx, configDir, stateDir, secretDir, config.TenancySingle)
	if err != nil {
		t.Fatal("retry after explicit conversion", err)
	}
	defer release()
	if raw, err := vol.OpenDocument(filepath.Join(configDir, "TUNING.md")).Read(); err != nil || string(raw) != "retained platform tuning" {
		t.Fatal("retained table document", err)
	}
	accounts, err := users.OpenExisting(ctx, configDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range retained {
		got, err := accounts.Get(want.ID)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("converted account changed", got, err)
		}
	}
}

func TestTableModeRefusesIncompleteAccountLayouts(t *testing.T) {
	dbbackend.RequirePostgres(t)
	for _, layout := range []string{"empty-table", "corrupt-table", "empty-file", "corrupt-file", "empty-account-file", "partial-account-file"} {
		t.Run(layout, func(t *testing.T) {
			root := t.TempDir()
			configDir, stateDir, secretDir := filepath.Join(root, "config"), filepath.Join(root, "state"), filepath.Join(root, "private")
			t.Setenv("CONFIG_DIR", configDir)
			t.Setenv("STATE_DIR", stateDir)
			dbbackend.FreshDatabaseForTest(t)
			st, err := state.New(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if err = st.SetSetting("volume_stores", "postgres"); err != nil {
				t.Fatal(err)
			}
			db, err := volumes.Open(stateDir, true)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			provisionTableKeys(t, secretDir)
			if layout == "corrupt-table" {
				if _, err = db.Exec(`INSERT INTO accounts(id,username,record) VALUES('broken','broken',?)`, []byte(`{"id":"wrong"}`)); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasSuffix(layout, "file") {
				if err = os.MkdirAll(configDir, 0700); err != nil {
					t.Fatal(err)
				}
				value := map[string]string{"empty-file": "", "corrupt-file": "{", "empty-account-file": `{"version":1,"users":[]}`, "partial-account-file": `{"version":1,"users":[{"id":"partial"}]}`}[layout]
				if err = os.WriteFile(filepath.Join(configDir, "users.json"), []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := dbbackend.RowCounts(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				release, err := startDocuments(context.Background(), configDir, stateDir, secretDir, config.TenancySingle)
				if err == nil {
					release()
					t.Fatal("incomplete account authority accepted", layout)
				}
				if users.TableMode(configDir) {
					t.Fatal("refusal left table mounted")
				}
			}
			if after, err := dbbackend.RowCounts(context.Background()); err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("refusal changed database", err)
			}
		})
	}
}
