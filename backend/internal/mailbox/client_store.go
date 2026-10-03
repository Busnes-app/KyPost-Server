package mailbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"sort"
	"strings"
	"time"
)

const metadataColumns = "id,folder,sender,subject,sent_to,cc,bcc,at_utc,seen,starred,draft,labels"

func scanMessage(row interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var labels string
	err := row.Scan(&m.ID, &m.Folder, &m.Sender, &m.Subject, &m.To, &m.CC, &m.BCC, &m.AtUTC, &m.Seen, &m.Starred, &m.Draft, &labels)
	if err != nil {
		return m, err
	}
	err = json.Unmarshal([]byte(labels), &m.Labels)
	return m, err
}
func (s *Store) metadata(ctx context.Context, folder string, id int64) (Message, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx, "SELECT "+metadataColumns+" FROM messages WHERE folder=? AND id=? AND raw IS NOT NULL", folder, id))
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}
func (s *Store) unreadAfter(ctx context.Context, after int64, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+metadataColumns+" FROM messages WHERE folder='INBOX' AND seen=0 AND raw IS NOT NULL AND id>? ORDER BY id LIMIT ?", after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func ensureLabelTx(ctx context.Context, tx *sql.Tx, label string) error {
	var exists int
	err := tx.QueryRowContext(ctx, "SELECT count(*) FROM labels WHERE name=?", label).Scan(&exists)
	if err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM labels").Scan(&count); err != nil {
		return err
	}
	if count >= 1000 {
		return errors.New("mailbox label limit reached")
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO labels VALUES(?)", label)
	return err
}
func (s *Store) ensureLabel(ctx context.Context, label string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = ensureLabelTx(ctx, tx, label); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) labels(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, "SELECT name COLLATE NOCASE FROM labels UNION SELECT value COLLATE NOCASE FROM messages,json_each(messages.labels) WHERE messages.raw IS NOT NULL ORDER BY 1 LIMIT 1001")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	seen := map[string]bool{}
	count := 0
	for rows.Next() {
		count++
		if count > 1000 {
			return nil, errors.New("mailbox label catalog exceeds limit; reconcile stored labels")
		}
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		key := strings.ToLower(name)
		if !seen[key] {
			out = append(out, name)
			seen[key] = true
		}
	}
	return out, rows.Err()
}

// editFlags reads and edits within one immediate transaction across processes.
// The legacy label interface has no folder argument; native IDs are owner-wide.
func (s *Store) editFlags(ctx context.Context, folder string, id int64, seen *bool, label string, add bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = editFlagsTx(ctx, tx, folder, id, seen, label, add); err != nil {
		return err
	}
	return tx.Commit()
}
func editFlagsTx(ctx context.Context, tx *sql.Tx, folder string, id int64, seen *bool, label string, add bool) error {
	var currentFolder, encoded string
	var oldSeen bool
	err := tx.QueryRowContext(ctx, "SELECT folder,seen,labels FROM messages WHERE id=? AND (?='' OR folder=?) AND raw IS NOT NULL", id, folder, folder).Scan(&currentFolder, &oldSeen, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	nextSeen := oldSeen
	if seen != nil {
		nextSeen = *seen
	}
	labels := []string{}
	if err = json.Unmarshal([]byte(encoded), &labels); err != nil {
		return err
	}
	if label != "" {
		next := []string{}
		found := false
		for _, old := range labels {
			if strings.EqualFold(old, label) {
				found = true
				if add {
					next = append(next, old)
				}
			} else {
				next = append(next, old)
			}
		}
		if add && !found {
			if len(next) >= 100 {
				return errors.New("too many mailbox labels")
			}
			next = append(next, label)
		}
		labels = next
		if add {
			if err = ensureLabelTx(ctx, tx, label); err != nil {
				return err
			}
		}
	}
	sort.Strings(labels)
	data, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	if oldSeen == nextSeen && encoded == string(data) {
		return nil
	}
	if _, err = tx.ExecContext(ctx, "UPDATE messages SET seen=?,labels=? WHERE id=?", nextSeen, string(data), id); err != nil {
		return err
	}
	if err = change(ctx, tx, id, currentFolder, false); err != nil {
		return err
	}
	return nil
}
func (s *Store) createFolder(ctx context.Context, parent, target string) error {
	if !validFolder(target) {
		return errors.New("invalid folder")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if parent != "" {
		var exists int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM folders WHERE name=?", parent).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO folders VALUES(?)", target); err != nil {
		return err
	}
	return tx.Commit()
}

// Folder deletion matches the shipped API: move live children to the parent,
// then delete the empty leaf. The moves and delete share one transaction.
func (s *Store) deleteFolderToParent(ctx context.Context, folder string) error {
	i := strings.LastIndex(folder, "/")
	if i < 0 {
		return errors.New("folder must have a parent mailbox")
	}
	parent := folder[:i]
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var children int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM folders WHERE substr(name,1,length(?)+1)=?||'/'", folder, folder).Scan(&children); err != nil {
		return err
	}
	if children != 0 {
		return errors.New("folder has subfolders; remove them first")
	}
	var exists int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM folders WHERE name=?", parent).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrNotFound
	}
	var after int64
	for {
		rows, err := tx.QueryContext(ctx, "SELECT id FROM messages WHERE folder=? AND raw IS NOT NULL AND id>? ORDER BY id LIMIT 200", folder, after)
		if err != nil {
			return err
		}
		ids := []int64{}
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			if _, err = tx.ExecContext(ctx, "UPDATE messages SET folder=? WHERE id=?", parent, id); err != nil {
				return err
			}
			if err = change(ctx, tx, id, folder, true); err != nil {
				return err
			}
			if err = change(ctx, tx, id, parent, false); err != nil {
				return err
			}
			after = id
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE messages SET folder=? WHERE folder=? AND raw IS NULL", parent, folder); err != nil {
		return err
	}
	r, err := tx.ExecContext(ctx, "DELETE FROM folders WHERE name=?", folder)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// Size predicate is evaluated by SQLite before the BLOB crosses into Go.
func (s *Store) rawWithin(ctx context.Context, folder string, id, limit int64) ([]byte, error) {
	var raw []byte
	var size int64
	err := s.db.QueryRowContext(ctx, "SELECT CASE WHEN length(raw)<=? THEN raw ELSE NULL END,length(raw) FROM messages WHERE folder=? AND id=? AND raw IS NOT NULL", limit, folder, id).Scan(&raw, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if size > limit {
		return nil, mailmsg.ErrMessageTooLarge
	}
	return raw, nil
}
