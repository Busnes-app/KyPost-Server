package cfreceiving

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	_ "modernc.org/sqlite"
)

const (
	// CredentialsFile (SECRET_DIR, sealed in backups) holds the current epoch.
	CredentialsFile = "cloudflare-receiving.json"
	// HostFile (SECRET_DIR, never backed up) marks this host as the live
	// consumer and holds an in-flight rotation. A restored copy lacks it, so
	// it starts fenced and can never replay rotation material the original
	// may already have installed.
	HostFile = "cloudflare-receiving.host.json"
	// DBFile (STATE_DIR/receiving, sealed) holds published revisions, the
	// provider ledger and the last status.
	DBFile = "cloudflare.db"
)

// ErrNoCredentials means `receiving cloudflare init` has not run.
var ErrNoCredentials = errors.New("no Cloudflare receiving credentials; run kypost-server receiving cloudflare init")

// Keys stores pickup credentials under SECRET_DIR, owner-only.
type Keys struct{ Dir string }

type hostRecord struct {
	Live    string    `json:"live"`
	Pending *Material `json:"pending,omitempty"`
}

// Lock serializes rotation, fencing and each daemon cycle across processes.
func (k Keys) Lock(ctx context.Context) (func(), error) {
	return fsutil.LockFileContext(ctx, filepath.Join(k.Dir, CredentialsFile))
}

func (k Keys) host() (hostRecord, error) {
	var h hostRecord
	raw, err := os.ReadFile(filepath.Join(k.Dir, HostFile))
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err == nil && (strict(raw, &h) != nil || h.Pending != nil && !h.Pending.valid()) {
		err = errors.New("cloudflare receiving host record is corrupt; preserve it and repair")
	}
	return h, err
}

func (k Keys) save(name string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(filepath.Join(k.Dir, name), raw, 0o600)
}

// Load returns the current material, any in-flight rotation and whether this
// host holds receiving.
func (k Keys) Load() (current Material, pending *Material, live bool, err error) {
	raw, err := os.ReadFile(filepath.Join(k.Dir, CredentialsFile))
	if errors.Is(err, os.ErrNotExist) {
		return current, nil, false, ErrNoCredentials
	}
	if err == nil && (strict(raw, &current) != nil || !current.valid()) {
		err = errors.New("cloudflare receiving credentials are corrupt; preserve them and repair")
	}
	if err != nil {
		return Material{}, nil, false, err
	}
	h, err := k.host()
	if err != nil {
		return Material{}, nil, false, err
	}
	// Pending is host-local and promoted only once installed, so pending
	// equal to current is a promotion interrupted between its two writes.
	if h.Pending != nil && h.Pending.Token == current.Token {
		return current, nil, true, nil
	}
	return current, h.Pending, h.Live == current.TokenSHA256(), nil
}

// Init creates epoch 1 on a host with no credentials. The caller holds Lock.
func (k Keys) Init() (Material, error) {
	if _, err := os.Lstat(filepath.Join(k.Dir, CredentialsFile)); !errors.Is(err, os.ErrNotExist) {
		return Material{}, errors.New("cloudflare receiving credentials already exist; rotate them instead")
	}
	m, err := NewMaterial(1)
	if err == nil {
		err = k.save(CredentialsFile, m)
	}
	if err == nil {
		err = k.save(HostFile, hostRecord{Live: m.TokenSHA256()})
	}
	return m, err
}

// SavePending persists rotation material before it is sent, so a retry
// sends the same material rather than inventing a third.
func (k Keys) SavePending(p Material) error {
	h, err := k.host()
	if err != nil {
		return err
	}
	h.Pending = &p
	return k.save(HostFile, h)
}

// Promote makes installed rotation material current and this host live.
// A crash between the writes leaves pending equal to current, which Load
// reports as live.
func (k Keys) Promote(p Material) error {
	if err := k.save(CredentialsFile, p); err != nil {
		return err
	}
	return k.save(HostFile, hostRecord{Live: p.TokenSHA256()})
}

// Fence records that this host no longer holds receiving. Callers fence only
// after the pending bearer was refused too, so pending material is dropped.
func (k Keys) Fence() error {
	return k.save(HostFile, hostRecord{})
}

