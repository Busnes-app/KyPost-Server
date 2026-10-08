package backup

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/kypost-server/backend/internal/cfreceiving"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"gopkg.in/yaml.v3"
)

const ErrMailExcluded = "IMAP mail and the rebuildable mail cache are excluded; local mailbox/receiving databases and encrypted pickup messages awaiting collection are included"
const scratchDirName = "backup-scratch"

var required = []string{"config/config.yaml", "config/users.json", "private/totp-secret.key", "state/state.db"}

func snapshotDatabase(name string) bool {
	return name == "state.db" || name == "mailbox.db" || name == "ingress.db" || name == cfreceiving.DBFile
}

// ".v1-migrated" (native storage migration copies) needs its own rule: it ends
// in "-migrated", which the ".migrated" rule does not match.
func skip(name string) bool {
	// The Cloudflare host record is this host's live marker: a restore must start fenced.
	return name == "supervisor.sock" || name == "supervisord.pid" || name == "poll-now.trigger" || name == "mailcache.json" || name == scratchDirName || name == bulkManifestName || strings.HasPrefix(name, cfreceiving.HostFile) || name == ingress.EvidenceDamagedFile || strings.HasSuffix(name, ".lock") ||
		strings.HasSuffix(name, ".migrated") || strings.HasSuffix(name, ".v1-migrated") || strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") || strings.HasSuffix(name, "-journal")
}

func (s *Service) Collect() (recoveryclient.Payload, error) { return s.collect(context.Background()) }

