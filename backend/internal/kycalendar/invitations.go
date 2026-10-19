package kycalendar

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// maxInvitations bounds one user's record; the oldest entries go first.
const maxInvitations = 2000

// Invitations is one user's record of iTIP REQUESTs that arrived with a DKIM
// signature aligned to their organizer. It is the provenance a REPLY sent
// through POST /api/calendar/itip/send needs: a user answers only invitations
// their mailbox actually received. The daemon writes it and the API reads it,
// so it lives on disk under the user's state directory.
type Invitations struct{ path string }

type invitation struct {
	UID       string `json:"uid"`
	Organizer string `json:"organizer"`
	At        int64  `json:"at"`
}

// OpenInvitations names the record in a user's state directory.
func OpenInvitations(userStateDir string) Invitations {
	return Invitations{path: filepath.Join(userStateDir, "kycalendar-invitations.json")}
}

func (s Invitations) load() ([]invitation, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []invitation
	return out, json.Unmarshal(raw, &out)
}

// Record notes an invitation with uid from organizer. Idempotent.
func (s Invitations) Record(uid, organizer string) error {
	organizer = strings.ToLower(organizer)
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	return fsutil.WithFileLock(s.path, func() error {
		list, err := s.load()
		if err != nil {
			return err
		}
		for _, inv := range list {
			if inv.UID == uid && inv.Organizer == organizer {
				return nil
			}
		}
		list = append(list, invitation{UID: uid, Organizer: organizer, At: time.Now().Unix()})
		if len(list) > maxInvitations {
			list = list[len(list)-maxInvitations:]
		}
		raw, err := json.Marshal(list)
		if err != nil {
			return err
		}
		return fsutil.AtomicWriteFile(s.path, raw, 0o600)
	})
}

// Has reports whether an invitation with uid from organizer was recorded.
// An unreadable record is an error, never a "no".
func (s Invitations) Has(uid, organizer string) (bool, error) {
	list, err := s.load()
	if err != nil {
		return false, err
	}
	organizer = strings.ToLower(organizer)
	for _, inv := range list {
		if inv.UID == uid && inv.Organizer == organizer {
			return true, nil
		}
	}
	return false, nil
}
