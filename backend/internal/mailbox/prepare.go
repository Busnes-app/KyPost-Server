package mailbox

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

var ErrPreparation = errors.New("native account preparation conflicts with existing state; preserve account data and reconcile provisioning")

const preparationFile = "native-mailbox.json"

type preparedAccount struct {
	Owner   Owner  `json:"owner"`
	Address string `json:"address"`
	Limits  Limits `json:"limits"`
	Source  string `json:"source"`
}

// PrepareAccount creates storage only, never routing or ready-to-receive state.
// The caller must prove a NEW local account's verified issuer/subject ownership,
// domain authority and unique primary address. It is not an account migration.
// Publication atomically includes an empty mailbox and prebound account state;
// retry validates existing files without adopting legacy/recreated databases.
func PrepareAccount(stateRoot string, owner Owner, address string, limits Limits) (string, error) {
	return PrepareAccountContext(context.Background(), stateRoot, owner, address, limits)
}

// PrepareAccountContext cancels lock contention and checks cancellation before
// publication. Blocking disk calls remain a separate deployment qualification.
func PrepareAccountContext(ctx context.Context, stateRoot string, owner Owner, address string, limits Limits) (string, error) {
	return prepareAccountContext(ctx, filepath.Join(stateRoot, "users"), owner, address, limits, publishPreparedAccount)
}

// PrepareMailboxContext prepares a mailbox under parent ($STATE_DIR/mailboxes
// for extra mailboxes). Its state.db is created like an account's but holds
// mail state only; callers never write device or notification rows to it.
func PrepareMailboxContext(ctx context.Context, parent string, owner Owner, address string, limits Limits) (string, error) {
	return prepareAccountContext(ctx, parent, owner, address, limits, publishPreparedAccount)
}