func (s *Service) collect(ctx context.Context) (recoveryclient.Payload, error) {
	var empty recoveryclient.Payload
	// Individual secret overrides must remain at their canonical location. A
	// capsule cannot reproduce an arbitrary host mount on a different machine.
	for env, name := range map[string]string{"IMAP_CONFIG_KEY_FILE": "imap-config.key", "TOTP_SECRET_KEY_FILE": "totp-secret.key", "PGP_PRIVATE_KEY_FILE": "pgp-private-key.key", "PICKUP_STORE_KEY_FILE": "pickup-store.key", "PAIRING_SECRET_FILE": "pairing.key", "POW_SECRET_FILE": "pow.key", "PUSH_RELAY_KEY_FILE": "push_relay_key", "APNS_RELAY_KEY_FILE": "apns_relay_key"} {
		if path := strings.TrimSpace(os.Getenv(env)); path != "" && filepath.Clean(path) != filepath.Join(s.dirs.Secret, name) {
			return empty, fmt.Errorf("backup does not support %s outside its default SECRET_DIR location; restore the standard layout before backing up", env)
		}
	}
	if _, err := cryptutil.LoadKey(filepath.Join(s.dirs.Secret, "totp-secret.key")); err != nil {
		return empty, fmt.Errorf("required TOTP master key: %w", err)
	}
	s.collectedBulk = nil
	scratchRoot := s.scratchRoot()
	if err := os.MkdirAll(scratchRoot, 0700); err != nil {
		return empty, err
	}
	if err := sweepScratch(scratchRoot); err != nil {
		return empty, fmt.Errorf("sweep stale backup scratch: %w", err)
	}
	scratch, err := os.MkdirTemp(scratchRoot, "snapshot-")
	if err != nil {
		return empty, err
	}
	defer os.RemoveAll(scratch)
	// The repository key exists before the walk so the capsule seals it.
	var bulk *bulkStage
	var bulkKey []byte
	if s.cfg.BulkRepository != "" {
		need, err := s.liveMailBytes()
		if err == nil {
			err = checkScratchSpace(scratchRoot, need)
		}
		if err != nil {
			return empty, err
		}
		if bulkKey, err = s.bulkKey(ctx); err != nil {
			return empty, fmt.Errorf("bulk backup repository: %w", err)
		}
		bulk = &bulkStage{dir: filepath.Join(scratchRoot, bulkDirName)}
		if err := os.Mkdir(bulk.dir, 0700); err != nil {
			return empty, err
		}
		defer os.RemoveAll(bulk.dir)
	}
	files := []recoveryclient.File{}
	var total int64
	have := map[string]bool{}
	imaps := []string{}
	totalHint := ""
	fileCap := func(rel string) error {
		hint := ""
		if bulkDatabase(path.Base(rel)) {
			hint = bulkHint
		}
		return fmt.Errorf("%s exceeds the 64 MiB per-file backup cap%s", rel, hint)
	}
	add := func(rel string, data []byte) error {
		if int64(len(data)) > recoveryclient.MaxCapsuleFileBytes {
			return fileCap(rel)
		}
		if bulkDatabase(path.Base(rel)) {
			totalHint = bulkHint
		}
		total += int64(len(data))
		if total > recoveryclient.MaxCapsuleTotalBytes {
			return fmt.Errorf("payload exceeds the 256 MiB backup cap%s", totalHint)
		}
		have[rel] = true
		files = append(files, recoveryclient.File{Path: rel, Data: data, Mode: 0600})
		return nil
	}
	// Snapshot receiving before any mailbox: a delivery archived after this
	// snapshot is still pending in it and re-imports idempotently, whereas a
	// tombstone captured before its mailbox commit would lose that mail.
	const ingressRel = "receiving/ingress.db"
	ingressPath := filepath.Join(s.dirs.State, filepath.FromSlash(ingressRel))
	if info, err := os.Lstat(ingressPath); err == nil {
		if !info.Mode().IsRegular() {
			return empty, fmt.Errorf("cannot back up non-regular file state/%s", ingressRel)
		}
		if bulk != nil {
			err = bulk.snapshot(ctx, ingressPath, "state/"+ingressRel)
		} else {
			var raw []byte
			if raw, err = state.SnapshotDB(ctx, ingressPath, scratch); err == nil {
				err = add("state/"+ingressRel, raw)
			}
		}
		if err != nil {
			return empty, fmt.Errorf("collect state/%s: %w", ingressRel, err)
		}
	} else if !os.IsNotExist(err) {
		return empty, err
	}
	for _, r := range []struct{ path, prefix string }{{s.dirs.Config, "config"}, {s.dirs.Secret, "private"}, {s.dirs.State, "state"}} {
		root, err := os.OpenRoot(r.path)
		if err != nil {
			return empty, err
		}
		err = func() error {
			defer root.Close()
			return fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if rel != "." && skip(d.Name()) {
					if d.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
				// Exclude the configured capsule destination, even when nested in a data root.
				full := filepath.Join(r.path, filepath.FromSlash(rel))
				if s.cfg.Dir != "" && full == s.cfg.Dir {
					if d.IsDir() {
						return fs.SkipDir
					}
					return fmt.Errorf("backup directory is not a directory")
				}
				if d.IsDir() || r.prefix == "state" && rel == ingressRel {
					return nil
				}
				if !d.Type().IsRegular() {
					return fmt.Errorf("cannot back up non-regular file %s/%s", r.prefix, rel)
				}
				name := r.prefix + "/" + rel
				info, err := d.Info()
				if err != nil {
					return err
				}
				// Automatic-block evidence is heuristic: a bad copy is left out,
				// never a reason to refuse the backup.
				if d.Name() == ingress.EvidenceFile && info.Size() > ingress.MaxEvidenceBytes {
					slog.Warn("backup skipped sender evidence", "actor", "backup", "task_id", "backup", "action", "collect", "target", name, "result", "skipped-oversized")
					return nil
				}
				if bulk != nil && r.prefix == "state" && bulkDatabase(d.Name()) {
					if err := bulk.snapshot(ctx, full, name); err != nil {
						return fmt.Errorf("collect %s: %w", name, err)
					}
					return nil
				}
				if info.Size() > recoveryclient.MaxCapsuleFileBytes {
					return fileCap(name)
				}
				var raw []byte
				if snapshotDatabase(d.Name()) {
					raw, err = state.SnapshotDB(ctx, full, scratch)
				} else {
					f, e := root.Open(rel)
					if e != nil {
						return e
					}
					remaining := min(recoveryclient.MaxCapsuleFileBytes, recoveryclient.MaxCapsuleTotalBytes-total)
					raw, err = io.ReadAll(io.LimitReader(f, remaining+1))
					f.Close()
				}
				if err != nil {
					return fmt.Errorf("collect %s: %w", name, err)
				}
				if d.Name() == ingress.EvidenceFile && ingress.ParseEvidence(raw) != nil {
					slog.Warn("backup skipped sender evidence", "actor", "backup", "task_id", "backup", "action", "collect", "target", name, "result", "skipped-malformed")
					return nil
				}
				if d.Name() == "imap-config.json" {
					imaps = append(imaps, name)
				}
				return add(name, raw)
			})
		}()
		if err != nil {
			return empty, err
		}
	}
	for _, name := range required {
		if !have[name] {
			return empty, fmt.Errorf("refusing to seal without %s", name)
		}
	}
	if len(imaps) > 0 {
		if _, err := cryptutil.LoadKey(filepath.Join(s.dirs.Secret, "imap-config.key")); err != nil {
			return empty, fmt.Errorf("IMAP master key required by stored credentials: %w", err)
		}
	}
	// Validate configured file dependencies against the collected roots. Environment
	// credentials and TLS mounts are restored separately from the operator's .env.
	var cfg config.Config
	for _, f := range files {
		if f.Path == "config/config.yaml" {
			if err := yaml.Unmarshal(f.Data, &cfg); err != nil {
				return empty, err
			}
		}
	}
	tuningPath := strings.TrimSpace(os.Getenv("TUNING_FILE"))
	_, tuningErr := os.Stat(tuningPath)
	for label, path := range map[string]string{"VAPID private key": cfg.Notifications.PrivateKeyPath, "TUNING_FILE": tuningPath} {
		if path == "" {
			continue
		}
		included := false
		for _, root := range []struct{ path, prefix string }{{s.dirs.Config, "config"}, {s.dirs.Secret, "private"}, {s.dirs.State, "state"}} {
			rel, err := filepath.Rel(root.path, path)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				included = included || have[root.prefix+"/"+filepath.ToSlash(rel)]
				// Compose's optional prompt may be absent, but only a path
				// inside collected roots qualifies for that exemption.
				included = included || (label == "TUNING_FILE" && os.IsNotExist(tuningErr))
			}
		}
		if !included {
			return empty, fmt.Errorf("%s is outside the supported backup layout or missing; place it inside CONFIG_DIR or SECRET_DIR", label)
		}
	}
	if err := validateDependencies(files); err != nil {
		return empty, err
	}
	recipe := map[string]any{"version": 1, "mail": ErrMailExcluded, "required": required, "sqlite": "all-state-databases", "imap": "all-stored-credentials", "relay": "domain-credentials-and-authority", "outbox": "encrypted-jobs-claims-and-sent"}
	staged := map[string]string{}
	if bulk != nil {
		for _, rel := range bulk.files {
			staged[rel] = bulk.path(rel)
		}
	}
	if err := validateNativePayload(ctx, files, staged, scratch); err != nil {
		return empty, err
	}
	// Restic runs last: a payload refused above leaves no snapshot behind. The
	// snapshot points are the VACUUM INTO copies, so this order changes nothing.
	if len(staged) > 0 {
		if !have["private/"+bulkKeyName] {
			return empty, fmt.Errorf("refusing to seal a bulk snapshot without private/%s", bulkKeyName)
		}
		manifest, err := s.backupBulk(ctx, bulk, bulkKey)
		if err != nil {
			return empty, fmt.Errorf("mail bulk backup: %w", err)
		}
		if err := add(bulkManifestPath, manifest); err != nil {
			return empty, err
		}
		recipe["bulk"] = bulkRecipe
	}
	return recoveryclient.Payload{ServiceName: AppName, AppVersion: s.version, Files: files,
		Dependencies:       map[string]any{"ollama": "model cache downloads again", "layout": "restore config, private and state to CONFIG_DIR, SECRET_DIR and STATE_DIR"},
		VerificationRecipe: recipe}, nil
}

