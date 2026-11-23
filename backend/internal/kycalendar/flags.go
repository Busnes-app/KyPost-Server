package kycalendar

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/Busnes-app/kypost-server/backend/internal/vol"
)

// maxFlags bounds one user's record; the oldest entries go first.
const maxFlags = 2000

// Flag is a message the user flagged in KyPost, under the opaque Ref KyCalendar's task carries
// (docs/KYCALENDAR.md, Tasks). Mailbox is the user's mailbox selector ("" the primary), Folder
// and Message the message reference the mail routes take.
type Flag struct {
	Ref     string `json:"ref"`
	Mailbox string `json:"mailbox,omitempty"`
	Folder  string `json:"folder"`
	Message string `json:"message"`
	Due     string `json:"due,omitempty"`
	// Cleared: unflagged, here or by KyCalendar; kept so reopening the task flags it again.
	Cleared bool `json:"cleared,omitempty"`
}

// Flags is one user's flagged messages, on disk under the user's state directory.
type Flags struct{ path string }

// OpenFlags names the record in a user's state directory.
func OpenFlags(userStateDir string) Flags {
	return Flags{path: filepath.Join(userStateDir, "kycalendar-flags.json")}
}

func (s Flags) load() ([]Flag, error) { return s.loadDocument(vol.OpenDocument(s.path)) }

func (s Flags) loadDocument(doc *vol.Document) ([]Flag, error) {
	raw, err := doc.Read()
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Flag
	return out, json.Unmarshal(raw, &out)
}

func (s Flags) edit(fn func([]Flag) []Flag) error {
	doc := vol.OpenDocument(s.path)
	if err := doc.Mkdir(0o700); err != nil {
		return err
	}
	return doc.Update(func() error {
		list, err := s.loadDocument(doc)
		if err != nil {
			return err
		}
		list = fn(list)
		if len(list) > maxFlags {
			list = list[len(list)-maxFlags:]
		}
		raw, err := json.Marshal(list)
		if err != nil {
			return err
		}
		return doc.Write(raw)
	})
}

// Put records f flagged (Ref and Cleared ignored) and returns its reference: the existing one when the message is
// already recorded, so flagging again names the same task.
func (s Flags) Put(f Flag) (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	f.Ref, f.Cleared = base64.RawURLEncoding.EncodeToString(buf[:]), false
	err := s.edit(func(list []Flag) []Flag {
		for i, g := range list {
			if g.Mailbox == f.Mailbox && g.Folder == f.Folder && g.Message == f.Message {
				f.Ref = g.Ref
				return append(append(list[:i:i], list[i+1:]...), f)
			}
		}
		return append(list, f)
	})
	return f.Ref, err
}

// Clear marks the message unflagged and returns its reference ("" when none).
func (s Flags) Clear(mailbox, folder, message string) (string, error) {
	ref := ""
	err := s.edit(func(list []Flag) []Flag {
		for i, g := range list {
			if g.Mailbox == mailbox && g.Folder == folder && g.Message == message {
				ref, list[i].Cleared = g.Ref, true
			}
		}
		return list
	})
	return ref, err
}

// SetCleared marks the record with ref cleared or flagged; false when there is none.
func (s Flags) SetCleared(ref string, cleared bool) (bool, error) {
	found := false
	err := s.edit(func(list []Flag) []Flag {
		for i := range list {
			if list[i].Ref == ref {
				found, list[i].Cleared = true, cleared
			}
		}
		return list
	})
	return found, err
}

// Get finds a record by reference. An unreadable record is an error, never "absent".
func (s Flags) Get(ref string) (Flag, bool, error) {
	list, err := s.load()
	if err != nil {
		return Flag{}, false, err
	}
	for _, g := range list {
		if g.Ref == ref {
			return g, true, nil
		}
	}
	return Flag{}, false, nil
}

// List is every record.
func (s Flags) List() ([]Flag, error) { return s.load() }
