package api

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/jhillyerd/enmime/v2"
	_ "modernc.org/sqlite"
)

// Test-only feasibility spike, never selected by a running server. The embedded
// fake supplies UNEXERCISED methods; this proves reads, not backend parity.
// ponytail: parse raw MIME on reads; production needs bounded parsing and an
// indexed metadata path before it can replace the IMAP adapter.
type nativeMailboxProof struct {
	fakeMailClient
	db    *sql.DB
	owner string
}

func (p *nativeMailboxProof) FetchRawMessage(ctx context.Context, mailbox string, uid int) ([]byte, error) {
	if mailbox == "" {
		mailbox = "INBOX"
	}
	var raw []byte
	err := p.db.QueryRowContext(ctx, "SELECT raw FROM messages WHERE owner=? AND mailbox=? AND uid=?", p.owner, mailbox, uid).Scan(&raw)
	return raw, err
}

func (p *nativeMailboxProof) GetMessageBodies(ctx context.Context, mailbox string, uids []int) (map[int]imapadapter.MessageContent, error) {
	result := make(map[int]imapadapter.MessageContent)
	for _, uid := range uids {
		raw, err := p.FetchRawMessage(ctx, mailbox, uid)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		result[uid] = imapadapter.MessageContent{Body: env.Text, BodyMode: "plain", HasAttachments: len(env.Attachments) > 0}
	}
	return result, nil
}

