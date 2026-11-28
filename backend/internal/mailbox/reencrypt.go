package mailbox

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"runtime"
	"sort"
	"strings"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

var _ imapadapter.ArchiveReencryptClient = (*Client)(nil)

// ListAllFolders returns all folders currently present in the native mailbox
// store (plus standard defaults) in deterministic order.
func (c *Client) ListAllFolders(ctx context.Context) ([]string, error) {
	defer runtime.KeepAlive(c)
	if err := c.checkAccess(ctx); err != nil {
		return nil, err
	}
	folders, err := c.store.Folders(ctx)
	if err != nil {
		return nil, err
	}
	sort.Strings(folders)
	return folders, nil
}

// ListFolderMessageUIDs returns up to limit live message IDs in folder strictly
// greater than afterUID in ascending order.
func (c *Client) ListFolderMessageUIDs(ctx context.Context, folder string, afterUID int, limit int) ([]int, error) {
	defer runtime.KeepAlive(c)
	if err := c.checkAccess(ctx); err != nil {
		return nil, err
	}
	if !validFolder(folder) {
		return nil, errors.New("invalid folder")
	}
	if limit <= 0 {
		return nil, nil
	}
	rows, err := c.store.db.QueryContext(
		ctx,
		"SELECT id FROM messages WHERE folder=? AND raw IS NOT NULL AND id>? ORDER BY id ASC LIMIT ?",
		folder, int64(afterUID), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id > 0 && int64(int(id)) == id {
			out = append(out, int(id))
		}
	}
	return out, rows.Err()
}

// GetRawMessage returns the exact stored RFC 5322 bytes for (folder, uid).
func (c *Client) GetRawMessage(ctx context.Context, folder string, uid int) ([]byte, error) {
	defer runtime.KeepAlive(c)
	if err := c.checkAccess(ctx); err != nil {
		return nil, err
	}
	if !validFolder(folder) || uid <= 0 {
		return nil, imapadapter.ErrMessageNotFound
	}
	raw, err := c.store.Raw(ctx, folder, int64(uid))
	if errors.Is(err, ErrNotFound) {
		return nil, imapadapter.ErrMessageNotFound
	}
	return raw, err
}

// ReplaceRawMessageCAS atomically replaces the stored raw ciphertext of
// (folder, uid) in place when its current SHA-256 equals expectedSHA256,
// preserving message ID, folder, flags, labels, received date, and receipts.
func (c *Client) ReplaceRawMessageCAS(ctx context.Context, folder string, uid int, expectedSHA256 string, newRaw []byte) (int, error) {
	defer runtime.KeepAlive(c)
	if err := c.checkAccess(ctx); err != nil {
		return 0, err
	}
	if !validFolder(folder) || uid <= 0 {
		return 0, imapadapter.ErrMessageNotFound
	}
	if len(newRaw) == 0 {
		return 0, errors.New("empty replacement message")
	}
	if int64(len(newRaw)) > c.store.limits.MessageBytes {
		return 0, mailmsg.ErrMessageTooLarge
	}

	tx, err := c.store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var oldRaw []byte
	var oldSize int64
	err = tx.QueryRowContext(
		ctx,
		"SELECT CASE WHEN length(raw)<=? THEN raw ELSE NULL END, length(raw) FROM messages WHERE folder=? AND id=? AND raw IS NOT NULL",
		c.store.limits.MessageBytes, folder, int64(uid),
	).Scan(&oldRaw, &oldSize)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, imapadapter.ErrMessageNotFound
	}
	if err != nil {
		return 0, err
	}
	if oldSize > c.store.limits.MessageBytes {
		return 0, mailmsg.ErrMessageTooLarge
	}
	if oldRaw, err = c.store.body(ctx, tx, int64(uid), oldRaw, c.store.limits.MessageBytes); err != nil {
		return 0, err
	}
	oldSize = int64(len(oldRaw))

	oldSum := sha256.Sum256(oldRaw)
	if !strings.EqualFold(hex.EncodeToString(oldSum[:]), strings.TrimSpace(expectedSHA256)) {
		return 0, imapadapter.ErrCASMismatch
	}

	delta := int64(len(newRaw)) - oldSize
	if delta > 0 {
		var records, used int64
		if err := tx.QueryRowContext(ctx, "SELECT records,payload_bytes FROM usage WHERE id=1").Scan(&records, &used); err != nil {
			return 0, err
		}
		if delta > c.store.limits.PayloadBytes-used {
			return 0, ErrCapacity
		}
		if err := c.store.checkReserve(delta, false); err != nil {
			return 0, err
		}
		if err := c.store.checkPool(ctx, delta, false); err != nil {
			return 0, err
		}
	}

	// The subject column follows the new bytes: sealing a plaintext message must not
	// leave its subject readable beside the ciphertext.
	headers, _, err := rawMetadata(newRaw)
	if err != nil {
		return 0, err
	}
	newSum := sha256.Sum256(newRaw)
	newDigest := hex.EncodeToString(newSum[:])
	// A moved body gets a new object; the old one is tombstoned, never overwritten.
	stored, pending, err := c.store.stash(ctx, newRaw)
	if err != nil {
		return 0, err
	}
	defer c.store.release(ctx, pending)
	if err := c.store.tombstone(ctx, tx, int64(uid)); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(
		ctx,
		"UPDATE messages SET raw=?, digest=?, subject=? WHERE folder=? AND id=? AND raw IS NOT NULL",
		stored, newDigest, headers.Get("Subject"), folder, int64(uid),
	)
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if affected != 1 {
		return 0, imapadapter.ErrMessageNotFound
	}
	if err := pending.name(ctx, tx, int64(uid)); err != nil {
		return 0, err
	}
	if err := change(ctx, tx, int64(uid), folder, false); err != nil {
		return 0, err
	}
	if err := commitBody(tx, pending); err != nil {
		return 0, err
	}
	return uid, nil
}

// Ref names one live message.
type Ref struct {
	Folder string
	ID     int64
}

// MessagesAfter lists up to limit live messages with an ID above after, in ID order,
// across every folder: a cursor over it survives moves, which keep the ID.
func (c *Client) MessagesAfter(ctx context.Context, after int64, limit int) ([]Ref, error) {
	defer runtime.KeepAlive(c)
	if err := c.checkAccess(ctx); err != nil {
		return nil, err
	}
	rows, err := c.store.db.QueryContext(ctx, "SELECT folder,id FROM messages WHERE raw IS NOT NULL AND id>? ORDER BY id LIMIT ?", after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ref
	for rows.Next() {
		var r Ref
		if err := rows.Scan(&r.Folder, &r.ID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Scrub folds the WAL into mailbox.db and truncates it, so with secure_delete the
// plaintext a replacement freed is zeroed in the file and gone from the WAL. Best effort:
// a busy reader leaves it for the next scrub. Postgres has no equivalent.
func (c *Client) Scrub(ctx context.Context) {
	if !c.store.postgres {
		_, _ = c.store.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	}
}
