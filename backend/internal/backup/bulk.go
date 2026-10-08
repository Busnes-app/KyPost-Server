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
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// Mail databases outgrow a capsule. With KYPOST_BULK_BACKUP_REPOSITORY set they
// go to restic (0.16.0 or newer, for <snapshot>:<subfolder> restores) and the
// capsule seals this manifest, which binds the snapshot ID and every file's
// digest. The restic password is derived from the dedicated
// private/bulk-backup.key, collected into the capsule like every other secret;
// private/bulk-backup.repo records the repository ID it belongs to.
const (
	bulkManifestName = "mail-bulk.json"
	bulkManifestPath = "state/" + bulkManifestName
	bulkKeyName      = "bulk-backup.key"
	bulkRepoIDName   = "bulk-backup.repo"
	bulkRoot         = "/mail"
	bulkDirName      = "bulk"
	bulkSetting      = "backup_bulk_last"
	bulkHint         = "; set KYPOST_BULK_BACKUP_REPOSITORY to back up mail databases through restic"
	depositBudget    = 16 * time.Minute
	// bulkRecipe is readable before shares: restore refuses early without a repository.
	bulkRecipe = "restic-mail-snapshot"
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

// BulkSnapshot is the last delivered bulk snapshot, for the screen.
type BulkSnapshot struct {
	Snapshot  string `json:"snapshot"`
	At        string `json:"at"`
	SizeBytes int64  `json:"sizeBytes"`
}

var snapshotID = regexp.MustCompile(`^[a-f0-9]{64}$`)

func bulkDatabase(name string) bool { return name == "mailbox.db" || name == "ingress.db" }

// bulkStage holds one collection's mail snapshots under dir/mail, laid out by
// capsule path, in the order they were taken. dir is fixed so restic groups
// every run as one series (retention works) and finds its parent snapshot.
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

// scratchRoot holds every backup scratch directory. A configured directory gets
// a dedicated child, so sweeping never touches the operator's other files.
func (s *Service) scratchRoot() string {
	if s.cfg.ScratchDir != "" {
		return filepath.Join(s.cfg.ScratchDir, "kypost-backup-scratch")
	}
	return filepath.Join(s.dirs.State, scratchDirName)
}

// sweepScratch removes what a killed backup, drill or restore left behind:
// plaintext copies of every mailbox. Callers hold the operation lock, so no
// other backup is using them.
func sweepScratch(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		n := e.Name()
		if n == bulkDirName || strings.HasPrefix(n, "snapshot-") || strings.HasPrefix(n, "bulk-restore-") || strings.HasPrefix(n, "recoveryclient-drill-") {
			if err := os.RemoveAll(filepath.Join(root, n)); err != nil {
				return err
			}
		}
	}
	return nil
}

// liveMailBytes is what staging copies: every state mailbox/ingress database
// and its WAL.
func (s *Service) liveMailBytes() (int64, error) {
	var n int64
	err := filepath.WalkDir(s.dirs.State, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != s.dirs.State && skip(d.Name()) {
			return fs.SkipDir
		}
		if !d.Type().IsRegular() || !bulkDatabase(strings.TrimSuffix(d.Name(), "-wal")) {
			return nil
		}
		info, err := d.Info()
		if err == nil {
			n += info.Size()
		}
		return err
	})
	return n, err
}

// diskSpace reports free and total bytes of the filesystem holding path.
var diskSpace = func(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), nil
}

// checkScratchSpace refuses staging that would leave the scratch filesystem
// with less than 10% or 5 GiB free, whichever is larger.
func checkScratchSpace(dir string, need int64) error {
	free, total, err := diskSpace(dir)
	if err != nil {
		return err
	}
	reserve := max(total/10, 5<<30)
	if want := uint64(max(need, 0)) + reserve; free < want {
		return fmt.Errorf("backup scratch %s needs %d MiB free (%d MiB of mail databases plus a %d MiB reserve) but has %d MiB, %d MiB short; free space or set KYPOST_BACKUP_SCRATCH_DIR",
			dir, want>>20, need>>20, reserve>>20, free>>20, (want-free)>>20)
	}
	return nil
}

