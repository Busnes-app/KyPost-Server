package tenancy

import (
	"database/sql"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	_ "github.com/Busnes-app/kypost-server/backend/internal/cfreceiving"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	_ "github.com/Busnes-app/kypost-server/backend/internal/ingress"
	_ "github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	_ "github.com/Busnes-app/kypost-server/backend/internal/sso"
	_ "github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

// Store classes of the tenancy design's Store inventory.
const (
	org      = "O" // carries org_id: the two org roots
	derived  = "D" // inherits the org of the user, mailbox or subject that owns it
	platform = "P" // install-wide, staff only
	absent   = "-" // not present in multi mode (K6)
)

// tableClass is every table of every store kind, and of the PostgreSQL catalog, with its
// class. Per-user and root state.db share one schema, so a table has one class for both.
var tableClass = map[string]map[string]string{
	"state": {
		"meta": platform, "backup_audit": platform,
		"agent_tokens": derived, "scheduled_sends": derived, "send_ledger": derived,
		"processed": derived, "decisions": derived, "notifications": derived, "native_devices": derived,
		"pull_notifications": derived, "deferrals": derived, "sorter_predictions": derived,
		"sorter_corrections": derived, "snoozes": derived, "ooo_messages": derived, "ooo_senders": derived,
		// The replica tables (#677).
		"sessions": derived, "session_revocations": derived, "auth_challenges": derived, "import_jobs": derived,
		"security_windows": platform, "spent_challenges": platform, "replay_ids": platform, "replica_instances": platform,
	},
	"mailbox": {
		"identity": derived, "namespace": derived, "reference_generation": derived, "folders": derived,
		"labels": derived, "messages": derived, "incoming_replacements": derived, "imported": derived,
		"receipts": derived, "usage": derived, "changes": derived,
		"outbox": derived, "outbox_deliveries": derived, "outbox_independent": derived,
	},
	"ingress": {
		"routes": derived, "bindings": derived,
		"deliveries": platform, "archived": platform, "limits": platform,
	},
	"cloudflare": {
		"tables": platform, "revisions": platform, "ledger": platform, "blocks": platform, "status": platform,
	},
	"native-authority": {
		"meta":      platform,
		"directory": derived, "directory_primary_emails": derived, "accounts": derived, "mailboxes": derived,
		"addresses": derived, "address_history": derived, "routes_pending": derived,
		"directory_events": platform, "logouts": platform, "sign_ons": platform, "release_floors": platform,
		"recovery_floors": platform, "recovery_barriers": platform, "lifecycle_records": platform,
	},
	"tenancy": {
		"orgs": platform, "user_orgs": org, "subject_orgs": org, "receiving_gateways": platform,
		"sender_blocks": platform, "sender_evidence": platform,
		"org_purges": platform, "org_purge_members": platform, "address_tombstones": platform, "restore_reads": platform, "purged_mailboxes": platform,
		"import_leases": derived, "retired_bindings": derived,
		// Byte counts per org, like the orgs cache.
		"org_storage": platform,
	},
	"volumes": {
		"documents": derived, "doc_keys": org, "accounts": org,
	},
	"catalog": {"stores": platform},
}

// fileClass is every file store, by the name the code spells it with.
var fileClass = map[string]string{
	// Accounts and the first-run administrator.
	"users.json": org, "admin.env": absent, "first-run-password.txt": absent,
	// Install settings; config.yaml is also the per-user settings file (D), see perUser.
	"config.yaml": platform, "mail-defaults.json": platform, "sso.json": platform,
	"sso-lifecycle.json": platform, "native-provisioning.json": platform,
	"native-domains.json": platform, "native-relay.json": platform, "wkd-domains.json": platform,
	"receiving.conf": platform, "quota-": derived,
	// Single mode only: multi mode keeps them in tenancy.db (receiving tier R7).
	"sender-blocks.json": absent, "sender-evidence.json": absent, "sender-evidence.damaged.json": absent,
	// Per user.
	"imap-config.json": absent, "tuning.md": derived, "carddav-auth.json": derived, "carddav-client.json": derived,
	"contacts.json": derived, "groups.json": derived, "rules.json": derived, "send_as_aliases.json": derived,
	"pgp-discovery.json": derived, "pgp-discovery-suppressions.json": derived,
	"kycalendar-invitations.json": derived, "kycalendar-flags.json": derived, "incoming-encryption.json": derived,
	// Per mailbox.
	"mailcache.json": derived, "native-mailbox.json": derived, "outbox-idle": derived,
	// Owner-keyed records in install directories.
	"pickup": derived, "imports": derived, "contact-photos": derived,
	// Whole-install recovery, backups and process coordination.
	"native-restore-hold.json": platform, "native-restore-qualification.json": platform,
	"native-restore-released.json": platform, "mfa-clear-all.done": platform, "mfa-clear-all.progress": platform,
	"backups": platform, "mail-bulk.json": platform, "backup-operation.lock": platform,
	"replica-volume.id": platform, "server-instance.lock": platform,
	"recovery.pub": platform, "recovery-pin.generation": platform,
	// Keys (SECRET_DIR): one per install.
	"documents.key": platform, "native-relay.key": platform, "imap-config.key": platform, "totp-secret.key": platform,
	"pgp-private-key.key": platform, "pickup-store.key": platform, "pairing.key": platform, "pow.key": platform,
	"relay-identity.key": platform, "scheduled-send.key": platform, "push_relay_key": platform,
	"apns_relay_key": platform, "notifications-vapid-private.pem": platform, "cloudflare-receiving.json": platform,
	"cloudflare-receiving.host.json": platform, "bulk-backup.key": platform, "bulk-backup.repo": platform,
	"imap-import.key": platform, "receiving-tls.key": platform, TombstoneKeyFile: platform,
	// The databases, classified table by table above.
	"state.db": platform, "mailbox.db": derived, "ingress.db": platform, "cloudflare.db": platform,
	"native-authority.db": platform, File: platform, volumes.File: platform,
}

// notStores are file names in the code that name no store: legacy inputs migrated away,
// read-only assets, scratch names and suffixes.
var notStores = map[string]string{
	"state.json": "legacy, migrated into state.db", "decisions.json": "legacy, migrated into state.db",
	"native-domain.json": "legacy single-domain file, migrated into native-domains.json",
	"tokenizer.json":     "the embedding model, read-only", "TUNING.md": "the bundled prompt, read-only",
	".json": "a suffix", ".lock": "a suffix", "x.db": "a probe name", "<path>.lock": "a message",
	"%s.%x.lock": "a lock name pattern", "snap-%d.db": "a snapshot scratch name",
}

// fileName is a quoted file-like literal; extras are store names without an extension.
var (
	fileName = regexp.MustCompile(`"([A-Za-z0-9_.<>*%-]*\.(?:json|yaml|yml|key|env|txt|pem|conf|repo|db|md|lock|id))"`)
	extras   = []string{"contact-photos", "quota-", "outbox-idle", "pickup", "imports", "mfa-clear-all.done", "mfa-clear-all.progress", "backups", "push_relay_key", "apns_relay_key", "recovery.pub", "recovery-pin.generation"}
	ddlTable = regexp.MustCompile(`(?i)CREATE TABLE (?:IF NOT EXISTS )?(?:kypost_catalog\.)?([a-z_]+)`)
)

// TestEveryStoreIsClassified fails on a table of any store kind, or a file name the code
// spells, that has no class, and on a class nothing names any more. A new store kind must
// be registered here and classified.
func TestEveryStoreIsClassified(t *testing.T) {
	sources := map[string]string{}
	_ = filepath.WalkDir("..", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		b, err := os.ReadFile(p)
		sources[p] = string(b)
		return err
	})

	registrations := 0
	for _, src := range sources {
		registrations += strings.Count(src, "dbbackend.Register(")
	}
	kinds := dbbackend.KindsForTest()
	if registrations != len(kinds) {
		t.Fatalf("%d dbbackend.Register calls but %d kinds registered: import the new store's package here", registrations, len(kinds))
	}
	for _, k := range kinds {
		classes, ok := tableClass[k.Name]
		if !ok {
			t.Errorf("store kind %s is not classified", k.Name)
			continue
		}
		dir := filepath.Join(t.TempDir(), k.Name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, k.File)
		if err := k.CreateSQLite(path); err != nil {
			t.Fatalf("%s: %v", k.Name, err)
		}
		tables := sqliteTables(t, path)
		for _, m := range ddlTable.FindAllStringSubmatch(k.DDL, -1) {
			tables[m[1]] = true
		}
		checkTables(t, k.Name, tables, classes)
	}
	catalog := map[string]bool{}
	for _, m := range ddlTable.FindAllStringSubmatch(sources[filepath.Join("..", "dbbackend", "dbbackend.go")], -1) {
		catalog[m[1]] = true
	}
	checkTables(t, "catalog", catalog, tableClass["catalog"])
	for name := range tableClass {
		if name != "catalog" && !slices.ContainsFunc(kinds, func(k dbbackend.Kind) bool { return k.Name == name }) {
			t.Errorf("classified store kind %s is not registered", name)
		}
	}

	named := map[string]bool{}
	for _, src := range sources {
		for _, m := range fileName.FindAllStringSubmatch(src, -1) {
			named[m[1]] = true
		}
		for _, e := range extras {
			if strings.Contains(src, `"`+e+`"`) {
				named[e] = true
			}
		}
	}
	for _, n := range slices.Sorted(maps.Keys(named)) {
		_, store := fileClass[n]
		_, skip := notStores[n]
		switch {
		case store && skip:
			t.Errorf("%s is both a store and not one", n)
		case !store && !skip:
			t.Errorf("file %s has no class: add it to fileClass (and the design's Store inventory) or notStores", n)
		}
	}
	for n := range fileClass {
		if !named[n] {
			t.Errorf("classified file %s is named nowhere", n)
		}
	}
	for n := range notStores {
		if !named[n] {
			t.Errorf("unclassified name %s is named nowhere", n)
		}
	}
	checkTargets(t)
}

