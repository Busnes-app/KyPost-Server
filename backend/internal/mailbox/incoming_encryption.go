package mailbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

func hashIncoming(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (c *Client) incomingAccount() string {
	encoded, _ := json.Marshal([]any{"native-mailbox", c.store.owner, c.store.namespace})
	return hashIncoming(encoded)
}
func (c *Client) checkIncomingSource(source imapadapter.IncomingSource) error {
	hash, err := hex.DecodeString(source.Hash)
	if source.UID <= 0 || source.UIDValidity != 1 || source.AccountHash != c.incomingAccount() || err != nil || len(hash) != sha256.Size {
		return errors.New("native mailbox identity or source changed; preserve encryption journal and reconcile mailbox")
	}
	return nil
}
func (c *Client) checkIncomingReplacement(source imapadapter.IncomingSource, marker string, encrypted []byte) error {
	if err := c.checkIncomingSource(source); err != nil {
		return err
	}
	token, err := hex.DecodeString(marker)
	if err != nil || len(token) != sha256.Size || !bytes.HasPrefix(encrypted, []byte("X-KyPost-Incoming: "+marker+"\r\n")) {
		return errors.New("invalid native incoming replacement marker; original preserved")
	}
	if int64(len(encrypted)) > c.store.limits.MessageBytes {
		return mailmsg.ErrMessageTooLarge
	}
	return nil
}

// PrepareIncoming takes a single snapshot of raw bytes and metadata. The native
// namespace persists across restart but differs even for a recreated same-owner DB.
func (c *Client) PrepareIncoming(ctx context.Context, uid int, _ bool) (imapadapter.IncomingSource, error) {
	defer runtime.KeepAlive(c)
	if err := c.checkAccess(ctx); err != nil {
		return imapadapter.IncomingSource{}, err
	}
	if uid <= 0 {
		return imapadapter.IncomingSource{}, errors.New("invalid incoming UID")
	}
	tx, err := c.store.db.BeginTx(ctx, nil)
	if err != nil {
		return imapadapter.IncomingSource{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var raw []byte
	var size int64
	var seen, starred, draft bool
	var encoded, at string
	err = tx.QueryRowContext(ctx, `SELECT CASE WHEN length(raw)<=? THEN raw ELSE NULL END,length(raw),seen,starred,draft,labels,at_utc FROM messages WHERE id=? AND folder='INBOX' AND raw IS NOT NULL`, c.store.limits.MessageBytes, uid).Scan(&raw, &size, &seen, &starred, &draft, &encoded, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return imapadapter.IncomingSource{}, ErrNotFound
	}
	if err != nil {
		return imapadapter.IncomingSource{}, err
	}
	if size > c.store.limits.MessageBytes {
		return imapadapter.IncomingSource{}, mailmsg.ErrMessageTooLarge
	}
	date, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return imapadapter.IncomingSource{}, errors.New("native source date invalid; original preserved")
	}
	flags := []string{}
	if err = json.Unmarshal([]byte(encoded), &flags); err != nil {
		return imapadapter.IncomingSource{}, err
	}
	if seen {
		flags = append(flags, `\Seen`)
	}
	if starred {
		flags = append(flags, `\Flagged`)
	}
	if draft {
		flags = append(flags, `\Draft`)
	}
	source := imapadapter.IncomingSource{UID: uid, UIDValidity: 1, AccountHash: c.incomingAccount(), Hash: hashIncoming(raw), Raw: raw, Flags: flags, Date: date}
	return source, tx.Commit()
}

// ReplaceIncoming commits replacement, original tombstone and receipt together.
// A lost commit acknowledgement is recovered by the receipt plus exact bytes,
// never a sender-controlled header. Original transport receipts retain their ID.
func (c *Client) ReplaceIncoming(ctx context.Context, source imapadapter.IncomingSource, marker string, encrypted []byte) (int, error) {
	defer runtime.KeepAlive(c)
	if err := c.checkAccess(ctx); err != nil {
		return 0, err
	}
	if err := c.checkIncomingReplacement(source, marker, encrypted); err != nil {
		return 0, err
	}
	headers, _, err := rawMetadata(encrypted)
	if err != nil {
		return 0, err
	}
	parsed, err := imapadapter.ParseRawContent(encrypted)
	if err != nil || !parsed.Content.PGPEncrypted {
		return 0, errors.New("incoming replacement is not readable PGP/MIME ciphertext; original preserved")
	}
	tx, err := c.store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var oldMarker, oldHash, cipherHash string
	var id int
	err = tx.QueryRowContext(ctx, "SELECT marker,source_hash,ciphertext_hash,replacement_id FROM incoming_replacements WHERE source_id=?", source.UID).Scan(&oldMarker, &oldHash, &cipherHash, &id)
	if err == nil {
		if oldMarker != marker || oldHash != source.Hash || cipherHash != hashIncoming(encrypted) {
			return 0, ErrConflict
		}
		if _, err = c.verifyIncomingTx(ctx, tx, source, id, marker, encrypted); err != nil {
			return 0, err
		}
		return id, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var raw []byte
	var size int64
	err = tx.QueryRowContext(ctx, "SELECT CASE WHEN length(raw)<=? THEN raw ELSE NULL END,length(raw) FROM messages WHERE id=? AND folder='INBOX' AND raw IS NOT NULL", c.store.limits.MessageBytes, source.UID).Scan(&raw, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if size > c.store.limits.MessageBytes {
		return 0, mailmsg.ErrMessageTooLarge
	}
	if hashIncoming(raw) != source.Hash {
		return 0, errors.New("native original changed; encryption paused without deleting mail")
	}
	var records, used int64
	if err = tx.QueryRowContext(ctx, "SELECT records,payload_bytes FROM usage WHERE id=1").Scan(&records, &used); err != nil {
		return 0, err
	}
	if records >= int64(c.store.limits.Records) || int64(len(encrypted))-size > c.store.limits.PayloadBytes-used {
		return 0, ErrCapacity
	}
	// Copy current flags/date, not the stale prepare snapshot. SQL retains labels
	// verbatim; native metadata has no plaintext body index to copy.
	result, err := tx.ExecContext(ctx, `INSERT INTO messages(folder,raw,digest,sender,subject,sent_to,cc,bcc,at_utc,seen,starred,draft,labels) SELECT 'INBOX',?,?,?,?,?,?,?,at_utc,seen,starred,draft,labels FROM messages WHERE id=?`, encrypted, hashIncoming(encrypted), headers.Get("From"), headers.Get("Subject"), headers.Get("To"), headers.Get("Cc"), headers.Get("Bcc"), source.UID)
	if err != nil {
		return 0, err
	}
	replacement, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	id = int(replacement)
	if id <= 0 || int64(id) != replacement {
		return 0, errors.New("native replacement ID exceeds client range; original preserved")
	}
	if _, err = tx.ExecContext(ctx, "UPDATE messages SET raw=NULL,sender='',subject='',sent_to='',cc='',bcc='',labels='[]' WHERE id=?", source.UID); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO incoming_replacements VALUES(?,?,?,?,?)", source.UID, marker, source.Hash, hashIncoming(encrypted), id); err != nil {
		return 0, err
	}
	if err = change(ctx, tx, int64(source.UID), "INBOX", true); err != nil {
		return 0, err
	}
	if err = change(ctx, tx, replacement, "INBOX", false); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (c *Client) verifyIncomingTx(ctx context.Context, tx *sql.Tx, source imapadapter.IncomingSource, uid int, marker string, encrypted []byte) (string, error) {
	var folder string
	var raw []byte
	var size int64
	err := tx.QueryRowContext(ctx, `SELECT m.folder,CASE WHEN length(m.raw)<=? THEN m.raw ELSE NULL END,length(m.raw) FROM incoming_replacements r JOIN messages m ON m.id=r.replacement_id WHERE r.source_id=? AND r.marker=? AND r.source_hash=? AND r.ciphertext_hash=? AND r.replacement_id=? AND m.raw IS NOT NULL`, c.store.limits.MessageBytes, source.UID, marker, source.Hash, hashIncoming(encrypted), uid).Scan(&folder, &raw, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("verified native encrypted copy absent; preserve journal for reconciliation")
	}
	if err != nil {
		return "", err
	}
	if size > c.store.limits.MessageBytes || !bytes.Equal(raw, encrypted) {
		return "", errors.New("native encrypted copy differs; action paused without modifying mail")
	}
	return folder, nil
}

// ApplyIncomingAction verifies and mutates in one writer transaction. Moves
// preserve IDs, so an uncertain action can check the exact destination on retry.
func (c *Client) ApplyIncomingAction(ctx context.Context, source imapadapter.IncomingSource, uid int, marker string, encrypted []byte, action, value string) error {
	defer runtime.KeepAlive(c)
	if err := c.checkAccess(ctx); err != nil {
		return err
	}
	if err := c.checkIncomingReplacement(source, marker, encrypted); err != nil {
		return err
	}
	if uid <= 0 || uid == source.UID {
		return errors.New("invalid native encrypted action UID")
	}
	target := ""
	switch action {
	case "keyword", "unkeyword":
		if err := imapadapter.ValidateKeyword(value); err != nil {
			return err
		}
	case "read", "stop":
	case "move":
		var err error
		target, err = normalizeFolder(value)
		if err != nil || strings.TrimSpace(value) == "" {
			return errors.New("invalid encrypted-mail destination")
		}
	case "archive":
		target = "Archive/" + strconv.Itoa(source.Date.UTC().Year())
	case "spam":
		target = "Junk"
	case "delete":
		target = "Trash"
	default:
		return errors.New("unsupported native incoming action")
	}
	tx, err := c.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	folder, err := c.verifyIncomingTx(ctx, tx, source, uid, marker, encrypted)
	if err != nil {
		return err
	}
	if action == "stop" {
		return tx.Commit()
	}
	if target != "" && folder == target {
		return tx.Commit()
	}
	if folder != "INBOX" {
		return errors.New("native encrypted copy moved outside expected folder; reconcile pending action")
	}
	switch action {
	case "keyword", "unkeyword":
		err = editFlagsTx(ctx, tx, "INBOX", int64(uid), nil, value, action == "keyword")
	case "read":
		seen := true
		err = editFlagsTx(ctx, tx, "INBOX", int64(uid), &seen, "", false)
	default:
		if !validFolder(target) {
			return errors.New("invalid encrypted-mail destination")
		}
		// Existing encrypted rule actions create a missing destination. This differs
		// from an interactive move, which requires an existing destination.
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO folders VALUES(?)", target); err != nil {
			return err
		}
		if target == "INBOX" {
			return tx.Commit()
		}
		if _, err = tx.ExecContext(ctx, "UPDATE messages SET folder=? WHERE id=?", target, uid); err != nil {
			return err
		}
		if err = change(ctx, tx, int64(uid), "INBOX", true); err != nil {
			return err
		}
		err = change(ctx, tx, int64(uid), target, false)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// MailSourceIdentity pins API/daemon state to this owner and database instance.
func (c *Client) MailSourceIdentity() string { return "native:" + c.incomingAccount() }
