package mailbox

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// The idle mark lets HasPendingOutbound skip opening mailbox.db. It records a
// fresh empty due-work query of one mailbox.db inode, valid until the earliest
// future retry and at most outboundIdleMax. Writers that can make work due
// hold the lock shared and remove the mark before their transaction; the probe
// writes a mark only while holding it exclusively. A crash releases the lock.
const (
	// OutboundIdleFile and its ".tmp" are per-process hints; backups skip them.
	OutboundIdleFile = "outbox-idle"
	outboundIdleMax  = 10 * time.Minute
)

type outboundIdle struct {
	Dev, Ino       uint64
	Written, Until int64
}

func lockOutboundIdle(dir string, how int) (release func(), err error) {
	f, err := os.OpenFile(filepath.Join(dir, OutboundIdleFile+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

// clearOutboundIdle runs before a transaction that can make outbound work due;
// the caller holds release until it commits or rolls back. The removal is
// synced so a power loss cannot bring an old mark back after the commit.
func (s *Store) clearOutboundIdle() (release func(), err error) {
	if release, err = lockOutboundIdle(s.dir, syscall.LOCK_SH); err != nil {
		return nil, err
	}
	err = os.Remove(filepath.Join(s.dir, OutboundIdleFile))
	if err == nil {
		err = fsutil.SyncDir(s.dir)
	} else if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func idleMarked(dir string, db os.FileInfo, now time.Time) bool {
	raw, err := os.ReadFile(filepath.Join(dir, OutboundIdleFile))
	var m outboundIdle
	if err != nil || json.Unmarshal(raw, &m) != nil {
		return false
	}
	st, ok := db.Sys().(*syscall.Stat_t)
	at := now.Unix()
	return ok && m.Dev == uint64(st.Dev) && m.Ino == st.Ino &&
		m.Written <= at && at < m.Until && m.Until-m.Written <= int64(outboundIdleMax/time.Second)
}

// markOutboundIdle records an empty result; failing to is only a lost skip.
func markOutboundIdle(dir string, db os.FileInfo, written time.Time, next int64) {
	st, ok := db.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	until := written.Add(outboundIdleMax).Unix()
	if next > 0 && next < until {
		until = next
	}
	raw, _ := json.Marshal(outboundIdle{uint64(st.Dev), st.Ino, written.Unix(), until})
	tmp := filepath.Join(dir, OutboundIdleFile+".tmp")
	if os.WriteFile(tmp, raw, 0o600) != nil || os.Rename(tmp, filepath.Join(dir, OutboundIdleFile)) != nil {
		_ = os.Remove(tmp)
	}
}
