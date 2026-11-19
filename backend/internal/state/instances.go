package state

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"
)

// Instance is one api or daemon process registered on PostgreSQL. It is live while the
// session advisory lock on its ID is held, which the database releases when the process's
// connection ends.
type Instance struct {
	ID       string
	BootID   string
	VolumeID string
	Role     string
	Started  int64 // unix seconds
}

// instanceRelock is how often a held instance lock is probed and, if lost, taken again.
var instanceRelock = 5 * time.Second

// RegisterInstance records in and holds its liveness lock on a dedicated connection until
// ctx ends or release is called. A lost connection is re-locked in the background: until
// then other processes read this instance as dead.
func (s *Store) RegisterInstance(ctx context.Context, in Instance) (release func(), err error) {
	conn, won, err := s.tryLock(ctx, spaceInstance, in.ID)
	if err != nil {
		return nil, err
	}
	if !won {
		return nil, errors.New("state: instance ID already live")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO replica_instances (instance_id, boot_id, volume_id, role, started_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (instance_id) DO UPDATE SET boot_id = excluded.boot_id, volume_id = excluded.volume_id, role = excluded.role, started_at = excluded.started_at`,
		in.ID, in.BootID, in.VolumeID, in.Role, in.Started); err != nil {
		discard(conn)
		return nil, err
	}
	held, stop := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { discard(conn) }()
		t := time.NewTicker(instanceRelock)
		defer t.Stop()
		for {
			select {
			case <-held.Done():
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			ping, cancel := context.WithTimeout(held, lockProbeTimeout)
			err := conn.PingContext(ping)
			cancel()
			if err == nil {
				continue
			}
			discard(conn)
			slog.Error("replica instance lock lost; taking it again", "instance", in.ID, "error", err.Error())
			for held.Err() == nil {
				c, won, err := s.tryLock(held, spaceInstance, in.ID)
				if err == nil && won {
					conn = c
					break
				}
				select {
				case <-held.Done():
				case <-time.After(instanceRelock):
				}
			}
		}
	}()
	return func() {
		stop()
		<-done
		_, _ = s.db.ExecContext(context.WithoutCancel(ctx), `DELETE FROM replica_instances WHERE instance_id = ?`, in.ID)
	}, nil
}

// LiveInstances lists registered instances whose liveness lock is held now. Rows of dead ones
// are deleted on the way.
func (s *Store) LiveInstances(ctx context.Context) ([]Instance, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.instance_id, i.boot_id, i.volume_id, i.role, i.started_at,
 EXISTS (SELECT 1 FROM pg_locks l WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 2
  AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND l.classid = 4 AND l.objid = hashtext(current_schema() || ':' || i.instance_id)::oid)
 FROM replica_instances i ORDER BY i.instance_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var live []Instance
	var dead []string
	for rows.Next() {
		var in Instance
		var ok bool
		if err := rows.Scan(&in.ID, &in.BootID, &in.VolumeID, &in.Role, &in.Started, &ok); err != nil {
			return nil, err
		}
		if ok {
			live = append(live, in)
		} else {
			dead = append(dead, in.ID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range dead {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM replica_instances WHERE instance_id = ? AND NOT EXISTS (SELECT 1 FROM pg_locks l WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 2
  AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND l.classid = 4 AND l.objid = hashtext(current_schema() || ':' || ?)::oid)`, id, id)
	}
	return live, nil
}

// InstanceLive reports whether id is registered and live.
func (s *Store) InstanceLive(ctx context.Context, id string) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks l WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 2
  AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND l.classid = 4 AND l.objid = hashtext(current_schema() || ':' || ?)::oid)`, id).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return ok, err
}
