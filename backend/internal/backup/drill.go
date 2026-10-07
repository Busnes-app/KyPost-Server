package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

func (s *Service) Drill(ctx context.Context) (*recoveryclient.DrillResult, error) {
	release, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer release()
	p, err := s.collect(ctx)
	if err != nil {
		return nil, err
	}
	return recoveryclient.Drill(ctx, filepath.Join(s.dirs.State, scratchDirName), p, func(dir string, opened capsule.Manifest) []recoveryclient.Check {
		// The bulk snapshot round-trips through the same restore an operator runs.
		bulk := []recoveryclient.Check{}
		if _, err := os.Stat(filepath.Join(dir, bulkManifestPath)); err == nil {
			err = RestoreBulk(ctx, dir, s.cfg.BulkRepository, s.cfg.ResticBinary)
			check := recoveryclient.Check{Name: "bulk:restore", Passed: err == nil, Message: "mail snapshot restored and every digest matched"}
			if err != nil {
				check.Message = recoveryclient.AuditSafe(err.Error())
			}
			bulk = append(bulk, check)
		}
		return append(bulk, drillChecks(dir, opened)...)
	})
}

// drillPaths is every member to check: the capsule's files and, when its
// sealed manifest names them, the restored bulk mail databases.
func drillPaths(dir string, opened capsule.Manifest) []string {
	paths := []string{}
	for _, f := range opened.Files {
		paths = append(paths, f.Path)
	}
	var m BulkManifest
	if raw, err := os.ReadFile(filepath.Join(dir, bulkManifestPath)); err == nil && json.Unmarshal(raw, &m) == nil {
		for _, f := range m.Files {
			paths = append(paths, f.Path)
		}
	}
	return paths
}

func drillChecks(dir string, opened capsule.Manifest) []recoveryclient.Check {
	checks := []recoveryclient.Check{}
	check := func(name string, ok bool) { checks = append(checks, recoveryclient.Check{Name: name, Passed: ok}) }
	recipe, validRecipe := opened.VerificationRecipe.(map[string]any)
	if !validRecipe {
		return []recoveryclient.Check{{Name: "recipe object", Passed: false}}
	}
	check("recipe version", recipe["version"] == float64(1))

	check("recipe:sqlite", recipe["sqlite"] == "all-state-databases")
	check("recipe:imap", recipe["imap"] == "all-stored-credentials")
	rawRequired, ok := recipe["required"].([]any)
	check("recipe:required", ok && len(rawRequired) > 0)
	requiredSet := map[string]bool{}
	for _, value := range rawRequired {
		path, ok := value.(string)
		valid := ok && fs.ValidPath(path) && path != "."
		check("recipe path", valid)
		if valid {
			requiredSet[path] = true
		}
	}
	for _, name := range required {
		check("recipe requires:"+name, requiredSet[name])
		_, err := os.Stat(filepath.Join(dir, name))
		check("present:"+name, err == nil)
	}
	for _, path := range drillPaths(dir, opened) {
		if !fs.ValidPath(path) {
			check("file path", false)
			continue
		}
		if snapshotDatabase(filepath.Base(path)) {
			check("sqlite:"+path, integrityOK(filepath.Join(dir, path)))
		}
		if filepath.Base(path) == "mailbox.db" {
			relay, _, _ := mailmsg.ReadDomainRelayAnyVersion(filepath.Join(dir, "config/native-relay.json"), filepath.Join(dir, "private/native-relay.key"))
			key, _ := cryptutil.LoadKey(filepath.Join(dir, "private/native-relay.key"))
			has, err := mailbox.ValidateOutboundSnapshot(context.Background(), filepath.Join(dir, path), key, relay)
			check("outbox:"+path, err == nil)
			if has {
				check("recipe:outbox", recipe["outbox"] == "encrypted-jobs-claims-and-sent")
			}
		}
		if path == "config/native-relay.json" {
			check("recipe:relay", recipe["relay"] == "domain-credentials-and-authority")
		}
	}
	_, nativeErr := nativeSnapshot(dir)
	check("native:historical-ownership", nativeErr == nil)
	var doc struct {
		Users []struct {
			Role   string `json:"role"`
			Active bool   `json:"active"`
		} `json:"users"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config/users.json"))
	admin := false
	if err == nil && json.Unmarshal(raw, &doc) == nil {
		for _, u := range doc.Users {
			admin = admin || (u.Role == "admin" && u.Active)
		}
	}
	check("accounts:active-admin", admin)
	_, err = cryptutil.LoadKey(filepath.Join(dir, "private/totp-secret.key"))
	check("TOTP master key", err == nil)

	for _, f := range opened.Files {
		if !fs.ValidPath(f.Path) {
			continue
		}
		if filepath.Base(f.Path) == "imap-config.json" {
			raw, err := os.ReadFile(filepath.Join(dir, f.Path))
			if err == nil {
				_, err = cryptutil.OpenBytes(raw, filepath.Join(dir, "private/imap-config.key"))
			}
			check("decrypt:"+f.Path, err == nil)
		}
	}
	return checks
}
func integrityOK(path string) bool {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=ro")
	if err != nil {
		return false
	}
	defer db.Close()
	var v string
	return db.QueryRow("PRAGMA integrity_check").Scan(&v) == nil && v == "ok"
}
