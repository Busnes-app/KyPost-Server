package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// Mail databases outgrow a capsule. With KYPOST_BULK_BACKUP_REPOSITORY set they
// go to restic and the capsule seals this manifest, which binds the snapshot ID
// and every file's digest. The restic password is derived from the dedicated
// private/bulk-backup.key, collected into the capsule like every other secret.
const (
	bulkManifestName = "mail-bulk.json"
	bulkManifestPath = "state/" + bulkManifestName
	bulkKeyName      = "bulk-backup.key"
	bulkRoot         = "/mail"
	bulkSetting      = "backup_bulk_last"
	bulkHint         = "; set KYPOST_BULK_BACKUP_REPOSITORY to back up mail databases through restic"
)

type BulkFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type BulkManifest struct {
	Version  int        `json:"version"`
	Snapshot string     `json:"snapshot"`
	Root     string     `json:"root"`
	Files    []BulkFile `json:"files"`
}

// BulkSnapshot is the last bulk snapshot, for the screen.
type BulkSnapshot struct {
	Snapshot  string `json:"snapshot"`
	At        string `json:"at"`
	SizeBytes int64  `json:"sizeBytes"`
}

var snapshotID = regexp.MustCompile(`^[a-f0-9]{64}$`)

func bulkDatabase(name string) bool { return name == "mailbox.db" || name == "ingress.db" }

// bulkStage holds one collection's mail snapshots under dir/mail, laid out by
// capsule path, in the order they were taken.
type bulkStage struct {
	dir   string
	files []string
}

func (b *bulkStage) path(rel string) string {
	return filepath.Join(b.dir, "mail", filepath.FromSlash(rel))
}