// RunBudget bounds one backup run: 16 minutes to seal and upload, plus, with
// bulk backup, 10 minutes and a minute per 2 GiB of mail databases, at most
// 4 hours more.
func (s *Service) RunBudget() time.Duration {
	if s.cfg.BulkRepository == "" {
		return depositBudget
	}
	n, _ := s.liveMailBytes()
	return depositBudget + min(10*time.Minute+time.Duration(n>>31)*time.Minute, 4*time.Hour)
}

// bulkKey returns the repository key, initializing a repository only when
// neither it nor a key exists. A key always means a repository existed, so an
// empty mountpoint or a repository with another ID is refused, never re-keyed.
func (s *Service) bulkKey(ctx context.Context) ([]byte, error) {
	repo, bin := s.cfg.BulkRepository, s.cfg.ResticBinary
	keyPath, idPath := filepath.Join(s.dirs.Secret, bulkKeyName), filepath.Join(s.dirs.Secret, bulkRepoIDName)
	empty := false
	if !strings.HasPrefix(repo, "rest:") {
		entries, err := os.ReadDir(repo)
		switch {
		case errors.Is(err, os.ErrNotExist) || err == nil && len(entries) == 0:
			empty = true
		case err != nil:
			return nil, err
		default:
			config, configErr := os.Lstat(filepath.Join(repo, "config"))
			keys, keysErr := os.Lstat(filepath.Join(repo, "keys"))
			if configErr != nil || keysErr != nil || !config.Mode().IsRegular() || !keys.IsDir() {
				return nil, errors.New("KYPOST_BULK_BACKUP_REPOSITORY is neither empty nor a restic repository")
			}
		}
	}
	key, err := cryptutil.LoadKey(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		if !empty && !strings.HasPrefix(repo, "rest:") {
			return nil, fmt.Errorf("restic repository exists but private/%s is missing; restore it from a capsule rather than generating a new one", bulkKeyName)
		}
		if key, err = cryptutil.LoadOrCreateKey(keyPath); err != nil {
			return nil, err
		}
		if err := runRestic(ctx, bin, repo, key, "", nil, "init"); err != nil {
			// The key was created just above and no repository accepted it.
			return nil, errors.Join(err, os.Remove(keyPath))
		}
		id, err := repositoryID(ctx, bin, repo, key)
		if err != nil {
			return nil, err
		}
		return key, fsutil.AtomicWriteFile(idPath, []byte(id+"\n"), 0600)
	}
	if err != nil {
		return nil, fmt.Errorf("private/%s is unusable; restore it from a capsule rather than generating a new one: %w", bulkKeyName, err)
	}
	if empty {
		return nil, fmt.Errorf("private/%s exists but KYPOST_BULK_BACKUP_REPOSITORY is empty; mount the repository it belongs to instead of starting a new one", bulkKeyName)
	}
	id, err := repositoryID(ctx, bin, repo, key)
	if err != nil {
		return nil, err
	}
	recorded, err := os.ReadFile(idPath)
	if errors.Is(err, os.ErrNotExist) {
		return key, fsutil.AtomicWriteFile(idPath, []byte(id+"\n"), 0600)
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(recorded)) != id {
		return nil, fmt.Errorf("restic repository ID %s is not the recorded %s; a different repository is mounted", id, strings.TrimSpace(string(recorded)))
	}
	return key, nil
}

func repositoryID(ctx context.Context, bin, repo string, key []byte) (string, error) {
	var out limitedBuffer
	if err := runRestic(ctx, bin, repo, key, "", &out, "--no-lock", "cat", "config"); err != nil {
		return "", err
	}
	var config struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out.Bytes(), &config); err != nil || !snapshotID.MatchString(config.ID) {
		return "", errors.New("restic repository config has no ID")
	}
	return config.ID, nil
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
	// --quiet leaves one JSON summary line (restic 0.13+).
	var out limitedBuffer
	if err := runRestic(ctx, s.cfg.ResticBinary, s.cfg.BulkRepository, key, stage.dir, &out, "backup", "--json", "--quiet", "--host", "kypost", "--tag", "kypost-mail", "mail"); err != nil {
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
	s.collectedBulk = &BulkSnapshot{Snapshot: m.Snapshot, At: time.Now().UTC().Format(time.RFC3339), SizeBytes: total}
	return json.Marshal(m)
}