// Status is the profile's operator view: counts and times, never addresses,
// envelopes or credentials. Times are Unix milliseconds; zero is never.
type Status struct {
	State            string         `json:"state"`
	Detail           string         `json:"detail,omitempty"`
	Epoch            int64          `json:"epoch,omitempty"`
	LastRevision     int64          `json:"lastRevision"`
	LastPublishAt    int64          `json:"lastPublishAt"`
	LastPickupAt     int64          `json:"lastPickupAt"`
	OldestUnpickedAt int64          `json:"oldestUnpickedAt"`
	OldestWarning    bool           `json:"oldestUnpickedWarning"`
	Waiting          int            `json:"waiting"`
	Ledger           map[string]int `json:"ledger"`
	UpdatedAt        int64          `json:"updatedAt"`
}

const schema = `
CREATE TABLE IF NOT EXISTS tables (digest TEXT PRIMARY KEY, routes TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS revisions (revision INTEGER PRIMARY KEY, issued_at INTEGER NOT NULL, digest TEXT NOT NULL REFERENCES tables(digest), installed_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS ledger (key TEXT PRIMARY KEY, digest TEXT NOT NULL, state TEXT NOT NULL CHECK(state IN ('junk','imported','quarantined','refused')), updated INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS status (id INTEGER PRIMARY KEY CHECK(id=1), doc TEXT NOT NULL);`

// DB is the local pickup state beside ingress.db.
type DB struct{ db *sql.DB }

func dsn(dir string, readOnly bool) (string, error) {
	path, err := filepath.Abs(filepath.Join(dir, DBFile))
	if err != nil {
		return "", err
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q["_pragma"] = []string{"synchronous(FULL)", "foreign_keys(ON)", "busy_timeout(5000)"}
	if readOnly {
		q.Set("mode", "ro")
	} else {
		q.Set("_txlock", "immediate")
		q["_pragma"] = append(q["_pragma"], "journal_mode(WAL)")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Open opens or creates the state in an existing owner-only receiving
// directory. A missing file after restore is safe: unknown revisions
// quarantine and the ingress store, not the ledger, decides provider deletes.
func Open(dir string) (*DB, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("cloudflare receiving state requires the owner-only receiving directory")
	}
	source, err := dsn(dir, false)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", source)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &DB{db: db}, nil
}

func (d *DB) Close() error { return d.db.Close() }

// Digest identifies a table's content for change detection.
func Digest(routes []Route, blocks []Block) string {
	raw, _ := json.Marshal(struct {
		Routes []Route
		Blocks []Block
	}{routes, blocks})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// LastRevision is the highest revision ever recorded, installed or not.
func (d *DB) LastRevision(ctx context.Context) (int64, error) {
	var rev int64
	err := d.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(revision),0) FROM revisions").Scan(&rev)
	return rev, err
}

// Record stores revision → routes before the table is sent, so captures
// under it resolve after a crash. Revisions strictly increase.
func (d *DB) Record(ctx context.Context, revision, issuedAt int64, digest string, routes []Route) error {
	raw, err := json.Marshal(routes)
	if err != nil {
		return err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var last int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(revision),0) FROM revisions").Scan(&last); err != nil {
		return err
	}
	if revision <= last {
		return errors.New("routing revision must increase")
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO tables VALUES(?,?)", digest, string(raw)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO revisions(revision,issued_at,digest) VALUES(?,?,?)", revision, issuedAt, digest); err != nil {
		return err
	}
	return tx.Commit()
}

// Installed marks a revision the Worker accepted.
func (d *DB) Installed(ctx context.Context, revision, at int64) error {
	_, err := d.db.ExecContext(ctx, "UPDATE revisions SET installed_at=? WHERE revision=?", at, revision)
	return err
}

// LastInstalled is the newest installed revision, its digest and time.
func (d *DB) LastInstalled(ctx context.Context) (revision int64, digest string, at int64, err error) {
	err = d.db.QueryRowContext(ctx, "SELECT revision,digest,installed_at FROM revisions WHERE installed_at>0 ORDER BY revision DESC LIMIT 1").Scan(&revision, &digest, &at)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return revision, digest, at, err
}

// Routes is a recorded revision's table by address; false when unknown.
func (d *DB) Routes(ctx context.Context, revision int64) (map[string]Route, bool, error) {
	var raw string
	err := d.db.QueryRowContext(ctx, "SELECT t.routes FROM revisions r JOIN tables t ON t.digest=r.digest WHERE r.revision=?", revision).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var routes []Route
	if err := json.Unmarshal([]byte(raw), &routes); err != nil {
		return nil, false, err
	}
	byAddress := make(map[string]Route, len(routes))
	for _, r := range routes {
		byAddress[r.Address] = r
	}
	return byAddress, true, nil
}

// Entry is one provider ledger row: junk (scanner rejected; Junk delivery
// owed), imported or quarantined (provider delete owed), refused (the
// provider copy cannot be held locally and is kept for the operator).
type Entry struct{ Key, Digest, State string }

func (d *DB) Entry(ctx context.Context, key string) (Entry, bool, error) {
	e := Entry{Key: key}
	err := d.db.QueryRowContext(ctx, "SELECT digest,state FROM ledger WHERE key=?", key).Scan(&e.Digest, &e.State)
	if errors.Is(err, sql.ErrNoRows) {
		return e, false, nil
	}
	return e, err == nil, err
}

func (d *DB) SetEntry(ctx context.Context, key, digest, state string) error {
	_, err := d.db.ExecContext(ctx, "INSERT INTO ledger VALUES(?,?,?,?) ON CONFLICT(key) DO UPDATE SET digest=excluded.digest,state=excluded.state,updated=excluded.updated", key, digest, state, time.Now().UnixMilli())
	return err
}

func (d *DB) RemoveEntry(ctx context.Context, key string) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM ledger WHERE key=?", key)
	return err
}

