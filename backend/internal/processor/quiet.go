package processor

import (
	"errors"
	"os"
	"time"
)

// quietRecheck bounds how long the poller passes over an unchanged native
// mailbox (owner decision, 2026-11-16): snoozed mail, send-as checks and other
// timed work run at most this late.
const quietRecheck = 10 * time.Minute

// mailboxStamp is mailbox.db's and its WAL's size and modification time. A
// commit changes one of them; one it misses (same size within a timestamp
// tick) waits for quietRecheck.
type mailboxStamp [4]int64

// quietMailbox is a mailbox whose last pass left nothing pending, stamped
// before that pass fetched.
type quietMailbox struct {
	db    string
	stamp mailboxStamp
	at    time.Time
}

func stampMailbox(db string) (mailboxStamp, bool) {
	var s mailboxStamp
	fi, err := os.Stat(db)
	if err != nil {
		return s, false
	}
	s[0], s[1] = fi.Size(), fi.ModTime().UnixNano()
	if fi, err := os.Stat(db + "-wal"); err == nil {
		s[2], s[3] = fi.Size(), fi.ModTime().UnixNano()
	} else if !errors.Is(err, os.ErrNotExist) {
		return s, false
	}
	return s, true
}

func quietKey(userID, mailboxID string) string { return userID + "\x00" + mailboxID }

// skipQuiet reports whether key's mailbox is unchanged since a pass that left
// nothing pending, less than quietRecheck ago.
func (p *Poller) skipQuiet(key string) bool {
	p.quietMu.Lock()
	q, ok := p.quiet[key]
	p.quietMu.Unlock()
	if !ok || time.Since(q.at) >= quietRecheck {
		return false
	}
	s, ok := stampMailbox(q.db)
	return ok && s == q.stamp
}

// setQuiet records key's pass: q.db empty forgets it.
func (p *Poller) setQuiet(key string, q quietMailbox) {
	p.quietMu.Lock()
	defer p.quietMu.Unlock()
	if q.db == "" {
		delete(p.quiet, key)
		return
	}
	if p.quiet == nil {
		p.quiet = map[string]quietMailbox{}
	}
	p.quiet[key] = q
}

// forgetQuiet makes the next tick a full pass.
func (p *Poller) forgetQuiet() {
	p.quietMu.Lock()
	p.quiet = nil
	p.quietMu.Unlock()
}