// recordBulk stamps the snapshot of a capsule that reached a destination.
func (s *Service) recordBulk() {
	if s.collectedBulk == nil {
		return
	}
	raw, err := json.Marshal(s.collectedBulk)
	if err == nil {
		err = s.settings.Set(bulkSetting, string(raw))
	}
	if err != nil {
		slog.Warn("backup bulk status not recorded", "actor", "backup", "task_id", "backup", "action", "record_bulk", "result", "failure")
	}
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
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("invalid mail bulk manifest: trailing data")
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
	if recorded, err := os.ReadFile(filepath.Join(staging, "private", bulkRepoIDName)); err == nil {
		id, err := repositoryID(ctx, resticBinary, repository, key)
		if err != nil {
			return err
		}
		if id != strings.TrimSpace(string(recorded)) {
			return fmt.Errorf("restic repository ID %s is not the %s this capsule recorded", id, strings.TrimSpace(string(recorded)))
		}
	}
	scratch, err := os.MkdirTemp(filepath.Dir(staging), "bulk-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	// --no-lock lets a read-only repository mount restore.
	if err := runRestic(ctx, resticBinary, repository, key, "", nil, "--no-lock", "restore", m.Snapshot+":"+bulkRoot, "--target", scratch, "--verify"); err != nil {
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

// redactRepository hides a rest: URL's password for the screen and errors.
func redactRepository(repo string) string {
	if rest, ok := strings.CutPrefix(repo, "rest:"); ok {
		if u, err := url.Parse(rest); err == nil {
			return "rest:" + u.Redacted()
		}
		return "rest:(unparseable)"
	}
	return repo
}

// runRestic never uses a shell and gives restic a minimal environment. The
// repository (which may carry rest: credentials) and the password reach it
// only there: never argv, a file or a log.
func runRestic(ctx context.Context, bin, repo string, key []byte, dir string, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, append([]string{"--no-cache"}, args...)...)
	cmd.Dir = dir
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "LANG"} {
		if v, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+v)
		}
	}
	cmd.Env = append(cmd.Env, "RESTIC_REPOSITORY="+repo, "RESTIC_PASSWORD="+hex.EncodeToString(key))
	cmd.Stdout = stdout
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := tail(redactSecrets(string(stderr.b), repo), 512)
		verb := args[0]
		if verb == "--no-lock" {
			verb = args[1]
		}
		return fmt.Errorf("restic %s failed: %w: %s", verb, err, msg)
	}
	return nil
}

// limitedBuffer keeps the first 1 MiB of stdout; --quiet leaves restic's JSON one summary line.
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	_, _ = b.Buffer.Write(p[:min(len(p), max(0, 1<<20-b.Len()))])
	return len(p), nil
}

// tailBuffer keeps the last 64 KiB of stderr, where restic puts the fatal
// error. Secrets are removed from that window before it is cut to the 512
// bytes shown, so a cut can never leave part of a credential behind.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 64<<10 {
		t.b = append([]byte(nil), t.b[len(t.b)-64<<10:]...)
	}
	return len(p), nil
}

// redactSecrets removes a rest: repository's credentials in every form restic
// may print them: the whole URL with or without the scheme prefix, and the
// password raw or URL-escaped.
func redactSecrets(msg, repo string) string {
	rest, ok := strings.CutPrefix(repo, "rest:")
	if !ok {
		return msg
	}
	msg = strings.ReplaceAll(msg, repo, redactRepository(repo))
	if u, err := url.Parse(rest); err == nil {
		msg = strings.ReplaceAll(msg, rest, u.Redacted())
		if pw, set := u.User.Password(); set && pw != "" {
			for _, form := range []string{pw, url.PathEscape(pw), url.QueryEscape(pw)} {
				msg = strings.ReplaceAll(msg, form, "xxxxx")
			}
		}
	}
	return msg
}

// tail returns the last n bytes of s, trimmed.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
}