// Entries lists rows in the given states, oldest key first.
func (d *DB) Entries(ctx context.Context, states ...string) ([]Entry, error) {
	var out []Entry
	for _, state := range states {
		rows, err := d.db.QueryContext(ctx, "SELECT key,digest FROM ledger WHERE state=? ORDER BY key", state)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			e := Entry{State: state}
			if err := rows.Scan(&e.Key, &e.Digest); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d *DB) SaveStatus(ctx context.Context, s Status) error {
	s.Ledger = nil
	raw, err := json.Marshal(s)
	if err == nil {
		_, err = d.db.ExecContext(ctx, "INSERT INTO status VALUES(1,?) ON CONFLICT(id) DO UPDATE SET doc=excluded.doc", string(raw))
	}
	return err
}

// ReadStatus reads the last saved status and current ledger counts without
// creating anything; a profile that never ran reports "not-started".
func ReadStatus(ctx context.Context, dir string) (Status, error) {
	s := Status{State: "not-started", Ledger: map[string]int{}}
	if _, err := os.Lstat(filepath.Join(dir, DBFile)); errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	source, err := dsn(dir, true)
	if err != nil {
		return s, err
	}
	db, err := sql.Open("sqlite", source)
	if err != nil {
		return s, err
	}
	defer db.Close()
	var doc string
	err = db.QueryRowContext(ctx, "SELECT doc FROM status WHERE id=1").Scan(&doc)
	if err == nil {
		err = json.Unmarshal([]byte(doc), &s)
	} else if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return s, err
	}
	s.Ledger = map[string]int{}
	rows, err := db.QueryContext(ctx, "SELECT state,COUNT(*) FROM ledger GROUP BY state")
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return s, err
		}
		s.Ledger[state] = n
	}
	return s, rows.Err()
}

// CurrentStatus is ReadStatus with this host's credential state, which a
// stopped daemon cannot have saved: no host record means fenced.
func CurrentStatus(ctx context.Context, keys Keys, receivingDir string) (Status, error) {
	s, err := ReadStatus(ctx, receivingDir)
	if err != nil {
		return s, err
	}
	if _, _, live, err := keys.Load(); errors.Is(err, ErrNoCredentials) {
		s.State, s.Detail = "uninitialized", ErrNoCredentials.Error()
	} else if err == nil && !live {
		s.State, s.Detail = "fenced", "this instance does not hold Cloudflare receiving; confirm a takeover to move receiving here"
	}
	return s, nil
}
