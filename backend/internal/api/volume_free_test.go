package api

import (
	"crypto/sha256"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
)

// rootSnapshot is every path under the three data roots with its mode and content hash.
func rootSnapshot(t *testing.T, roots map[string]string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil || rel == "." {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if d.IsDir() {
				// An empty directory carries no data; stores MkdirAll before a table write.
				return nil
			}
			sum := info.Mode().String()
			if info.Mode().IsRegular() {
				b, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				h := sha256.Sum256(b)
				sum += string(h[:])
			}
			out[name+"/"+filepath.ToSlash(rel)] = sum
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// The volume-free test (MH7): with table documents mounted, every org-bound route's flows
// (the cross-org matrix: contacts, groups, rules, settings, masked addresses, send-as, PGP,
// devices, agent tokens and the mailbox routes) change nothing on the host's disk but the
// per-host files tenancy.HostFiles names. Directories are not compared.
func TestTableModeWritesNothingOnTheHostsDisk(t *testing.T) {
	dbbackend.RequirePostgres(t)
	fx, logs := newOrgFixture(t)
	mountServerDocuments(t, fx.srv)
	roots := map[string]string{"config": fx.srv.configDir, "state": fx.srv.stateDir, "private": config.SecretDir()}
	before := rootSnapshot(t, roots)
	runCrossOrgMatrix(t, fx, logs)
	after := rootSnapshot(t, roots)
	for _, p := range slices.Sorted(maps.Keys(after)) {
		// Keys: this fixture provisions none, so a lazy key is created here; in table mode
		// creating one is refused (MH5, TestTableModeRequiresProvisionedKeysAndCreatesNone).
		if tenancy.Targets[path.Base(p)] == tenancy.TargetKey {
			continue
		}
		if before[p] == after[p] || hostFile(p) {
			continue
		}
		t.Errorf("table mode changed %s on the host's disk", p)
	}
	for p := range before {
		if _, ok := after[p]; !ok && !hostFile(p) {
			t.Errorf("table mode removed %s from the host's disk", p)
		}
	}
}

// hostFile reports a path ("<root>/<rel>") that tenancy.HostFiles lets a host change.
func hostFile(p string) bool {
	return slices.ContainsFunc(tenancy.HostFiles, func(pattern string) bool { ok, _ := path.Match(pattern, p); return ok })
}
