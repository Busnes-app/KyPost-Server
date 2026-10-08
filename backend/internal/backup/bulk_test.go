//go:build linux

package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/ky-primitives/recoverykey"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// resticForTest is the real restic CLI. CI installs it, so a missing binary
// there is a failure rather than a silent skip.
func resticForTest(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("restic")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("restic CLI required in CI")
		}
		t.Skip("restic CLI required")
	}
	return bin
}

type bulkFixture struct {
	s       *Service
	u       users.User
	key     recoverykey.PrivateKey
	mail    []byte
	id      int64
	a       sso.NativeAssignment
	capsule []byte
}

// bulkService is a native deployment with one delivered message, a receiving
// database and, when probe > 0, that many extra bytes in the mailbox.
func bulkService(t *testing.T, probe int64) *bulkFixture {
	t.Helper()
	ctx := context.Background()
	s, u := nativeService(t)
	s.cfg.BulkRepository = filepath.Join(t.TempDir(), "repo")
	s.cfg.ResticBinary = resticForTest(t)
	f := &bulkFixture{s: s, u: u, key: pinTestKey(t, s)}
	life := sso.NewLifecycleStore(s.dirs.Config)
	a, ok, err := life.NativeAssignment(u.NativeMailboxIssuer, u.SSOSub)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	addresses, err := life.NativeAddresses()
	if err != nil {
		t.Fatal(err)
	}
	store, err := mailbox.Open(filepath.Join(s.dirs.State, "users", u.ID, "mailbox"), a.Owner, a.Limits)
	if err != nil {
		t.Fatal(err)
	}
	f.mail = []byte("From: sender@outside.test\r\nTo: one@example.test\r\nSubject: bulk\r\n\r\nexact bytes\x00\xff\r\n")
	f.id, err = store.Import(ctx, mailbox.Receipt{Gateway: "qualified-test", Delivery: "one", Sender: "sender@outside.test", Recipients: []mailbox.Recipient{{Address: a.Address, Generation: addresses[a.Address].Generation}}}, bytes.NewReader(f.mail))
	_ = store.Close()
	if err != nil {
		t.Fatal(err)
	}
	holding, err := ingress.Open(filepath.Join(s.dirs.State, "receiving"), ingress.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100})
	if err != nil {
		t.Fatal(err)
	}
	_ = holding.Close()
	if probe > 0 {
		db, err := sql.Open("sqlite", filepath.Join(s.dirs.State, "users", u.ID, "mailbox", "mailbox.db"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`CREATE TABLE capacity_probe (raw BLOB); INSERT INTO capacity_probe VALUES (zeroblob(?))`, probe)
		_ = db.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f.capsule, err = os.ReadFile(result.LocalPath); err != nil {
		t.Fatal(err)
	}
	f.a = a
	return f
}

func (f *bulkFixture) open(t *testing.T) (string, BulkManifest) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if _, _, err := capsule.Open(f.capsule, f.key, dir); err != nil {
		t.Fatal(err)
	}
	var m BulkManifest
	raw, err := os.ReadFile(filepath.Join(dir, bulkManifestPath))
	if err != nil || json.Unmarshal(raw, &m) != nil {
		t.Fatal("capsule lacks its bulk manifest", err)
	}
	return dir, m
}

func emptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("%s not clean: %v %v", dir, entries, err)
	}
}

