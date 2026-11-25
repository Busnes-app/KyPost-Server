package tenancy

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

// Target is where a volume file's store lives in table mode (MH7, "The volume-free
// assertion"). TestEveryStoreIsClassified keeps Targets and its file classes in step.
type Target string

const (
	TargetDatabase  Target = "database"  // a dbbackend store: PostgreSQL in table mode
	TargetDocuments Target = "documents" // read and written through the config or state mount
	TargetKey       Target = "key"       // a provisioned, read-only key in SECRET_DIR
	TargetFence     Target = "fence"     // a lock replaced by a database fence
	TargetAbsent    Target = "absent"    // never written in table mode
	TargetHost      Target = "host"      // per-host scratch, matched by HostFiles
	TargetFile      Target = "file"      // still written to this host's disk: refuses the start
)

// HostFiles are the only paths, relative to a data root, a table-mode host may change on its
// own disk. The volume-free test allows these and nothing else.
var HostFiles = []string{"state/imports/*/upload-*", "state/mail-bulk.json", "state/recovery.pub", "state/recovery-pin.generation"}

// Targets is every file store by the name the code spells it with. A TargetFile entry says
// why it still lives on the host's disk; each one keeps table mode from starting.
var Targets = map[string]Target{
	"users.json": TargetDatabase, "admin.env": TargetFile, "first-run-password.txt": TargetFile,
	"config.yaml": TargetDocuments, "mail-defaults.json": TargetDocuments, "sso.json": TargetDocuments,
	"sso-lifecycle.json": TargetDocuments, "native-provisioning.json": TargetDocuments,
	"native-domains.json": TargetDocuments, "native-relay.json": TargetDocuments, "wkd-domains.json": TargetDocuments,
	"receiving.conf": TargetAbsent, "quota-": TargetFence,
	"sender-blocks.json": TargetAbsent, "sender-evidence.json": TargetAbsent, "sender-evidence.damaged.json": TargetAbsent,
	"imap-config.json": TargetDocuments, "tuning.md": TargetFile, "carddav-auth.json": TargetDocuments, "carddav-client.json": TargetDocuments,
	"contacts.json": TargetDocuments, "groups.json": TargetDocuments, "rules.json": TargetDocuments, "send_as_aliases.json": TargetDocuments,
	"pgp-discovery.json": TargetDocuments, "pgp-discovery-suppressions.json": TargetDocuments,
	"kycalendar-invitations.json": TargetDocuments, "kycalendar-flags.json": TargetDocuments, "incoming-encryption.json": TargetDocuments,
	"mailcache.json": TargetDocuments, "native-mailbox.json": TargetFile, "outbox-idle": TargetAbsent,
	"pickup": TargetDocuments, "imports": TargetHost, "contact-photos": TargetDocuments,
	"native-restore-hold.json": TargetDocuments, "native-restore-qualification.json": TargetDocuments,
	"native-restore-released.json": TargetDocuments, "mfa-clear-all.done": TargetDocuments, "mfa-clear-all.progress": TargetDocuments,
	"backups": TargetAbsent, "mail-bulk.json": TargetHost, "backup-operation.lock": TargetFence,
	"replica-volume.id": TargetAbsent, "server-instance.lock": TargetAbsent,
	"recovery.pub": TargetHost, "recovery-pin.generation": TargetHost,
	"documents.key": TargetKey, "native-relay.key": TargetKey, "imap-config.key": TargetKey, "totp-secret.key": TargetKey,
	"pgp-private-key.key": TargetKey, "pickup-store.key": TargetKey, "pairing.key": TargetKey, "pow.key": TargetKey,
	"relay-identity.key": TargetKey, "scheduled-send.key": TargetKey, "push_relay_key": TargetFile,
	"apns_relay_key": TargetFile, "notifications-vapid-private.pem": TargetKey, "cloudflare-receiving.json": TargetFile,
	"cloudflare-receiving.host.json": TargetFile, "bulk-backup.key": TargetKey, "bulk-backup.repo": TargetFile,
	"imap-import.key": TargetKey, "receiving-tls.key": TargetKey, TombstoneKeyFile: TargetKey,
	"state.db": TargetDatabase, "mailbox.db": TargetDatabase, "ingress.db": TargetDatabase, "cloudflare.db": TargetDatabase,
	"native-authority.db": TargetDatabase, File: TargetDatabase, volumes.File: TargetDatabase,
}

// ErrHostFiles refuses a table-mode start while a store still lives on the host's disk.
var ErrHostFiles = errors.New("table mode cannot start: these stores are still written to this host's disk")

// AssertVolumeFree refuses unless every store in targets resolves off this host's disk, or is
// per-host scratch that a HostFiles pattern names. It runs before any listener or worker.
func AssertVolumeFree(targets map[string]Target) error {
	var onDisk []string
	for _, name := range slices.Sorted(maps.Keys(targets)) {
		switch targets[name] {
		case TargetDatabase, TargetDocuments, TargetKey, TargetFence, TargetAbsent:
		case TargetHost:
			if !NamesHostFile(name) {
				onDisk = append(onDisk, name+" (no per-host pattern)")
			}
		default:
			onDisk = append(onDisk, name)
		}
	}
	if len(onDisk) > 0 {
		return fmt.Errorf("%w: %s", ErrHostFiles, strings.Join(onDisk, ", "))
	}
	return nil
}

// NamesHostFile reports whether a HostFiles pattern covers the store name.
func NamesHostFile(name string) bool {
	return slices.ContainsFunc(HostFiles, func(p string) bool { return slices.Contains(strings.Split(p, "/"), name) })
}