func (b *bulkStage) snapshot(ctx context.Context, live, rel string) error {
	dest := b.path(rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	if err := state.SnapshotDBToFile(ctx, live, dest); err != nil {
		return err
	}
	b.files = append(b.files, rel)
	return nil
}

// bulkKey initializes an empty repository with a new key, or loads the key of
// an existing one. A missing key beside an existing repository is never
// replaced: that would strand every snapshot already in it.
func (s *Service) bulkKey(ctx context.Context) ([]byte, error) {
	repo, keyPath := s.cfg.BulkRepository, filepath.Join(s.dirs.Secret, bulkKeyName)
	entries, err := os.ReadDir(repo)
	if errors.Is(err, os.ErrNotExist) || err == nil && len(entries) == 0 {
		key, err := cryptutil.LoadOrCreateKey(keyPath)
		if err != nil {
			return nil, err
		}
		return key, runRestic(ctx, s.cfg.ResticBinary, repo, key, "", nil, "init")
	}
	if err != nil {
		return nil, err
	}
	config, configErr := os.Lstat(filepath.Join(repo, "config"))
	keys, keysErr := os.Lstat(filepath.Join(repo, "keys"))
	if configErr != nil || keysErr != nil || !config.Mode().IsRegular() || !keys.IsDir() {
		return nil, fmt.Errorf("KYPOST_BULK_BACKUP_REPOSITORY %s is neither empty nor a restic repository", repo)
	}
	key, err := cryptutil.LoadKey(keyPath)
	if err != nil {
		return nil, fmt.Errorf("restic repository exists but private/%s is unusable; restore it from a capsule rather than generating a new one: %w", bulkKeyName, err)
	}
	return key, nil
}

// backupBulk hashes the staged snapshots, runs one restic backup over them and
// returns the manifest to seal. Any failure fails the whole backup.
func (s *Service) backupBulk(ctx context.Context, stage *bulkStage, key []byte) ([]byte, error) {
	m := BulkManifest{Version: 1, Root: bulkRoot}
	var total int64
	for _, rel := range stage.files {
		sum, size, err := hashFile(stage.path(rel))
		if err != nil {
			return nil, err
		}
		m.Files = append(m.Files, BulkFile{Path: rel, SHA256: sum, Size: size})
		total += size
	}
	var out limitedBuffer
	if err := runRestic(ctx, s.cfg.ResticBinary, s.cfg.BulkRepository, key, stage.dir, &out, "backup", "--json", "--quiet", "--tag", "kypost-mail", "mail"); err != nil {
		return nil, err
	}
	for _, line := range bytes.Split(out.Bytes(), []byte("\n")) {
		var msg struct {
			Type     string `json:"message_type"`
			Snapshot string `json:"snapshot_id"`
		}
		if json.Unmarshal(line, &msg) == nil && msg.Type == "summary" {
			m.Snapshot = msg.Snapshot
		}
	}
	if !snapshotID.MatchString(m.Snapshot) {
		return nil, errors.New("restic did not report a full snapshot ID")
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	last, err := json.Marshal(BulkSnapshot{Snapshot: m.Snapshot, At: time.Now().UTC().Format(time.RFC3339), SizeBytes: total})
	if err != nil {
		return nil, err
	}
	return raw, s.settings.Set(bulkSetting, string(last))
}

func (s *Service) lastBulk() (*BulkSnapshot, error) {
	raw, err := s.store.Setting(bulkSetting)
	if errors.Is(err, state.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var last BulkSnapshot
	return &last, json.Unmarshal([]byte(raw), &last)
}

// RestoreBulk runs after a capsule opened into staging and before any product
// validation. It restores exactly the sealed snapshot with --verify into
// private scratch, re-hashes every file, refuses missing or extra files and
// only then moves them into staging, where none may exist yet. Absent manifest:
// nothing to do; validation still refuses a native account without its mail.
func RestoreBulk(ctx context.Context, staging, repository, resticBinary string) error {
	raw, err := os.ReadFile(filepath.Join(staging, filepath.FromSlash(bulkManifestPath)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var m BulkManifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil || m.Version != 1 || !snapshotID.MatchString(m.Snapshot) || m.Root != bulkRoot || len(m.Files) == 0 {
		return errors.New("invalid mail bulk manifest")
	}
	want := map[string]BulkFile{}
	for _, f := range m.Files {
		if !fs.ValidPath(f.Path) || !strings.HasPrefix(f.Path, "state/") || !bulkDatabase(path.Base(f.Path)) || !snapshotID.MatchString(f.SHA256) || f.Size <= 0 || want[f.Path] != (BulkFile{}) {
			return errors.New("invalid mail bulk manifest entry")
		}
		want[f.Path] = f
		if _, err := os.Lstat(filepath.Join(staging, filepath.FromSlash(f.Path))); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("capsule already holds %s; refusing to combine it with a bulk snapshot", f.Path)
		}
	}
	if repository == "" {
		return fmt.Errorf("capsule names mail bulk snapshot %s; set KYPOST_BULK_BACKUP_REPOSITORY to its restic repository", m.Snapshot)
	}
	key, err := cryptutil.LoadKey(filepath.Join(staging, "private", bulkKeyName))
	if err != nil {
		return fmt.Errorf("capsule names a bulk snapshot but private/%s is unusable: %w", bulkKeyName, err)
	}
	scratch, err := os.MkdirTemp(filepath.Dir(staging), "bulk-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	if err := runRestic(ctx, resticBinary, repository, key, "", nil, "restore", m.Snapshot+":"+bulkRoot, "--target", scratch, "--verify"); err != nil {
		return err
	}
	found := 0
	err = filepath.WalkDir(scratch, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(scratch, p)
		if err != nil {
			return err
		}
		f, ok := want[filepath.ToSlash(rel)]
		if !ok || !d.Type().IsRegular() {
			return fmt.Errorf("bulk snapshot holds unexpected %s", filepath.ToSlash(rel))
		}
		sum, size, err := hashFile(p)
		if err != nil {
			return err
		}
		if sum != f.SHA256 || size != f.Size {
			return fmt.Errorf("bulk snapshot %s does not match the sealed digest", f.Path)
		}
		found++
		return nil
	})
	if err != nil {
		return err
	}
	if found != len(want) {
		return errors.New("bulk snapshot is missing files the capsule names")
	}
	for _, f := range m.Files {
		dest := filepath.Join(staging, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(scratch, filepath.FromSlash(f.Path)), dest); err != nil {
			return err
		}
	}
	return nil
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// runRestic never uses a shell. The password reaches only the child's
// environment: never argv, a file or a log. Inherited RESTIC_* settings are
// dropped so the operator's shell cannot redirect the repository or password.
func runRestic(ctx context.Context, bin, repo string, key []byte, dir string, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, append([]string{"--repo", repo, "--no-cache"}, args...)...)
	cmd.Dir = dir
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "RESTIC_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "RESTIC_PASSWORD="+hex.EncodeToString(key))
	cmd.Stdout = stdout
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		return fmt.Errorf("restic %s failed: %w: %s", args[0], err, msg[max(0, len(msg)-512):])
	}
	return nil
}

// limitedBuffer keeps the first 1 MiB; --quiet leaves restic's JSON one summary line.
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	_, _ = b.Buffer.Write(p[:min(len(p), max(0, 1<<20-b.Len()))])
	return len(p), nil
}