func (p *nativeMailboxProof) ListUnreadMessages(ctx context.Context, mailbox string, limit int) ([]imapadapter.UnreadMessage, error) {
	if mailbox == "" {
		mailbox = "INBOX"
	}
	rows, err := p.db.QueryContext(ctx, "SELECT uid, raw FROM messages WHERE owner=? AND mailbox=? ORDER BY uid DESC LIMIT ?", p.owner, mailbox, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []imapadapter.UnreadMessage
	for rows.Next() {
		var uid int
		var raw []byte
		if err := rows.Scan(&uid, &raw); err != nil {
			return nil, err
		}
		env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		result = append(result, imapadapter.UnreadMessage{MessageID: strconv.Itoa(uid), Subject: env.GetHeader("Subject"), Sender: env.GetHeader("From"), SentTo: env.GetHeader("To"), Body: env.Text, BodyMode: "plain", Status: "unread", HasAttachments: len(env.Attachments) > 0})
	}
	return result, rows.Err()
}

func (p *nativeMailboxProof) ListOverviews(ctx context.Context, mailbox string, limit int) ([]imapadapter.Overview, error) {
	messages, err := p.ListUnreadMessages(ctx, mailbox, limit)
	if err != nil {
		return nil, err
	}
	result := make([]imapadapter.Overview, len(messages))
	for i, msg := range messages {
		uid, err := strconv.Atoi(msg.MessageID)
		if err != nil {
			return nil, err
		}
		result[i] = imapadapter.Overview{UID: uid, MessageID: msg.MessageID, Subject: msg.Subject, Sender: msg.Sender, SentTo: msg.SentTo, Status: msg.Status}
	}
	return result, nil
}

func (p *nativeMailboxProof) ListAttachments(ctx context.Context, mailbox string, uid int) ([]imapadapter.AttachmentInfo, error) {
	raw, err := p.FetchRawMessage(ctx, mailbox, uid)
	if err != nil {
		return nil, err
	}
	env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	infos := make([]imapadapter.AttachmentInfo, len(env.Attachments))
	for i, part := range env.Attachments {
		infos[i] = imapadapter.AttachmentInfo{Index: i, Name: part.FileName, MimeType: part.ContentType, Size: len(part.Content)}
	}
	return infos, nil
}

func (p *nativeMailboxProof) GetAttachment(ctx context.Context, mailbox string, uid, index int) (imapadapter.AttachmentInfo, []byte, error) {
	raw, err := p.FetchRawMessage(ctx, mailbox, uid)
	if err != nil {
		return imapadapter.AttachmentInfo{}, nil, err
	}
	env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
	if err != nil {
		return imapadapter.AttachmentInfo{}, nil, err
	}
	if index < 0 || index >= len(env.Attachments) {
		return imapadapter.AttachmentInfo{}, nil, imapadapter.ErrAttachmentNotFound
	}
	part := env.Attachments[index]
	return imapadapter.AttachmentInfo{Index: index, Name: part.FileName, MimeType: part.ContentType, Size: len(part.Content)}, part.Content, nil
}

func TestNativeMailboxProof(t *testing.T) {
	t.Setenv("STATE_DIR", t.TempDir())
	t.Setenv("CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mail.db")
	open := func() *sql.DB {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		for _, stmt := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "CREATE TABLE IF NOT EXISTS messages (uid INTEGER PRIMARY KEY AUTOINCREMENT, owner TEXT NOT NULL, mailbox TEXT NOT NULL, raw BLOB NOT NULL)", "CREATE INDEX IF NOT EXISTS mailbox_uids ON messages(owner, mailbox, uid)"} {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
		}
		return db
	}
	db := open()
	t.Cleanup(func() { _ = db.Close() })
	srv, owner := mailBodyServer(t, &fakeMailClient{})
	attachment := bytes.Repeat([]byte("attachment-byte-proof\x00"), 50000)
	raw := mailmsg.Message{From: "sender@example.test", To: []string{"owner@example.test"}, Subject: "Native proof", Body: "durable local body", Attachments: []mailmsg.Attachment{{Name: "proof.bin", MimeType: "application/octet-stream", Content: attachment}}}.Build()
	insert := func(owner, mailbox string, raw []byte) int {
		result, err := db.Exec("INSERT INTO messages(owner, mailbox, raw) VALUES (?, ?, ?)", owner, mailbox, raw)
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return int(id)
	}
	uid := insert(owner, "INBOX", raw)
	foreignUID := insert("another-owner", "INBOX", raw)
	otherFolderUID := insert(owner, "Private", raw)
	// A rolled-back delivery is not an accepted/visible delivery.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO messages(owner, mailbox, raw) VALUES (?, 'INBOX', ?)", owner, raw); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = open()
	proof := &nativeMailboxProof{db: db, owner: owner}
	stored, err := proof.FetchRawMessage(ctx, "INBOX", uid)
	if err != nil || !bytes.Equal(stored, raw) {
		t.Fatalf("reopened MIME changed: %v", err)
	}
	listed, err := proof.ListUnreadMessages(ctx, "INBOX", 100)
	if err != nil || len(listed) != 1 || listed[0].MessageID != strconv.Itoa(uid) {
		t.Fatalf("rollback/scope/stable ID: %+v, %v", listed, err)
	}
	srv.userMu.Lock()
	srv.userMail[owner] = &serverMailEntry{client: proof, updatedAt: "test"}
	srv.userMu.Unlock()
	device, secret := pairNativeDevice(t, srv, owner, "native-storage-proof")
	routes := srv.routes()
	request := func(path string, native, authenticated bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if authenticated {
			if native {
				setDeviceHeaders(req, device, secret)
			} else {
				authRequestAs(srv, req, owner)
			}
		}
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		return rec
	}
	query := "?mailbox=INBOX&messageId=" + strconv.Itoa(uid)
	for _, native := range []bool{false, true} {
		body := request("/api/mail/body"+query, native, true)
		if body.Code != http.StatusOK || !bytes.Contains(body.Body.Bytes(), []byte("durable local body")) || !bytes.Contains(body.Body.Bytes(), []byte(`"bodyMode":"plain"`)) {
			t.Fatalf("body native=%v: %d %s", native, body.Code, body.Body.String())
		}
		download := request("/api/mail/attachment"+query+"&index=0", native, true)
		if download.Code != http.StatusOK || !bytes.Equal(download.Body.Bytes(), attachment) || download.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("attachment native=%v: %d", native, download.Code)
		}
		metadata := request("/api/mail/attachments"+query, native, true)
		if metadata.Code != http.StatusOK || !bytes.Contains(metadata.Body.Bytes(), []byte(`"name":"proof.bin"`)) {
			t.Fatalf("metadata native=%v: %d %s", native, metadata.Code, metadata.Body.String())
		}
		for _, hidden := range []int{foreignUID, otherFolderUID} {
			if rec := request("/api/mail/body?mailbox=INBOX&messageId="+strconv.Itoa(hidden), native, true); rec.Code != http.StatusNotFound {
				t.Fatalf("scope leak native=%v id=%d: %d", native, hidden, rec.Code)
			}
		}
		inbox := request("/api/inbox?mailbox=INBOX&limit=100", native, true)
		if inbox.Code != http.StatusOK || len(allEmails(decodeInboxResponse(t, inbox))) != 1 {
			t.Fatalf("inbox native=%v: %d %s", native, inbox.Code, inbox.Body.String())
		}
		// Android and web actually request since=0&bodies=0, then use the
		// returned cursor. Exercise that wire contract, not just classic inbox.
		first := request("/api/inbox?mailbox=INBOX&limit=100&since=0&bodies=0", native, true)
		full := decodeInboxResponse(t, first)
		emails := allEmails(full)
		if first.Code != http.StatusOK || len(emails) != 1 || emails[0].MessageID != strconv.Itoa(uid) || emails[0].Body != "" || full.Cursor <= 0 {
			t.Fatalf("initial cursor native=%v: %d %s", native, first.Code, first.Body.String())
		}
		next := request("/api/inbox?mailbox=INBOX&limit=100&bodies=0&since="+strconv.FormatInt(full.Cursor, 10), native, true)
		delta := decodeInboxResponse(t, next)
		if next.Code != http.StatusOK || !delta.Delta || len(allEmails(delta)) != 0 || len(delta.Removed) != 0 {
			t.Fatalf("unchanged delta native=%v: %d %s", native, next.Code, next.Body.String())
		}
	}
	if rec := request("/api/mail/body"+query, false, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated read: %d", rec.Code)
	}
	largeStart := time.Now()
	largeAttachment := bytes.Repeat([]byte{0, 1, 127, 255}, 2*1024*1024)
	largeRaw := mailmsg.Message{From: "sender@example.test", To: []string{"owner@example.test"}, Body: "large attachment", Attachments: []mailmsg.Attachment{{Name: "large.bin", Content: largeAttachment}}}.Build()
	largeUID := insert(owner, "Large", largeRaw)
	largeDownload := request("/api/mail/attachment?mailbox=Large&messageId="+strconv.Itoa(largeUID)+"&index=0", true, true)
	if largeDownload.Code != http.StatusOK || !bytes.Equal(largeDownload.Body.Bytes(), largeAttachment) {
		t.Fatalf("8 MiB attachment: %d", largeDownload.Code)
	}
	t.Logf("8 MiB MIME insert+authenticated attachment read: %s; raw=%d bytes", time.Since(largeStart), len(largeRaw))
	// Exercise keyset enumeration beyond Mailflare's 10k query cap, without
	// fetching MIME BLOBs for the listing. This is storage feasibility only.
	start := time.Now()
	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 11001; i++ {
		if _, err := tx.Exec("INSERT INTO messages(owner, mailbox, raw) VALUES (?, 'Bulk', ?)", owner, []byte("From: sender@example.test\r\n\r\nbulk")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var count, after int
	for {
		rows, err := db.Query("SELECT uid FROM messages WHERE owner=? AND mailbox='Bulk' AND uid>? ORDER BY uid LIMIT 200", owner, after)
		if err != nil {
			t.Fatal(err)
		}
		page := 0
		for rows.Next() {
			if err := rows.Scan(&after); err != nil {
				t.Fatal(err)
			}
			page++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		count += page
		if page == 0 {
			break
		}
	}
	if count != 11001 {
		t.Fatalf("enumerated %d, want 11001", count)
	}
	t.Logf("storage-only 11001 insert+keyset enumeration: %s; MIME=%d attachment=%d bytes", time.Since(start), len(raw), len(attachment))
	for _, filename := range []string{path, path + "-wal"} {
		info, err := os.Stat(filename)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d bytes", filepath.Base(filename), info.Size())
	}
}