// root is the parent directory the mailbox is published into.
func prepareAccountContext(ctx context.Context, root string, owner Owner, address string, limits Limits, publish func(string, string) error) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	a, err := mail.ParseAddress(address)
	if !fsutil.SafePathComponent(owner.Mailbox) || err != nil || a.Address != address {
		return "", ErrPreparation
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	target := filepath.Join(root, owner.Mailbox)
	release, err := fsutil.LockFileContext(ctx, target)
	if err != nil {
		return "", err
	}
	defer release()
	if _, err = os.Lstat(target); err == nil {
		if dbbackend.Postgres() {
			// A published directory is complete: settle what a crash left pending.
			if err = dbbackend.Settle(ctx, filepath.Join(target, "state.db"), filepath.Join(target, "mailbox", "mailbox.db")); err != nil {
				return "", err
			}
		}
		return preparedSource(target, owner, address, limits)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// ponytail: crash-abandoned staging directories are empty-mail preparation,
	// never live mailboxes. Add explicit bounded orphan cleanup before activation.
	stage, err := os.MkdirTemp(root, ".native-prepare-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if dbbackend.Postgres() {
		// The stage's schemas, unless published under the target's names.
		defer func() { _ = dbbackend.Remove(stage) }()
	}
	store, err := Open(filepath.Join(stage, "mailbox"), owner, limits)
	if err != nil {
		return "", err
	}
	client, err := NewClient(store, address)
	if err != nil {
		_ = store.Close()
		return "", err
	}
	source := client.MailSourceIdentity()
	if err = store.Close(); err != nil {
		return "", err
	}
	account := filepath.Join(stage, "account")
	st, err := state.NewNative(account, source)
	if err != nil {
		return "", err
	}
	if err = st.Close(); err != nil {
		return "", err
	}
	if err = os.Rename(filepath.Join(stage, "mailbox"), filepath.Join(account, "mailbox")); err != nil {
		return "", err
	}
	for _, path := range []string{filepath.Join(account, "state.db"), filepath.Join(account, "mailbox", "mailbox.db")} {
		if dbbackend.Postgres() {
			break
		}
		file, e := os.Open(path)
		if e != nil {
			return "", e
		}
		e = fsutil.Sync(file)
		closeErr := file.Close()
		if e != nil {
			return "", e
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	if err = fsutil.PersistJSONFile(filepath.Join(account, preparationFile), preparedAccount{owner, address, limits, source}); err != nil {
		return "", err
	}
	if err = fsutil.SyncDir(account); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if dbbackend.Postgres() {
		// The schemas move to the target's names, pending, in one transaction; the
		// directory is published; then they settle. A crash before publication leaves
		// pending schemas the next attempt replaces, one after it a published directory
		// whose retry settles them (preparedSource), as a published SQLite directory is
		// complete.
		targets := []string{filepath.Join(target, "state.db"), filepath.Join(target, "mailbox", "mailbox.db")}
		err = dbbackend.Stage(ctx, map[string]string{
			filepath.Join(stage, "account", "state.db"):   targets[0],
			filepath.Join(stage, "mailbox", "mailbox.db"): targets[1],
		}, publishSchemasStep)
		if err == nil {
			if err = publish(account, target); err != nil {
				err = errors.Join(err, dbbackend.Unstage(context.WithoutCancel(ctx), targets...))
			} else {
				err = dbbackend.Settle(context.WithoutCancel(ctx), targets...)
			}
		}
	} else {
		err = publish(account, target)
	}
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrPreparation, err)
	}
	if err = fsutil.SyncDir(root); err != nil {
		return "", err
	}
	return source, nil
}

// publishSchemasStep runs before the Postgres publication commits; tests fail it.
var publishSchemasStep = func() error { return nil }

// ValidatePreparedAccount checks published preparation without recreating any
// missing state. A provisioner with an acknowledged source must use this on
// repair; PrepareAccount would legitimately create a new directory if absent.
func ValidatePreparedAccount(stateRoot string, owner Owner, address string, limits Limits) (string, error) {
	if !fsutil.SafePathComponent(owner.Mailbox) {
		return "", ErrPreparation
	}
	return ValidatePreparedMailbox(filepath.Join(stateRoot, "users"), owner, address, limits)
}

// ValidatePreparedMailbox is ValidatePreparedAccount under an explicit parent.
func ValidatePreparedMailbox(parent string, owner Owner, address string, limits Limits) (string, error) {
	if !fsutil.SafePathComponent(owner.Mailbox) {
		return "", ErrPreparation
	}
	return preparedSource(filepath.Join(parent, owner.Mailbox), owner, address, limits)
}

// Check before any constructor: constructors create missing schemas/identities.
// Read-only opens reject incomplete state without silently initializing it.
func preparedSource(dir string, owner Owner, address string, limits Limits) (string, error) {
	p := &Prepared{dir: dir}
	defer p.Close()
	source, err := p.check(owner, address, limits)
	if err != nil {
		return "", err
	}
	// A successful retry also repairs a lost acknowledgement of the parent sync.
	if err = fsutil.SyncDir(filepath.Dir(dir)); err != nil {
		return "", err
	}
	return source, nil
}

// Prepared keeps a prepared mailbox's state.db and mailbox.db open read-only,
// so per-operation admission re-runs every preparation check on these handles
// instead of reopening both databases. Each check also requires both paths to
// still name the files the handles opened.
type Prepared struct {
	mu                  sync.Mutex // an open client's checks run concurrently under a shared fence
	dir                 string
	state, mail         *heldDB
	stateFile, mailFile os.FileInfo
	// The last native-mailbox.json bytes decoded and their decoding; a check
	// decodes again only when the bytes it reads differ. Guarded by mu.
	raw     []byte
	decoded preparedAccount
	// Guarded by heldAdmissions: the last pooled admission, for the idle
	// close, and the admissions checking p now plus open clients holding it.
	used  time.Time
	users int
}

// ValidatePreparedMailbox is the package function; for p's own mailbox it
// reuses p's handles. A nil p or another mailbox uses checkHeld.
func (p *Prepared) ValidatePreparedMailbox(parent string, owner Owner, address string, limits Limits) (string, error) {
	dir, err := filepath.Abs(filepath.Join(parent, owner.Mailbox))
	if err != nil || !fsutil.SafePathComponent(owner.Mailbox) {
		return ValidatePreparedMailbox(parent, owner, address, limits)
	}
	if p != nil && dir == p.dir {
		source, err := p.check(owner, address, limits)
		heldAdmissions.Lock()
		if heldAdmissions.m[dir] == p { // a client's check keeps its pool entry fresh, or evicts it
			if err != nil {
				delete(heldAdmissions.m, dir)
			} else {
				p.used = time.Now()
			}
		}
		heldAdmissions.Unlock()
		return source, err
	}
	return checkHeld(dir, owner, address, limits)
}

// MaxHeldAdmissions is the bound the api and daemon pass to
// HoldAdmissionHandles: a quarter of this process's memory limit at 512 KiB a
// mailbox. A held mailbox measured about 420 KiB and 6 descriptors; past the
// bound a mailbox reopens and re-parses both databases on every admission,
// which took a quarter of the api's CPU at 2,000 accounts with 1,024 held.
func MaxHeldAdmissions() int { return heldBound(memoryLimit()) }

// heldBound keeps at least 1,024 (a 2 GiB limit) and at most 32,768, about
// 200,000 descriptors (Go raises the soft descriptor limit to the hard one).
func heldBound(limit int) int { return min(max(limit/4/(512<<10), 1024), 32768) }

// memoryLimit is this process's cgroup v2 memory.max, else the machine's
// memory, in bytes; 0 if neither can be read.
func memoryLimit() int {
	raw, _ := os.ReadFile("/proc/self/cgroup")
	for line := range strings.Lines(string(raw)) {
		if path, ok := strings.CutPrefix(strings.TrimSpace(line), "0::"); ok {
			limit, _ := os.ReadFile(filepath.Join("/sys/fs/cgroup", path, "memory.max"))
			if n, err := strconv.Atoi(strings.TrimSpace(string(limit))); err == nil {
				return n
			}
		}
	}
	raw, _ = os.ReadFile("/proc/meminfo")
	var kib int
	_, _ = fmt.Sscanf(string(raw), "MemTotal: %d kB", &kib)
	return kib << 10
}

// heldIdle closes a held mailbox after 5–10 minutes without an admission:
// longer than a scan interval (90 s by default), short enough that an
// offboarded mailbox's deleted files are released.
const heldIdle = 5 * time.Minute

// heldAdmissions holds Prepared handles between admissions, one per mailbox.
// Admissions without a client and clients opened while it is held share it
// (checks serialize on its mu). A passed check keeps it, a refused one drops
// it; it closes once it is out of the map and no admission or client uses it.
var heldAdmissions = struct {
	sync.Mutex
	max   int
	m     map[string]*Prepared
	timer *time.Timer
}{m: map[string]*Prepared{}}

// HoldAdmissionHandles lets this process hold up to max mailboxes between
// admissions (0, the default, holds none) and closes those held now. It is
// for long-running processes; short commands run with few descriptors.
func HoldAdmissionHandles(max int) {
	var unused []*Prepared
	heldAdmissions.Lock()
	heldAdmissions.max = max
	for _, p := range heldAdmissions.m {
		if p.users == 0 {
			unused = append(unused, p)
		}
	}
	heldAdmissions.m = map[string]*Prepared{}
	heldAdmissions.Unlock()
	for _, p := range unused {
		p.Close()
	}
}

// checkHeld is the full check with this process's held handles for dir, if
// any. A handle opened here repairs a lost parent sync, as preparedSource
// does; a held one did so when it opened (the #571 rule). A failed check
// drops the handles from the pool, so the next admission reopens.
func checkHeld(dir string, owner Owner, address string, limits Limits) (string, error) {
	p, opened := useHeld(dir)
	source, err := p.check(owner, address, limits)
	if err == nil && opened {
		err = fsutil.SyncDir(filepath.Dir(dir))
	}
	if err != nil {
		dropHeld(p)
		return "", err
	}
	heldAdmissions.Lock()
	holdAdmission(p)
	heldAdmissions.Unlock()
	releaseHeld(p)
	return source, nil
}

// dropHeld ends a use whose check refused: p leaves the pool.
func dropHeld(p *Prepared) {
	heldAdmissions.Lock()
	if heldAdmissions.m[p.dir] == p {
		delete(heldAdmissions.m, p.dir)
	}
	heldAdmissions.Unlock()
	releaseHeld(p)
}

// useHeld returns dir's held Prepared, or a new one, for one more user;
// releaseHeld ends that use.
func useHeld(dir string) (p *Prepared, opened bool) {
	heldAdmissions.Lock()
	defer heldAdmissions.Unlock()
	if p = heldAdmissions.m[dir]; p == nil {
		p, opened = &Prepared{dir: dir}, true
	}
	p.users++
	return p, opened
}

// releaseHeld closes p once it is out of the pool and unused.
func releaseHeld(p *Prepared) {
	heldAdmissions.Lock()
	p.users--
	if p.users < 0 {
		heldAdmissions.Unlock()
		panic("mailbox: held handles released twice")
	}
	unused := p.users == 0 && heldAdmissions.m[p.dir] != p
	heldAdmissions.Unlock()
	if unused {
		p.Close()
	}
}

// holdAdmission keeps p unless the bound is reached or the mailbox already
// has an entry; the caller holds heldAdmissions.
func holdAdmission(p *Prepared) bool {
	if old := heldAdmissions.m[p.dir]; old != nil || len(heldAdmissions.m) >= heldAdmissions.max {
		if old == p {
			p.used = time.Now()
		}
		return old == p
	}
	p.used = time.Now()
	heldAdmissions.m[p.dir] = p
	if heldAdmissions.timer == nil {
		heldAdmissions.timer = time.AfterFunc(heldIdle, closeIdleAdmissions)
	}
	return true
}

// closeIdleAdmissions drops the mailboxes not admitted for heldIdle and
// closes those no client or admission uses.
func closeIdleAdmissions() {
	var idle []*Prepared
	heldAdmissions.Lock()
	for dir, p := range heldAdmissions.m {
		if time.Since(p.used) >= heldIdle {
			if p.users == 0 {
				idle = append(idle, p)
			}
			delete(heldAdmissions.m, dir)
		}
	}
	heldAdmissions.timer = nil
	if len(heldAdmissions.m) > 0 {
		heldAdmissions.timer = time.AfterFunc(heldIdle, closeIdleAdmissions)
	}
	heldAdmissions.Unlock()
	for _, p := range idle {
		p.Close()
	}
}

// Close releases the handles.
func (p *Prepared) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, db := range []*heldDB{p.state, p.mail} {
		if db != nil {
			db.Close()
		}
	}
}

