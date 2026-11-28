package mailbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
)

// Message bodies in object storage, multi mode only
// (docs/superpowers/specs/2026-11-22-multi-host-replicas-design.md, "MH11 object storage,
// amended"). A body of ObjectThreshold bytes or more is written to the bucket before its
// row commits; the row keeps raw empty and a bodies row names the object.

// ObjectThreshold is the smallest body that goes to object storage (spec default).
const ObjectThreshold = 128 << 10

// BodyRetention keeps an expunged or unnamed object, so any capsule within the capsule
// retention restores with every body. Spec default until K open question 7 fixes it.
const BodyRetention = 30 * 24 * time.Hour

// BodyScope places a mailbox: the cell and org its objects live under.
type BodyScope struct{ Cell, Org string }

// objectBodies places each runtime store a multi start opens: K PR 7's multi wiring sets
// it, from the org's cell and doc_keys data key. Single mode never does, so its stores keep
// every body in raw. A mailbox it cannot place refuses to open.
var objectBodies func(Owner) (*vol.Object, BodyScope, error)

var errBody = errors.New("mailbox body failed its size or digest check")

// bodyCommitHook runs between writing an object and committing the row that names it
// (tests: a crash there).
var bodyCommitHook func() error

func (s *Store) placeBodies() error {
	if objectBodies == nil {
		return nil
	}
	objects, scope, err := objectBodies(s.owner)
	if err != nil {
		return err
	}
	if objects == nil {
		return errors.New("mailbox object storage is not configured")
	}
	s.objects, s.bodyPrefix = objects, vol.BodyPrefix(scope.Cell, scope.Org, s.owner.Mailbox)
	return nil
}

// pendingBody is an object written for a row that has not committed.
type pendingBody struct {
	object     string
	size       int64
	sum        [32]byte
	committing bool
}

// stash writes a large body to object storage, before its transaction so no lock waits on
// the upload, and returns what the row keeps in raw: the body itself, or empty.
func (s *Store) stash(ctx context.Context, raw []byte) ([]byte, *pendingBody, error) {
	if s.objects == nil || len(raw) < ObjectThreshold {
		return raw, nil, nil
	}
	id, err := vol.NewObjectID()
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.objects.Write(ctx, s.bodyPrefix+id, raw, 0); err != nil {
		return nil, nil, fmt.Errorf("mailbox body not stored: %w", err)
	}
	return []byte{}, &pendingBody{object: id, size: int64(len(raw)), sum: sha256.Sum256(raw)}, nil
}

// name records message id's object in tx.
func (p *pendingBody) name(ctx context.Context, tx *sql.Tx, id int64) error {
	if p == nil {
		return nil
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO bodies VALUES(?,?,?,?)", id, p.object, p.size, p.sum[:])
	return err
}

// commitBody commits a transaction that names p's object. From here the outcome may be
// unknown even on error, so the object is the orphan sweep's to judge, never deleted here.
func commitBody(tx *sql.Tx, p *pendingBody) error {
	if p != nil {
		p.committing = true
		if bodyCommitHook != nil {
			if err := bodyCommitHook(); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// release deletes an object whose row was never committed: a refusal, a duplicate or a
// replayed delivery. A crash before it leaves an orphan the sweep removes.
func (s *Store) release(ctx context.Context, p *pendingBody) {
	if p != nil && !p.committing {
		_ = s.objects.Remove(context.WithoutCancel(ctx), s.bodyPrefix+p.object, 1)
	}
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// body returns message id's bytes: raw, or the object its bodies row names, checked against
// the row's size and SHA-256. A missing or failing object is an error, never empty mail.
func (s *Store) body(ctx context.Context, q rowQuerier, id int64, raw []byte, limit int64) ([]byte, error) {
	if s.objects == nil || len(raw) > 0 {
		return raw, nil
	}
	var object string
	var size int64
	var sum []byte
	err := q.QueryRowContext(ctx, "SELECT object,size,sha256 FROM bodies WHERE message_id=?", id).Scan(&object, &size, &sum)
	if errors.Is(err, sql.ErrNoRows) {
		// Every stored message has bytes, so an empty raw without a bodies row is a moved
		// body expunged or replaced since raw was read: report it gone, never empty mail.
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if size > limit {
		return nil, mailmsg.ErrMessageTooLarge
	}
	b, _, err := s.objects.Read(ctx, s.bodyPrefix+object)
	if err != nil {
		return nil, fmt.Errorf("mailbox body unavailable: %w", err)
	}
	if h := sha256.Sum256(b); int64(len(b)) != size || !bytes.Equal(h[:], sum) {
		return nil, errBody
	}
	return b, nil
}

func (s *Store) epoch() string {
	return pick(s.postgres, "unixepoch()", "floor(extract(epoch from statement_timestamp()))::bigint")
}

// tombstone moves message id's object, if it has one, to body_tombstones in tx: an expunge
// never deletes the object, so a capsule taken before it still restores the body.
func (s *Store) tombstone(ctx context.Context, tx *sql.Tx, id int64) error {
	if s.objects == nil {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO body_tombstones SELECT object,"+s.epoch()+" FROM bodies WHERE message_id=?", id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM bodies WHERE message_id=?", id)
	return err
}

// SweepBodies deletes this mailbox's objects that a tombstone older than BodyRetention
// names, and those no row has named for BodyRetention since the sweep first saw them (a
// write whose row never committed, or a row a restore dropped). Clocks are the database's.
// The elected api-sweepers worker runs it.
func (s *Store) SweepBodies(ctx context.Context) (deleted int, err error) {
	if s.objects == nil {
		return 0, nil
	}
	keys, err := s.objects.List(ctx, s.bodyPrefix)
	if err != nil {
		return 0, err
	}
	var now int64
	if err := s.db.QueryRowContext(ctx, "SELECT "+s.epoch()).Scan(&now); err != nil {
		return 0, err
	}
	cutoff := now - int64(BodyRetention/time.Second)
	for _, key := range keys {
		object := strings.TrimPrefix(key, s.bodyPrefix)
		drop, err := s.judge(ctx, object, now, cutoff)
		if err != nil {
			return deleted, err
		}
		if !drop {
			continue
		}
		if err := s.objects.Remove(ctx, key, 1); err != nil {
			return deleted, err
		}
		if _, err := s.db.ExecContext(ctx, "DELETE FROM body_tombstones WHERE object=?", object); err != nil {
			return deleted, err
		}
		if _, err := s.db.ExecContext(ctx, "DELETE FROM body_orphans WHERE object=?", object); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

// judge reports whether object may be deleted, recording a first unnamed sighting.
func (s *Store) judge(ctx context.Context, object string, now, cutoff int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM bodies WHERE object=?", object).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 { // named again, after a restore: its clock starts only when next unnamed
		if _, err := tx.ExecContext(ctx, "DELETE FROM body_orphans WHERE object=?", object); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	var at int64
	err = tx.QueryRowContext(ctx, "SELECT at FROM body_tombstones WHERE object=?", object).Scan(&at)
	if err == nil {
		return at <= cutoff, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	err = tx.QueryRowContext(ctx, "SELECT seen_at FROM body_orphans WHERE object=?", object).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO body_orphans VALUES(?,?)", object, now); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	return at <= cutoff, tx.Commit()
}
