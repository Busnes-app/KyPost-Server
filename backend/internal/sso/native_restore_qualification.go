package sso

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
)

// NativeRestoreQualificationFile records that every offline restore stage
// completed. Backups never collect it; it binds to one hold epoch.
const NativeRestoreQualificationFile = "native-restore-qualification.json"

// NativeRestoreQualification maps every published native mailbox, primary and
// extra, to the reference generation the restore left it with. CreatedAt
// precedes the restore's token fence (release precondition P7).
type NativeRestoreQualification struct {
	Version   int               `json:"version"`
	Epoch     string            `json:"epoch"`
	CreatedAt time.Time         `json:"createdAt"`
	Mailboxes map[string]string `json:"mailboxes"`
}

// NativeRestoreUnqualifiedError lists every reason release must be refused.
type NativeRestoreUnqualifiedError struct{ Reasons []string }

func (e *NativeRestoreUnqualifiedError) Error() string {
	return "native restore is not qualified for release: " + strings.Join(e.Reasons, "; ")
}

var errQualificationMalformed = errors.New("malformed native restore qualification")

// RecordNativeRestoreQualification is the last QuarantineNativeRestore step, in
// stopped staging after every earlier stage succeeded. createdAt must be taken
// before the token fence, so every fenced RevokedBefore is at least createdAt+31.
func (s *LifecycleStore) RecordNativeRestoreQualification(stateRoot string, createdAt time.Time) error {
	epoch, err := nativeRecoveryEpoch(stateRoot)
	if err != nil {
		return err
	}
	all, err := s.NativeRestoreMailboxes(stateRoot)
	if err != nil {
		return err
	}
	q := NativeRestoreQualification{Version: 1, Epoch: epoch, CreatedAt: createdAt.UTC(), Mailboxes: map[string]string{}}
	for id, m := range all {
		if q.Mailboxes[id], _, err = mailbox.InspectRestored(filepath.Join(m.Dir, "mailbox/mailbox.db")); err != nil {
			return err
		}
	}
	return fsutil.PersistJSONFile(filepath.Join(stateRoot, NativeRestoreQualificationFile), q)
}

// CheckNativeRestoreQualification is read-only. It returns a
// *NativeRestoreUnqualifiedError naming every failed precondition.
func (s *LifecycleStore) CheckNativeRestoreQualification(stateRoot string) (NativeRestoreQualification, error) {
	var reasons []string
	epoch, err := nativeRecoveryEpoch(stateRoot)
	if err != nil {
		if _, statErr := os.Lstat(filepath.Join(stateRoot, NativeRestoreHoldFile)); errors.Is(statErr, os.ErrNotExist) {
			reasons = append(reasons, "no native restore hold")
		} else {
			reasons = append(reasons, "native restore hold has no usable epoch")
		}
	}
	q, err := readNativeRestoreQualification(filepath.Join(stateRoot, NativeRestoreQualificationFile))
	marker := err == nil
	switch {
	case errors.Is(err, os.ErrNotExist):
		reasons = append(reasons, "no restore qualification marker: restore again with this version to qualify for release")
	case err != nil:
		reasons = append(reasons, "restore qualification marker is malformed")
	case epoch != "" && q.Epoch != epoch:
		reasons = append(reasons, "restore qualification marker belongs to another restore epoch")
	}
	all, ledgerErr := s.NativeRestoreMailboxes(stateRoot)
	if ledgerErr != nil {
		reasons = append(reasons, "native mailbox ledger is unreadable")
	}
	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		generation, _, err := mailbox.InspectRestored(filepath.Join(all[id].Dir, "mailbox/mailbox.db"))
		recorded, listed := q.Mailboxes[id]
		switch {
		case err != nil:
			reasons = append(reasons, fmt.Sprintf("mailbox %s cannot be read", id))
		case generation == "":
			reasons = append(reasons, fmt.Sprintf("mailbox %s has no reference generation", id))
		case !marker:
		case !listed:
			reasons = append(reasons, fmt.Sprintf("mailbox %s is missing from the qualification marker", id))
		case recorded != generation:
			reasons = append(reasons, fmt.Sprintf("mailbox %s reference generation changed since the restore", id))
		}
	}
	if marker {
		for id := range q.Mailboxes {
			if _, ok := all[id]; !ok && ledgerErr == nil {
				reasons = append(reasons, fmt.Sprintf("qualification marker lists mailbox %s, which the ledger does not publish", id))
			}
		}
	}
	// Every mailbox database, published or not, must hold no sendable work.
	// Only the mailbox trees: backup scratch holds unquarantined copies.
	for _, tree := range []string{"users", nativeMailboxesDir} {
		root := filepath.Join(stateRoot, tree)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) && path == root {
				return nil
			}
			if err != nil {
				return err
			}
			if entry.IsDir() || entry.Name() != "mailbox.db" {
				return nil
			}
			rel, _ := filepath.Rel(stateRoot, path)
			if !entry.Type().IsRegular() {
				reasons = append(reasons, fmt.Sprintf("mailbox database %s is not a regular file", rel))
			} else if _, pending, err := mailbox.InspectRestored(path); err != nil {
				reasons = append(reasons, fmt.Sprintf("mailbox database %s cannot be read", rel))
			} else if pending != 0 {
				reasons = append(reasons, fmt.Sprintf("mailbox database %s has %d queued or retryable outbound deliveries", rel, pending))
			}
			return nil
		})
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("state/%s cannot be scanned", tree))
		}
	}
	if len(reasons) != 0 {
		return q, &NativeRestoreUnqualifiedError{Reasons: reasons}
	}
	return q, nil
}

// NativeRestoreMailbox is a published ledger mailbox in a current or restored state root.
type NativeRestoreMailbox struct {
	Dir     string // state/users/<id> or state/mailboxes/<id>
	Source  string
	Primary bool
}

// NativeRestoreMailboxes lists every published ledger mailbox, primary and
// extra, whether or not users.json publishes its owner. Restore fences and the
// qualification marker cover exactly this set.
func (s *LifecycleStore) NativeRestoreMailboxes(stateRoot string) (map[string]NativeRestoreMailbox, error) {
	f, _, err := s.loadNativeLedger(true)
	if err != nil {
		return nil, err
	}
	all := map[string]NativeRestoreMailbox{}
	for _, a := range f.Accounts {
		if a.Source != "" {
			all[a.Owner.Mailbox] = NativeRestoreMailbox{filepath.Join(stateRoot, "users", a.Owner.Mailbox), a.Source, true}
		}
	}
	for id, m := range f.stored.Mailboxes {
		if m.Kind == "extra" && m.Source != "" {
			all[id] = NativeRestoreMailbox{filepath.Join(stateRoot, nativeMailboxesDir, id), m.Source, false}
		}
	}
	for id := range all {
		if !fsutil.SafePathComponent(id) {
			return nil, ErrNativeProvisioning
		}
	}
	return all, nil
}

func readNativeRestoreQualification(path string) (NativeRestoreQualification, error) {
	var q NativeRestoreQualification
	info, err := os.Lstat(path)
	if err != nil {
		return q, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 1<<20 {
		return q, errQualificationMalformed
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return q, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if dec.Decode(&q) != nil || dec.Decode(&struct{}{}) != io.EOF || q.Version != 1 || !recoveryEpochPattern.MatchString(q.Epoch) || q.CreatedAt.IsZero() || q.Mailboxes == nil {
		return NativeRestoreQualification{}, errQualificationMalformed
	}
	for id := range q.Mailboxes {
		if !fsutil.SafePathComponent(id) {
			return NativeRestoreQualification{}, errQualificationMalformed
		}
	}
	return q, nil
}