// heldDB prepares each check query once per handle. SQLite re-prepares a
// statement itself after a schema change and every run reads the file anew,
// so only the parse is reused, never a value.
type heldDB struct {
	*sql.DB
	mu    sync.Mutex
	stmts map[string]*sql.Stmt
	pg    bool // a Postgres schema, not a file
}

func (h *heldDB) QueryRow(query string, args ...any) *sql.Row {
	h.mu.Lock()
	s := h.stmts[query]
	h.mu.Unlock()
	if s == nil {
		var err error
		if s, err = h.Prepare(query); err != nil {
			return h.DB.QueryRow(query, args...) // reports the error on Scan
		}
		h.mu.Lock()
		if old := h.stmts[query]; old != nil {
			_ = s.Close()
			s = old
		} else {
			h.stmts[query] = s
		}
		h.mu.Unlock()
	}
	return s.QueryRow(args...)
}

func (h *heldDB) Close() {
	h.mu.Lock()
	for _, s := range h.stmts {
		_ = s.Close()
	}
	h.mu.Unlock()
	_ = h.DB.Close()
}

// check opens the handles on first use, recording each file before its first
// connection; a file still in place after the queries is the one they read.
func (p *Prepared) check(owner Owner, address string, limits Limits) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	dir := p.dir
	for _, path := range []string{dir, filepath.Join(dir, "mailbox")} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return "", ErrPreparation
		}
	}
	path := filepath.Join(dir, preparationFile)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8192 {
		return "", ErrPreparation
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if p.raw == nil || !bytes.Equal(raw, p.raw) {
		var decoded preparedAccount
		if err = json.Unmarshal(raw, &decoded); err != nil {
			return "", ErrPreparation
		}
		p.raw, p.decoded = raw, decoded
	}
	pa := p.decoded
	if pa.Owner != owner || pa.Address != address || pa.Limits != limits {
		return "", ErrPreparation
	}
	statePath, mailPath := filepath.Join(dir, "state.db"), filepath.Join(dir, "mailbox", "mailbox.db")
	if p.state == nil {
		if p.state, p.stateFile, err = openHeld(statePath); err != nil {
			return "", err
		}
		if p.mail, p.mailFile, err = openHeld(mailPath); err != nil {
			return "", err
		}
	}
	var tables int
	if e := p.state.QueryRow(pick(p.state.pg, "SELECT count(*) FROM sqlite_schema WHERE type='table' AND name IN ('backup_audit','meta','processed','decisions','notifications','native_devices','pull_notifications','deferrals','sorter_predictions','sorter_corrections')",
		"SELECT count(*) FROM pg_tables WHERE schemaname=current_schema() AND tablename IN ('backup_audit','meta','processed','decisions','notifications','native_devices','pull_notifications','deferrals','sorter_predictions','sorter_corrections')")).Scan(&tables); e != nil || tables != 10 {
		return "", ErrPreparation
	}
	var source string
	if err = p.state.QueryRow("SELECT value FROM meta WHERE key='mail_source'").Scan(&source); err != nil || source != pa.Source {
		return "", ErrPreparation
	}
	db := p.mail
	if err = db.QueryRow(pick(db.pg, "SELECT count(*) FROM sqlite_schema WHERE type='table' AND name IN ('identity','namespace','incoming_replacements','folders','labels','messages','receipts','changes','usage')",
		"SELECT count(*) FROM pg_tables WHERE schemaname=current_schema() AND tablename IN ('identity','namespace','incoming_replacements','folders','labels','messages','receipts','changes','usage')")).Scan(&tables); err != nil || tables != 9 {
		return "", ErrPreparation
	}
	var storedOwner Owner
	var storedLimits Limits
	var namespace string
	if err = db.QueryRow("SELECT issuer,subject,mailbox,message_bytes,payload_bytes,records FROM identity WHERE id=1").Scan(&storedOwner.Issuer, &storedOwner.Subject, &storedOwner.Mailbox, &storedLimits.MessageBytes, &storedLimits.PayloadBytes, &storedLimits.Records); err != nil || storedOwner != owner || storedLimits != limits {
		return "", ErrPreparation
	}
	if err = db.QueryRow("SELECT token FROM namespace WHERE id=1").Scan(&namespace); err != nil || namespace == "" {
		return "", ErrPreparation
	}
	if _, err = referenceGenerationOn(db, db.pg); err != nil {
		return "", ErrPreparation
	}
	client := &Client{store: &Store{owner: storedOwner, namespace: namespace}}
	if client.MailSourceIdentity() != source {
		return "", ErrPreparation
	}
	for _, f := range []struct {
		path string
		info os.FileInfo
	}{{statePath, p.stateFile}, {mailPath, p.mailFile}} {
		if p.state.pg {
			break // no files; the schemas are found by path on every open
		}
		if info, err = os.Lstat(f.path); err != nil || !os.SameFile(info, f.info) {
			return "", ErrPreparation
		}
	}
	return source, nil
}