// checkTargets keeps the volume-free assertion's list (Targets) in step with fileClass, and
// every per-host pattern tied to a per-host store.
func checkTargets(t *testing.T) {
	t.Helper()
	for n := range fileClass {
		if _, ok := Targets[n]; !ok {
			t.Errorf("file %s has no table-mode target in Targets (hosts.go)", n)
		}
	}
	for n, target := range Targets {
		if _, ok := fileClass[n]; !ok {
			t.Errorf("Targets names %s, which fileClass does not", n)
		}
		if target == TargetHost && !NamesHostFile(n) {
			t.Errorf("per-host store %s has no HostFiles pattern", n)
		}
	}
	for _, p := range HostFiles {
		if !slices.ContainsFunc(slices.Collect(maps.Keys(Targets)), func(n string) bool { return Targets[n] == TargetHost && slices.Contains(strings.Split(p, "/"), n) }) {
			t.Errorf("HostFiles pattern %s names no per-host store", p)
		}
	}
}

func checkTables(t *testing.T, kind string, tables map[string]bool, classes map[string]string) {
	t.Helper()
	for name := range tables {
		if _, ok := classes[name]; !ok {
			t.Errorf("table %s.%s has no class", kind, name)
		}
	}
	for name, class := range classes {
		if !tables[name] {
			t.Errorf("classified table %s.%s does not exist", kind, name)
		}
		if !slices.Contains([]string{org, derived, platform, absent}, class) {
			t.Errorf("%s.%s: unknown class %q", kind, name, class)
		}
	}
}

func sqliteTables(t *testing.T, path string) map[string]bool {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[n] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestPurgeCoversEveryOrgStore fails when an org-owned or derived store has no part of the
// purge's final check (PurgeCoverage), or the coverage names a store that is not one.
func TestPurgeCoversEveryOrgStore(t *testing.T) {
	want := map[string]bool{}
	for kind, tables := range tableClass {
		for table, class := range tables {
			if class == org || class == derived {
				want[kind+"."+table] = true
			}
		}
	}
	for name, class := range fileClass {
		if class == org || class == derived {
			want[name] = true
		}
	}
	for name := range want {
		if PurgeCoverage[name] == "" {
			t.Errorf("%s is org-owned or derived but the purge's final check does not cover it (PurgeCoverage)", name)
		}
	}
	for name := range PurgeCoverage {
		if !want[name] {
			t.Errorf("PurgeCoverage names %s, which is not an org-owned or derived store", name)
		}
	}
}