// validateDependencies refuses a capsule whose stored identities cannot be
// opened after restore. Client-wrapped PGP keys stay opaque throughout.
func validateDependencies(files []recoveryclient.File) error {
	byPath := map[string][]byte{}
	for _, f := range files {
		byPath[f.Path] = f.Data
	}
	var accounts struct {
		Users []struct {
			TOTP string `json:"totpSecretEnc"`
			PGP  string `json:"pgpPrivateKeyEnc"`
		} `json:"users"`
	}
	if err := json.Unmarshal(byPath["config/users.json"], &accounts); err != nil {
		return fmt.Errorf("invalid users.json: %w", err)
	}
	requireKey := func(name string) error {
		raw, ok := byPath["private/"+name]
		if !ok {
			return fmt.Errorf("stored encrypted data requires private/%s", name)
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(key) != 32 {
			return fmt.Errorf("invalid private/%s", name)
		}
		return nil
	}
	if raw, ok := byPath["config/native-relay.json"]; ok {
		if err := requireKey("native-relay.key"); err != nil {
			return err
		}
		key, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(string(byPath["private/native-relay.key"])))
		if _, _, err := mailmsg.DecodeDomainRelay(raw, key); err != nil {
			return err
		}
	}
	for _, u := range accounts.Users {
		if u.TOTP != "" {
			if err := requireKey("totp-secret.key"); err != nil {
				return err
			}
		}
		if u.PGP != "" {
			if err := requireKey("pgp-private-key.key"); err != nil {
				return err
			}
		}
	}
	for name, raw := range byPath {
		if strings.HasPrefix(name, "state/pickup/") && strings.HasSuffix(name, ".json") {
			var record pgpmail.PickupRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return fmt.Errorf("invalid pickup record %s: %w", name, err)
			}
			if record.SubjectEnc != nil || record.BodyEnc != (cryptutil.EncryptedPayload{}) {
				if err := requireKey("pickup-store.key"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
