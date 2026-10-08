package mailbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"

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

// publish is the syscall boundary exercised by killed-process checks.
func prepareAccount(stateRoot string, owner Owner, address string, limits Limits, publish func(string, string) error) (string, error) {
	return prepareAccountContext(context.Background(), filepath.Join(stateRoot, "users"), owner, address, limits, publish)
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
		file, e := os.Open(path)
		if e != nil {
			return "", e
		}
		e = file.Sync()
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
	if err = publish(account, target); err != nil {
		return "", fmt.Errorf("%w: %v", ErrPreparation, err)
	}
	if err = fsutil.SyncDir(root); err != nil {
		return "", err
	}
	return source, nil
}

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
	for _, path := range []string{dir, filepath.Join(dir, "mailbox")} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return "", ErrPreparation
		}
	}
	var p preparedAccount
	path := filepath.Join(dir, preparationFile)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8192 {
		return "", ErrPreparation
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if err = json.Unmarshal(raw, &p); err != nil || p.Owner != owner || p.Address != address || p.Limits != limits {
		return "", ErrPreparation
	}
	db, err := openReadOnly(filepath.Join(dir, "state.db"))
	if err != nil {
		return "", err
	}
	var tables int
	if e := db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='table' AND name IN ('backup_audit','meta','processed','decisions','notifications','native_devices','pull_notifications','deferrals','sorter_predictions','sorter_corrections')").Scan(&tables); e != nil || tables != 10 {
		_ = db.Close()
		return "", ErrPreparation
	}
	var source string
	err = db.QueryRow("SELECT value FROM meta WHERE key='mail_source'").Scan(&source)
	_ = db.Close()
	if err != nil || source != p.Source {
		return "", ErrPreparation
	}
	db, err = openReadOnly(filepath.Join(dir, "mailbox", "mailbox.db"))
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	if err = db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='table' AND name IN ('identity','namespace','incoming_replacements','folders','labels','messages','receipts','changes','usage')").Scan(&tables); err != nil || tables != 9 {
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
	if _, err = messageReferenceGeneration(db); err != nil {
		return "", ErrPreparation
	}
	client := &Client{store: &Store{owner: storedOwner, namespace: namespace}}
	if client.MailSourceIdentity() != source {
		return "", ErrPreparation
	}
	// A successful retry also repairs a lost acknowledgement of the parent sync.
	if err = fsutil.SyncDir(filepath.Dir(dir)); err != nil {
		return "", err
	}
	return source, nil
}

// openSQLite opens an existing regular database file, never creating one.
func openSQLite(path, query string) (*sql.DB, error) {
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