func openHeld(path string) (*heldDB, os.FileInfo, error) {
	var info os.FileInfo
	var err error
	pg, err := dbbackend.Held(path)
	if err != nil {
		return nil, nil, err
	}
	if !pg {
		if info, err = os.Lstat(path); err != nil {
			return nil, nil, ErrPreparation
		}
	}
	db, err := openReadOnly(path)
	if err != nil {
		return nil, nil, err
	}
	return &heldDB{DB: db, stmts: map[string]*sql.Stmt{}, pg: pg}, info, nil
}

// openSQLite opens an existing regular database file, never creating one.
// A path the Postgres catalog holds opens its schema instead, read-only for mode=ro.
func openSQLite(path, query string) (*sql.DB, error) {
	if pg, err := dbbackend.Held(path); err != nil {
		return nil, err
	} else if pg {
		db, err := dbbackend.Open(path, dbbackend.Options{ReadOnly: strings.Contains(query, "mode=ro")})
		if err != nil {
			return nil, ErrPreparation
		}
		return db, nil
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, ErrPreparation
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs, RawQuery: query}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// Another process's open or close of the same idle database holds its lock
// briefly; wait as writers do instead of reporting the mailbox unprepared.
func openReadOnly(path string) (*sql.DB, error) {
	return openSQLite(path, "mode=ro&_pragma=busy_timeout(5000)")
}

// ReadUsage reads the usage counters of the mailbox database in dir without
// a writer, for admission and display.
func ReadUsage(dir string) (Usage, error) {
	db, err := openReadOnly(filepath.Join(dir, "mailbox.db"))
	if err != nil {
		return Usage{}, err
	}
	defer func() { _ = db.Close() }()
	var u Usage
	err = db.QueryRow("SELECT payload_bytes,records FROM usage WHERE id=1").Scan(&u.Bytes, &u.Records)
	return u, err
}

// convergeStep lets tests stop ConvergeLimits between its two writes, as a
// crash would.
var convergeStep = func() error { return nil }

// ConvergeLimits sets the durable limits of the published mailbox at dir to
// limits, which the caller's ledger already records: the mailbox.db identity
// first, native-mailbox.json last, so a file at the target proves both are.
// Any earlier value is replaced, so an interrupted run, or one for a quota
// changed since, completes on the next. A directory never published (no
// preparation file) has nothing to converge. Reports whether it wrote.
func ConvergeLimits(ctx context.Context, dir string, owner Owner, limits Limits) (bool, error) {
	if !limits.valid() || !fsutil.SafePathComponent(owner.Mailbox) || filepath.Base(dir) != owner.Mailbox {
		return false, ErrPreparation
	}
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	release, err := fsutil.LockFileContext(ctx, dir)
	if err != nil {
		return false, err
	}
	defer release()
	path := filepath.Join(dir, preparationFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8192 {
		return false, ErrPreparation
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var p preparedAccount
	if err = json.Unmarshal(raw, &p); err != nil || p.Owner != owner {
		return false, ErrPreparation
	}
	if p.Limits == limits {
		return false, nil
	}
	db, err := openSQLite(filepath.Join(dir, "mailbox", "mailbox.db"), "mode=rw&_txlock=immediate&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return false, err
	}
	defer func() { _ = db.Close() }()
	result, err := db.ExecContext(ctx, "UPDATE identity SET message_bytes=?,payload_bytes=?,records=? WHERE id=1 AND issuer=? AND subject=? AND mailbox=?", limits.MessageBytes, limits.PayloadBytes, limits.Records, owner.Issuer, owner.Subject, owner.Mailbox)
	if err != nil {
		return false, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return false, ErrPreparation
	}
	if err = convergeStep(); err != nil {
		return false, err
	}
	p.Limits = limits
	return true, fsutil.PersistJSONFile(path, p)
}
