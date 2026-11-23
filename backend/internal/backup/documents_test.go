package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

func TestSealedDocumentsSurviveBackupAndRestore(t *testing.T) {
	s, u := nativeService(t)
	td, err := tenancy.Open(s.dirs.State, true)
	if err != nil {
		t.Fatal(err)
	}
	defer td.Close()
	if err := tenancy.BindUser(context.Background(), td, u.ID, keptOrg); err != nil {
		t.Fatal(err)
	}
	master, err := cryptutil.LoadOrCreateKey(filepath.Join(s.dirs.Secret, "documents.key"))
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{91}, 32)
	seal := func(body, key []byte, identity []string) []byte {
		aad, _ := json.Marshal(identity)
		env, err := cryptutil.SealAssociated(body, key, aad)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	db, err := volumes.Open(s.dirs.State, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	wrapped := seal(key, master, []string{"kypost:document-key:1", keptOrg})
	if _, err := db.Exec(`INSERT INTO doc_keys(org_id,wrapped,created_at) VALUES(?,?,1)`, keptOrg, wrapped); err != nil {
		t.Fatal(err)
	}
	path := "users/" + u.ID + "/contacts.json"
	plain := []byte(`{"name":"Sealed Contact","email":"sealed@example.test"}`)
	body := seal(plain, key, []string{"kypost:document:1", "state", path, u.ID, keptOrg})
	if _, err := db.Exec(`INSERT INTO documents(root,path,owner,revision,body,updated_at) VALUES('state',?,?,41,?,1)`, path, u.ID, body); err != nil {
		t.Fatal(err)
	}
	recoveryKey := pinTestKey(t, s)
	result, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(result.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if _, _, err := capsule.Open(raw, recoveryKey, restored); err != nil {
		t.Fatal(err)
	}
	if err := validateDocumentSnapshot(restored); err != nil {
		t.Fatal("restored documents", err)
	}
	// The SQLite capsule member contains only ciphertext, including its WAL contents.
	snapshot, err := os.ReadFile(filepath.Join(restored, "state", volumes.File))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(snapshot, []byte("sealed@example.test")) || bytes.Contains(snapshot, []byte("Sealed Contact")) {
		t.Fatal("plaintext in capsule database")
	}
	if _, err := s.Drill(context.Background()); err != nil {
		t.Fatal("drill", err)
	}
	staged, err := sql.Open("sqlite", filepath.Join(restored, "state", volumes.File))
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	if _, err := staged.Exec(`UPDATE documents SET path=?`, "users/"+u.ID+"/groups.json"); err != nil {
		t.Fatal(err)
	}
	if err := validateDocumentSnapshot(restored); err == nil {
		t.Fatal("restore accepted document moved to another path")
	}
	if _, err := staged.Exec(`UPDATE documents SET path=?`, path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(restored, "private", "documents.key")); err != nil {
		t.Fatal(err)
	}
	if err := validateDocumentSnapshot(restored); err == nil {
		t.Fatal("restore accepted sealed documents without key")
	}
	if err := os.Remove(filepath.Join(s.dirs.Secret, "documents.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(context.Background()); err == nil {
		t.Fatal("backup sealed documents without the master key")
	}
}

func TestTableDocumentsBackupRestore(t *testing.T) {
	dbbackend.RequirePostgres(t)
	s, u := nativeService(t)
	master, err := cryptutil.LoadOrCreateKey(filepath.Join(s.dirs.Secret, "documents.key"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := volumes.Open(s.dirs.State, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := users.LoadOrMigrate(context.Background(), s.dirs.Config, "")
	if err != nil {
		t.Fatal(err)
	}
	all, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	// The legacy fixture has an IMAP document for u1 and a pre-issuer native
	// link. Make both identities explicit before selecting account tables.
	all = append(all, users.User{ID: "u1", Username: "legacy", Role: users.RoleUser, Active: true})
	for _, u := range all {
		if u.SSOIssuer == "" && u.SSOSub != "" {
			u.SSOIssuer = u.NativeMailboxIssuer
		}
		raw, err := json.Marshal(u)
		if err != nil {
			t.Fatal(err)
		}
		var issuer, subject any
		if u.SSOIssuer != "" {
			issuer, subject = u.SSOIssuer, u.SSOSub
		}
		if _, err = db.Exec(`INSERT INTO accounts(id,username,issuer,subject,record) VALUES(?,?,?,?,?)`, u.ID, users.NormalizeUsername(u.Username), issuer, subject, raw); err != nil {
			t.Fatal(err)
		}
	}
	releaseAccounts, err := users.MountAccounts(s.dirs.Config, s.dirs.State)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseAccounts()
	options := vol.TableOptions{MasterKey: master, Org: func(context.Context, string) (string, error) { return "", nil }}
	for _, root := range []struct{ name, path string }{{"config", s.dirs.Config}, {"state", s.dirs.State}} {
		table, err := vol.NewTable(db, root.name, options)
		if err != nil {
			t.Fatal(err)
		}
		err = filepath.WalkDir(root.path, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			name := entry.Name()
			if snapshotDatabase(name) || skip(name) || name == "users.json" || name == "recovery.pub" || name == "admin.env" || name == "first-run-password.txt" {
				return nil
			}
			rel, err := filepath.Rel(root.path, path)
			if err != nil {
				return err
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			_, err = table.Write(t.Context(), filepath.ToSlash(rel), body, 0)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		release, err := vol.Mount(root.path, table)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	// The inner credential seal still needs its own master key in the capsule.
	imapKey, err := cryptutil.LoadOrCreateKey(filepath.Join(s.dirs.Secret, "imap-config.key"))
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := cryptutil.Seal([]byte(`{"serverUrl":"https://contacts.example.test","username":"member","password":"private"}`), imapKey)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := vol.OpenDocument(filepath.Join(s.dirs.Config, "users", u.ID, "carddav-client.json")).Replace(credential); err != nil {
		t.Fatal(err)
	}
	// Include an actual contact photo and a credential whose inner key the restore needs.
	photoPath := filepath.Join(s.dirs.State, "users", u.ID, "contact-photos", "test.jpg")
	if err := vol.OpenDocument(photoPath).Replace([]byte("private-photo")); err != nil {
		t.Fatal(err)
	}
	key := pinTestKey(t, s)
	p, err := s.Collect()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range p.Files {
		if f.Path == "config/config.yaml" || f.Path == "config/users.json" || strings.Contains(f.Path, "native-domains.json") {
			t.Fatal("sealed stale file beside table", f.Path)
		}
	}
	result, err := s.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(result.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "restore")
	if _, _, err := capsule.Open(raw, key, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config/users.json")); !os.IsNotExist(err) {
		t.Fatal("table accounts expanded to a file", err)
	}
	accountBytes, err := snapshotAccountBytes(dir)
	if err != nil {
		t.Fatal(err)
	}
	var restoredAccounts struct {
		Users []users.User `json:"users"`
	}
	if err = json.Unmarshal(accountBytes, &restoredAccounts); err != nil || len(restoredAccounts.Users) != 2 {
		t.Fatal("account rows did not restore", err)
	}
	if _, err := nativeSnapshot(dir); err != nil {
		t.Fatal("table snapshot", err)
	}
	release, err := documentSnapshot(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	data, err := vol.OpenDocument(filepath.Join(dir, "state", "users", u.ID, "contact-photos", "test.jpg")).Read()
	release()
	if err != nil || string(data) != "private-photo" {
		t.Fatal("restored photo", err)
	}
	innerPath := filepath.Join(dir, "private", "imap-config.key")
	inner, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(innerPath); err != nil {
		t.Fatal(err)
	}
	if err := validateDocumentSnapshot(dir); err == nil {
		t.Fatal("accepted credentials without their inner key")
	}
	if err := os.WriteFile(innerPath, inner, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := QuarantineNativeRestore(dir); err != nil {
		t.Fatal("table quarantine", err)
	}
	release, err = documentSnapshot(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := sso.RequireNativeRestoreReleased(filepath.Join(dir, "state")); !errors.Is(err, sso.ErrNativeRestoreHold) {
		t.Fatal("restore did not remain held", err)
	}
}
