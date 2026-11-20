package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Mail-import jobs shared by PostgreSQL replicas (import_jobs): one row per user, written by
// the replica that runs the job. An active job whose instance no longer holds its liveness
// lock is interrupted; cancel is a flag the runner reads each time it writes the row.

var (
	ErrImportRunning = errors.New("state: the user's import is running")
	ErrImportsFull   = errors.New("state: every import slot is taken")
)

// ImportJob is a user's job row.
type ImportJob struct {
	Instance string
	Data     []byte
	Active   bool // uploading or running, as last written
	Live     bool // its instance holds its liveness lock
	Cancel   bool
}

// ClaimImport records userID's new active job, run by instance, unless the user already has
// an active job on a live instance or maxActive active jobs run on live instances. The check
// and the write share one write transaction, which serialises the replicas.
func (s *Store) ClaimImport(ctx context.Context, userID, instance string, data []byte, maxActive int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := importBusy(ctx, tx, userID, maxActive); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO import_jobs (user_id, instance_id, data, active, cancel, updated_at) VALUES (?, ?, ?, 1, 0, ?)
ON CONFLICT (user_id) DO UPDATE SET instance_id = excluded.instance_id, data = excluded.data, active = 1, cancel = 0, updated_at = excluded.updated_at`,
		userID, instance, string(data), time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// ImportBusy reports what ClaimImport would refuse now, writing nothing.
func (s *Store) ImportBusy(ctx context.Context, userID string, maxActive int) error {
	return importBusy(ctx, s.db, userID, maxActive)
}

func importBusy(ctx context.Context, q rowQueryer, userID string, maxActive int) error {
	var own, all int
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN user_id = ? THEN 1 ELSE 0 END), 0), COUNT(*)
 FROM import_jobs WHERE active = 1 AND `+liveSQL("instance_id"), userID).Scan(&own, &all); err != nil {
		return err
	}
	switch {
	case own > 0:
		return ErrImportRunning
	case all >= maxActive:
		return ErrImportsFull
	}
	return nil
}

// PutImport writes the job's row if instance still runs it, and reports whether a cancel was
// asked for. ok is false when another job has replaced it.
func (s *Store) PutImport(ctx context.Context, userID, instance string, data []byte, active bool) (cancel, ok bool, err error) {
	var flag int64
	err = s.db.QueryRowContext(ctx, `UPDATE import_jobs SET data = ?, active = ?, updated_at = ? WHERE user_id = ? AND instance_id = ? RETURNING cancel`,
		string(data), boolInt(active), time.Now().UnixMilli(), userID, instance).Scan(&flag)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	return flag != 0, err == nil, err
}

// Import reads userID's job row.
func (s *Store) Import(ctx context.Context, userID string) (ImportJob, bool, error) {
	var j ImportJob
	var data string
	var active, cancel int64
	err := s.db.QueryRowContext(ctx, `SELECT instance_id, data, active, cancel, `+liveSQL("instance_id")+` FROM import_jobs WHERE user_id = ?`, userID).
		Scan(&j.Instance, &data, &active, &cancel, &j.Live)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportJob{}, false, nil
	}
	if err != nil {
		return ImportJob{}, false, err
	}
	j.Data, j.Active, j.Cancel = []byte(data), active != 0, cancel != 0
	return j, true, nil
}

// CancelImport asks the runner of userID's active job on instance to stop.
func (s *Store) CancelImport(ctx context.Context, userID, instance string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE import_jobs SET cancel = 1 WHERE user_id = ? AND instance_id = ? AND active = 1`, userID, instance)
	return oneRow(res, err)
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