// A mailbox past the capsule's per-file cap backs up through restic without
// being read into memory, restores byte-exact and passes the drill.
func TestBulkBackupLargeMailboxRoundTrip(t *testing.T) {
	ctx := context.Background()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f := bulkService(t, recoveryclient.MaxCapsuleFileBytes+1)
	runtime.ReadMemStats(&after)
	// Every byte buffered would count here; streaming keeps the whole run under the file size.
	if grew := after.TotalAlloc - before.TotalAlloc; grew > uint64(recoveryclient.MaxCapsuleFileBytes) {
		t.Fatalf("backup allocated %d bytes for a %d byte mailbox", grew, recoveryclient.MaxCapsuleFileBytes+1)
	}
	if len(f.capsule) > 1<<20 {
		t.Fatalf("capsule carries mail: %d bytes", len(f.capsule))
	}
	emptyDir(t, filepath.Join(f.s.dirs.State, scratchDirName))
	dir, m := f.open(t)
	// Snapshot order: receiving before any mailbox.
	if len(m.Files) != 2 || m.Files[0].Path != "state/receiving/ingress.db" || m.Files[1].Path != "state/users/"+f.u.ID+"/mailbox/mailbox.db" || m.Files[1].Size <= recoveryclient.MaxCapsuleFileBytes {
		t.Fatalf("manifest %+v", m.Files)
	}
	for _, rel := range []string{"state/receiving/ingress.db", m.Files[1].Path} {
		if _, err := os.Lstat(filepath.Join(dir, rel)); !os.IsNotExist(err) {
			t.Fatalf("capsule itself carries %s", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "private", bulkKeyName)); err != nil {
		t.Fatal("capsule lacks the repository key", err)
	}
	if err := RestoreBulk(ctx, dir, f.s.cfg.BulkRepository, f.s.cfg.ResticBinary); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(dir)); len(entries) != 1 {
		t.Fatal("restore scratch left behind", entries)
	}
	for _, want := range m.Files {
		sum, size, err := hashFile(filepath.Join(dir, want.Path))
		if err != nil || sum != want.SHA256 || size != want.Size {
			t.Fatal("restored bytes differ", want.Path, err)
		}
	}
	native, err := QuarantineNativeRestore(filepath.Join(dir))
	if !native || err != nil {
		t.Fatal(native, err)
	}
	restored, err := mailbox.OpenExisting(filepath.Join(dir, "state/users", f.u.ID, "mailbox"), f.a.Owner, f.a.Limits, f.a.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if got, err := restored.Raw(ctx, "INBOX", f.id); err != nil || !bytes.Equal(got, f.mail) {
		t.Fatal("mail lost", err)
	}
	status, err := f.s.Status()
	if err != nil || status.BulkRepo != f.s.cfg.BulkRepository || status.LastBulk == nil || status.LastBulk.Snapshot != m.Snapshot || status.LastBulk.SizeBytes != m.Files[0].Size+m.Files[1].Size {
		t.Fatalf("status %+v %v", status.LastBulk, err)
	}
	res, err := f.s.Drill(ctx)
	if err != nil || !res.Passed {
		t.Fatalf("drill %+v %v", res, err)
	}
	if res.Checks[2].Name != "bulk:restore" || !res.Checks[2].Passed {
		t.Fatalf("drill skipped the bulk restore: %+v", res.Checks)
	}
	// Only a delivered capsule's snapshot is shown; the drill's is not.
	if after, err := f.s.Status(); err != nil || after.LastBulk.Snapshot != m.Snapshot {
		t.Fatal("drill replaced the delivered snapshot in status", err)
	}
	emptyDir(t, filepath.Join(f.s.dirs.State, scratchDirName))
}

func TestBulkRestoreRefusals(t *testing.T) {
	ctx := context.Background()
	f := bulkService(t, 0)
	repo, bin := f.s.cfg.BulkRepository, f.s.cfg.ResticBinary
	key, err := cryptutil.LoadKey(filepath.Join(f.s.dirs.Secret, bulkKeyName))
	if err != nil {
		t.Fatal(err)
	}
	otherRepo := filepath.Join(t.TempDir(), "other")
	if err := runRestic(ctx, bin, otherRepo, key, "", nil, "init"); err != nil {
		t.Fatal(err)
	}
	tampered := filepath.Join(t.TempDir(), "tampered")
	if out, err := exec.Command("cp", "-a", repo, tampered).CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	packs, _ := filepath.Glob(filepath.Join(tampered, "data", "*", "*"))
	for _, p := range packs {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for i := range raw {
			raw[i] ^= 0x55
		}
		if err := os.Chmod(p, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	rewrite := func(dir string, change func(*BulkManifest)) {
		var m BulkManifest
		raw, _ := os.ReadFile(filepath.Join(dir, bulkManifestPath))
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		change(&m)
		raw, _ = json.Marshal(m)
		if err := os.WriteFile(filepath.Join(dir, bulkManifestPath), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]struct {
		repo  string
		setup func(dir string)
		want  string
	}{
		"no repository":    {"", nil, "KYPOST_BULK_BACKUP_REPOSITORY"},
		"other repository": {otherRepo, nil, "is not the"},
		"absent snapshot":  {otherRepo, func(d string) { _ = os.Remove(filepath.Join(d, "private", bulkRepoIDName)) }, "restic restore failed"},
		"trailing data": {repo, func(d string) {
			raw, _ := os.ReadFile(filepath.Join(d, bulkManifestPath))
			_ = os.WriteFile(filepath.Join(d, bulkManifestPath), append(raw, []byte(" {}")...), 0600)
		}, "trailing"},
		"tampered packs": {tampered, nil, "restic restore failed"},
		"wrong digest":   {repo, func(d string) { rewrite(d, func(m *BulkManifest) { m.Files[1].SHA256 = strings.Repeat("0", 64) }) }, "does not match"},
		"missing file": {repo, func(d string) {
			rewrite(d, func(m *BulkManifest) {
				m.Files = append(m.Files, BulkFile{"state/mailboxes/x/mailbox/mailbox.db", m.Files[0].SHA256, 1})
			})
		}, "missing files"},
		"extra file":   {repo, func(d string) { rewrite(d, func(m *BulkManifest) { m.Files = m.Files[:1] }) }, "unexpected"},
		"foreign path": {repo, func(d string) { rewrite(d, func(m *BulkManifest) { m.Files[0].Path = "config/users.json" }) }, "invalid"},
		"capsule has mail": {repo, func(d string) {
			_ = os.MkdirAll(filepath.Join(d, "state/receiving"), 0700)
			_ = os.WriteFile(filepath.Join(d, "state/receiving/ingress.db"), []byte("x"), 0600)
		}, "combine"},
		"missing key": {repo, func(d string) { _ = os.Remove(filepath.Join(d, "private", bulkKeyName)) }, bulkKeyName},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir, m := f.open(t)
			if c.setup != nil {
				c.setup(dir)
			}
			err := RestoreBulk(ctx, dir, c.repo, bin)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
			if _, statErr := os.Stat(filepath.Join(dir, m.Files[1].Path)); !os.IsNotExist(statErr) {
				t.Fatal("refused restore placed mail")
			}
			if strings.Contains(err.Error(), hex.EncodeToString(key)) {
				t.Fatal("error leaks the restic password")
			}
			entries, _ := os.ReadDir(filepath.Dir(dir))
			if len(entries) != 1 {
				t.Fatal("restore scratch left behind", entries)
			}
		})
	}
	// Without a manifest the mailbox's absence is validation's to refuse, never empty mail.
	dir, _ := f.open(t)
	if err := os.Remove(filepath.Join(dir, bulkManifestPath)); err != nil {
		t.Fatal(err)
	}
	if err := RestoreBulk(ctx, dir, repo, bin); err != nil {
		t.Fatal(err)
	}
	if native, err := QuarantineNativeRestore(dir); !native || err == nil {
		t.Fatal("restore without its mailbox validated", native, err)
	}
}

// The password reaches restic only through its environment, and inherited
// RESTIC_* settings cannot redirect it.
func TestBulkPasswordNeverInArgv(t *testing.T) {
	bin := resticForTest(t)
	logPath := filepath.Join(t.TempDir(), "argv.log")
	wrapper := filepath.Join(t.TempDir(), "restic")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + logPath + "'\nenv | sed 's/=.*//; s/^/ENV /' >> '" + logPath + "'\nexec '" + bin + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESTIC_PASSWORD", "operator-shell-password")
	t.Setenv("KYPOST_PROBE_SECRET", "must-not-reach-restic")
	t.Setenv("RESTIC_REPOSITORY", t.TempDir())
	t.Setenv("PATH", filepath.Dir(wrapper)+string(os.PathListSeparator)+os.Getenv("PATH"))
	f := bulkService(t, 0)
	dir, _ := f.open(t)
	if err := RestoreBulk(context.Background(), dir, f.s.cfg.BulkRepository, wrapper); err != nil {
		t.Fatal(err)
	}
	key, err := cryptutil.LoadKey(filepath.Join(f.s.dirs.Secret, bulkKeyName))
	if err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(logPath)
	if err != nil || !bytes.Contains(argv, []byte(" init")) || !bytes.Contains(argv, []byte(" backup ")) || !bytes.Contains(argv, []byte(" restore ")) || !bytes.Contains(argv, []byte("--verify")) {
		t.Fatalf("wrapper did not see every call: %s %v", argv, err)
	}
	if bytes.Contains(argv, []byte(hex.EncodeToString(key))) || bytes.Contains(argv, []byte(base64.StdEncoding.EncodeToString(key))) || bytes.Contains(argv, []byte(f.s.cfg.BulkRepository)) {
		t.Fatal("restic password or repository in argv")
	}
	if !bytes.Contains(argv, []byte("--no-lock restore")) {
		t.Fatal("restore takes a lock; a read-only repository mount would fail")
	}
	for _, line := range strings.Split(string(argv), "\n") {
		name, ok := strings.CutPrefix(line, "ENV ")
		if ok && (strings.HasPrefix(name, "KYPOST_") || strings.HasPrefix(name, "RESTIC_") && name != "RESTIC_PASSWORD" && name != "RESTIC_REPOSITORY") {
			t.Fatalf("restic inherited %s", name)
		}
	}
}

func TestBulkBackupFailures(t *testing.T) {
	ctx := context.Background()
	f := bulkService(t, 0)
	s := f.s
	before, _ := os.ReadDir(s.cfg.Dir)
	check := func(want string) {
		t.Helper()
		result, err := s.Run(ctx)
		if err == nil || !strings.Contains(err.Error(), want) || result.LocalPath != "" {
			t.Fatalf("want %q, got %+v %v", want, result, err)
		}
		if after, _ := os.ReadDir(s.cfg.Dir); len(after) != len(before) {
			t.Fatal("failed backup published a capsule")
		}
		emptyDir(t, filepath.Join(s.dirs.State, scratchDirName))
	}
	// A restic failure fails the whole backup.
	bin := s.cfg.ResticBinary
	failing := filepath.Join(t.TempDir(), "restic")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\ncase \"$*\" in *backup*) exit 1;; esac\nexec '"+bin+"' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s.cfg.ResticBinary = failing
	check("restic backup failed")
	s.cfg.ResticBinary = bin
	// An existing repository's key is never replaced.
	keyPath := filepath.Join(s.dirs.Secret, bulkKeyName)
	keyBytes, _ := os.ReadFile(keyPath)
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	check("restore it from a capsule")
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatal("backup generated a replacement repository key")
	}
	if err := os.WriteFile(keyPath, keyBytes, 0600); err != nil {
		t.Fatal(err)
	}
	// A key means a repository existed: an empty mountpoint is never initialized.
	repo := s.cfg.BulkRepository
	s.cfg.BulkRepository = t.TempDir()
	check("mount the repository")
	if entries, _ := os.ReadDir(s.cfg.BulkRepository); len(entries) != 0 {
		t.Fatal("empty mountpoint was initialized")
	}
	// Another repository the same key opens is not the recorded one.
	other := filepath.Join(t.TempDir(), "other")
	key, _ := cryptutil.LoadKey(keyPath)
	if err := runRestic(ctx, bin, other, key, "", nil, "init"); err != nil {
		t.Fatal(err)
	}
	s.cfg.BulkRepository = other
	check("is not the recorded")
	s.cfg.BulkRepository = repo
	// A non-empty directory that is not a restic repository is refused.
	s.cfg.BulkRepository = t.TempDir()
	if err := os.WriteFile(filepath.Join(s.cfg.BulkRepository, "notes.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	check("neither empty nor a restic repository")
}

func TestBulkRepositoryOutsideDataRoots(t *testing.T) {
	d := fixtureDirs(t)
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(d.State, link); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{filepath.Join(d.State, "repo"), d.Secret, filepath.Dir(d.Config), filepath.Join(link, "repo")} {
		bc := config.BackupConfig{Keep: 1, BulkRepository: repo}
		if _, err := New(d, bc, nil, "t"); err == nil {
			t.Fatalf("repository %s overlapping data roots accepted", repo)
		}
	}
	local := t.TempDir()
	if _, err := New(d, config.BackupConfig{Keep: 1, Dir: local, BulkRepository: filepath.Join(local, "repo")}, nil, "t"); err == nil {
		t.Fatal("repository inside the local capsule directory accepted")
	}
	if _, err := New(d, config.BackupConfig{Keep: 1, ScratchDir: filepath.Join(d.State, "scratch")}, nil, "t"); err == nil {
		t.Fatal("scratch directory inside STATE_DIR accepted")
	}
}

// Every run backs up from one path under one host, so restic retention groups
// them as one series and --keep-last 1 really removes the older snapshot.
func TestBulkRetentionGroupsRuns(t *testing.T) {
	ctx := context.Background()
	f := bulkService(t, 0)
	if _, err := f.s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	key, err := cryptutil.LoadKey(filepath.Join(f.s.dirs.Secret, bulkKeyName))
	if err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var out limitedBuffer
		if err := runRestic(ctx, f.s.cfg.ResticBinary, f.s.cfg.BulkRepository, key, "", &out, "snapshots", "--json", "--tag", "kypost-mail"); err != nil {
			t.Fatal(err)
		}
		var snaps []json.RawMessage
		if err := json.Unmarshal(out.Bytes(), &snaps); err != nil {
			t.Fatal(err)
		}
		return len(snaps)
	}
	if n := count(); n != 2 {
		t.Fatalf("%d snapshots, want 2", n)
	}
	if err := runRestic(ctx, f.s.cfg.ResticBinary, f.s.cfg.BulkRepository, key, "", nil, "forget", "--tag", "kypost-mail", "--keep-last", "1"); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("forget --keep-last 1 left %d snapshots", n)
	}
}

// A killed run's plaintext copies are swept at the next collection, inside the
// dedicated child of a configured scratch directory only.
func TestBulkSweepsStaleScratch(t *testing.T) {
	f := bulkService(t, 0)
	f.s.cfg.ScratchDir = t.TempDir()
	root := f.s.scratchRoot()
	stale := []string{"bulk/mail/state/receiving/ingress.db", "snapshot-1/x.db", "bulk-restore-2/state/users/u/mailbox/mailbox.db", "recoveryclient-drill-3/state/state.db"}
	for _, rel := range append(stale, "../operator-file") {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("plaintext"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	emptyDir(t, root)
	if _, err := os.Stat(filepath.Join(f.s.cfg.ScratchDir, "operator-file")); err != nil {
		t.Fatal("sweep touched a file it does not own", err)
	}
}

func TestBulkRefusesWithoutScratchSpace(t *testing.T) {
	f := bulkService(t, 0)
	defer func(orig func(string) (uint64, uint64, error)) { diskSpace = orig }(diskSpace)
	// A 10 GiB reserve (10% of 100 GiB) plus the mail exceeds 6 GiB free.
	diskSpace = func(string) (uint64, uint64, error) { return 6 << 30, 100 << 30, nil }
	_, err := f.s.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "MiB short") || !strings.Contains(err.Error(), "KYPOST_BACKUP_SCRATCH_DIR") {
		t.Fatal("staging without space accepted", err)
	}
	emptyDir(t, f.s.scratchRoot())
	diskSpace = func(string) (uint64, uint64, error) { return 20 << 30, 100 << 30, nil }
	if _, err := f.s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBulkRunBudgetGrowsWithMail(t *testing.T) {
	s := validService(t)
	if s.RunBudget() != 16*time.Minute {
		t.Fatal("budget without bulk", s.RunBudget())
	}
	s.cfg.BulkRepository = filepath.Join(t.TempDir(), "repo")
	db := filepath.Join(s.dirs.State, "users", "u1", "mailbox", "mailbox.db")
	if err := os.MkdirAll(filepath.Dir(db), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(db, 4<<30); err != nil {
		t.Fatal(err)
	}
	if got := s.RunBudget(); got != 28*time.Minute {
		t.Fatalf("4 GiB budget %s, want 28m", got)
	}
}

func TestBulkRestRepositoryRedacted(t *testing.T) {
	repo := "rest:https://kypost:hunter2@backup.example:8000/kypost"
	s := openService(t, fixtureDirs(t), config.BackupConfig{BulkRepository: repo})
	st, err := s.Status()
	if err != nil || strings.Contains(st.BulkRepo, "hunter2") || !strings.Contains(st.BulkRepo, "backup.example") {
		t.Fatal(st.BulkRepo, err)
	}
	fake := filepath.Join(t.TempDir(), "restic")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"Fatal: unable to open $RESTIC_REPOSITORY\" >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	err = runRestic(context.Background(), fake, repo, make([]byte, 32), "", nil, "init")
	if err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "backup.example") {
		t.Fatal("restic error not redacted", err)
	}
}

// Credentials never survive in restic's error, whatever form restic prints and
// wherever the 512-byte cut falls.
func TestBulkRestRepositoryRedactedInEveryForm(t *testing.T) {
	repo := "rest:https://kypost:s3cr%2Fet!@backup.example:8000/kypost"
	pw := "s3cr/et!"
	url := strings.TrimPrefix(repo, "rest:")
	// The cut lands two characters into the escaped password of the last copy.
	cut := url[strings.Index(url, "s3cr")+2:]
	stderr := "open " + url + " failed\npassword was " + pw + "\n" + url + strings.Repeat("x", 512-len(cut))
	dir := t.TempDir()
	msgFile := filepath.Join(dir, "stderr")
	if err := os.WriteFile(msgFile, []byte(stderr), 0600); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "restic")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat '"+msgFile+"' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	err := runRestic(context.Background(), fake, repo, make([]byte, 32), "", nil, "init")
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, leak := range []string{pw, "s3cr", "cr%2Fet", "et!@"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("restic error leaks %q: %v", leak, err)
		}
	}
}

// Only state/** mail databases go to restic, matching what restore accepts.
func TestBulkStagesOnlyStateDatabases(t *testing.T) {
	f := bulkService(t, 0)
	db, err := sql.Open("sqlite", filepath.Join(f.s.dirs.Config, "mailbox.db"))
	if err == nil {
		_, err = db.Exec(`CREATE TABLE t (x)`)
		_ = db.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.s.Collect()
	if err != nil {
		t.Fatal(err)
	}
	inCapsule := false
	for _, file := range p.Files {
		inCapsule = inCapsule || file.Path == "config/mailbox.db"
		if file.Path == bulkManifestPath {
			var m BulkManifest
			if err := json.Unmarshal(file.Data, &m); err != nil {
				t.Fatal(err)
			}
			for _, b := range m.Files {
				if !strings.HasPrefix(b.Path, "state/") {
					t.Fatal("staged outside state:", b.Path)
				}
			}
		}
	}
	if !inCapsule {
		t.Fatal("non-state database dropped")
	}
}

// A key nobody's repository accepted is not left behind to block the next init.
func TestBulkInitFailureLeavesNoKey(t *testing.T) {
	s := validService(t)
	s.cfg.BulkRepository = filepath.Join(t.TempDir(), "repo")
	s.cfg.ResticBinary = "/bin/false"
	pinTestKey(t, s)
	if _, err := s.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "restic init failed") {
		t.Fatal("failed init accepted", err)
	}
	if _, err := os.Stat(filepath.Join(s.dirs.Secret, bulkKeyName)); !os.IsNotExist(err) {
		t.Fatal("failed init left its key", err)
	}
}
