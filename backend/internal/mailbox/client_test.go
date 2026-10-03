package mailbox

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
)

func newTestClient(t *testing.T) (*Store, *Client) {
	t.Helper()
	s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
	c, err := NewClient(s, "alice@example.test")
	must(t, err)
	return s, c
}
func importClient(t *testing.T, s *Store, id string, raw []byte) int {
	t.Helper()
	uid, err := s.Import(context.Background(), testReceipt(id), bytes.NewReader(raw))
	must(t, err)
	return int(uid)
}

func TestNativeClientReadParseAndActions(t *testing.T) {
	ctx := context.Background()
	s, c := newTestClient(t)
	content := bytes.Repeat([]byte("binary\x00attachment"), 1000)
	raw := mailmsg.Message{From: "sender@outside.test", To: []string{"alice@example.test"}, CC: []string{"cc@outside.test"}, Subject: "native message", Body: "body <not a tag>", Attachments: []mailmsg.Attachment{{Name: "test.bin", MimeType: "application/octet-stream", Content: content}}}.Build()
	uid := importClient(t, s, "first", raw)
	id := strconv.Itoa(uid)
	got, err := c.FetchRawMessage(ctx, "", uid)
	must(t, err)
	if !bytes.Equal(raw, got) {
		t.Fatal("raw MIME changed")
	}
	overviews, err := c.ListOverviews(ctx, "inbox", 0)
	must(t, err)
	if len(overviews) != 1 || overviews[0].Subject != "native message" || overviews[0].SenderBindingAddress != "sender@outside.test" {
		t.Fatalf("overview: %+v", overviews)
	}
	bodies, err := c.GetMessageBodies(ctx, "", []int{uid, uid, uid + 999})
	must(t, err)
	if len(bodies) != 1 || bodies[uid].Body != "body <not a tag>" || bodies[uid].BodyMode != "plain" || !bodies[uid].HasAttachments {
		t.Fatalf("bodies: %+v", bodies)
	}
	listed, err := c.ListUnreadMessages(ctx, "INBOX", 100)
	must(t, err)
	if len(listed) != 1 || listed[0].Body != bodies[uid].Body {
		t.Fatal("classic/read shape drift")
	}
	attachments, err := c.ListAttachments(ctx, "", uid)
	must(t, err)
	if len(attachments) != 1 || attachments[0].Name != "test.bin" {
		t.Fatalf("attachments: %+v", attachments)
	}
	info, data, err := c.GetAttachment(ctx, "", uid, 0)
	must(t, err)
	if info.Size != len(content) || !bytes.Equal(data, content) {
		t.Fatal("attachment changed")
	}
	if _, _, err = c.GetAttachment(ctx, "", uid, 1); !errors.Is(err, imapadapter.ErrAttachmentNotFound) {
		t.Fatal("missing attachment accepted")
	}
	headers, err := c.FetchHeaderFields(ctx, "", []int{uid, uid + 999}, "From", "from", "X-Missing")
	must(t, err)
	if len(headers) != 1 || len(headers[uid]) != 1 || !strings.HasPrefix(headers[uid][0], "From:") {
		t.Fatalf("header presence: %+v", headers)
	}
	must(t, c.EnsureLabel(ctx, "Travel"))
	must(t, c.ApplyLabel(ctx, id, "Travel"))
	must(t, c.ApplyLabel(ctx, id, "$Phishing"))
	must(t, c.RemoveLabel(ctx, id, "travel"))
	labels, err := c.ListLabels(ctx)
	must(t, err)
	if len(labels) != 2 {
		t.Fatalf("durable labels: %+v", labels)
	}
	must(t, c.ApplyInboxAction(ctx, id, "read", "INBOX", ""))
	listed, err = c.ListUnreadMessages(ctx, "", 100)
	must(t, err)
	if len(listed) != 1 || listed[0].Status != "read" || len(listed[0].Keywords) != 1 {
		t.Fatal("read/label state lost")
	}
	poll, next, err := c.ListUnreadInbox(ctx, "")
	must(t, err)
	if len(poll) != 0 || next != "0" {
		t.Fatal("poller classified read message")
	}
	_, err = c.CreateFolder(ctx, "INBOX", "Work")
	must(t, err)
	_, err = c.CreateFolder(ctx, "INBOX", "Work")
	must(t, err)
	must(t, c.ApplyInboxAction(ctx, id, "move", "INBOX", "INBOX/Work"))
	if _, err = c.FetchRawMessage(ctx, "INBOX", uid); !errors.Is(err, ErrNotFound) {
		t.Fatal("stale folder served")
	}
	renamed, err := c.RenameFolder(ctx, "INBOX/Work", "Personal")
	must(t, err)
	if renamed != "INBOX/Personal" {
		t.Fatal("renamed path")
	}
	must(t, c.DeleteFolder(ctx, renamed))
	got, err = c.FetchRawMessage(ctx, "INBOX", uid)
	must(t, err)
	if !bytes.Equal(got, raw) {
		t.Fatal("folder delete lost mail")
	}
	must(t, c.ApplyInboxAction(ctx, id, "archive", "INBOX", ""))
	folders, err := s.Folders(ctx)
	must(t, err)
	archive := ""
	for _, f := range folders {
		if strings.HasPrefix(f, "Archive/") {
			archive = f
		}
	}
	if archive == "" {
		t.Fatal("no year archive")
	}
	must(t, c.ApplyInboxAction(ctx, id, "spam", archive, ""))
	must(t, c.ApplyInboxAction(ctx, id, "delete", "Junk", ""))
	must(t, c.ApplyInboxAction(ctx, id, "delete", "Trash", ""))
	if _, err = c.FetchRawMessage(ctx, "Trash", uid); !errors.Is(err, ErrNotFound) {
		t.Fatal("permanent delete failed")
	}
}

func TestNativeClientPGPMIMEAndSenderBinding(t *testing.T) {
	ctx := context.Background()
	s, c := newTestClient(t)
	key, err := pgpmail.GenerateIdentity("Sender", "sender@outside.test")
	must(t, err)
	raw := mailmsg.Message{From: "Sender <sender@outside.test>", To: []string{"alice@example.test"}, Subject: "protected subject", Body: "private plaintext"}.Build()
	encrypted, err := pgpmail.EncryptMIME(raw, []string{key.ArmoredPublicKey}, key)
	must(t, err)
	signed, err := pgpmail.SignMIME(raw, key)
	must(t, err)
	for _, test := range []struct {
		id                string
		raw               []byte
		encrypted, signed bool
	}{{"encrypted", encrypted, true, false}, {"signed", signed, false, true}, {"file", mailmsg.Message{From: "sender@outside.test", To: []string{"alice@example.test"}, Attachments: []mailmsg.Attachment{{Name: "document.pgp", MimeType: "application/octet-stream", Content: []byte("-----BEGIN PGP MESSAGE-----\r\nx\r\n-----END PGP MESSAGE-----")}}}.Build(), false, false}} {
		uid := importClient(t, s, test.id, test.raw)
		bodies, err := c.GetMessageBodies(ctx, "INBOX", []int{uid})
		must(t, err)
		body := bodies[uid]
		if (body.PGPEncryptedPayload != "") != test.encrypted || (body.PGPSignaturePayload != "") != test.signed {
			t.Fatalf("%s PGP classification: %+v", test.id, body)
		}
		if test.encrypted && (body.Body != "" || body.BodyMode != "" || body.HasAttachments) {
			t.Fatal("ciphertext rendered as ordinary body/attachments")
		}
		exact, err := c.FetchRawMessage(ctx, "", uid)
		must(t, err)
		if !bytes.Equal(exact, test.raw) {
			t.Fatal("signed/encrypted wire bytes changed")
		}
		if test.signed {
			part, signature, err := pgpmail.ExtractSignedParts(exact)
			must(t, err)
			if len(part) == 0 || signature != body.PGPSignaturePayload {
				t.Fatal("detached signed parts differ")
			}
		}
	}
	encoded := []byte("From: =?utf-8?B?QWxpY2U=?= <sender@outside.test>\r\nSubject: =?utf-8?B?UsOpc3Vtw6k=?=\r\nContent-Type: text/plain\r\n\r\nplain <user@example.test>\r\n")
	uid := importClient(t, s, "encoded", encoded)
	overviews, err := c.ListOverviews(ctx, "", 10)
	must(t, err)
	var o imapadapter.Overview
	for _, v := range overviews {
		if v.UID == uid {
			o = v
		}
	}
	bodies, err := c.GetMessageBodies(ctx, "", []int{uid})
	must(t, err)
	if o.Subject != "Résumé" || !strings.Contains(o.Sender, "Alice") || o.SenderBindingAddress != bodies[uid].Sender {
		t.Fatalf("header/body parser drift: %+v %+v", o, bodies[uid])
	}
	multi := []byte("From: a@outside.test, b@outside.test\r\n\r\ntext")
	uid = importClient(t, s, "multiple", multi)
	overviews, err = c.ListOverviews(ctx, "", 10)
	must(t, err)
	bodies, err = c.GetMessageBodies(ctx, "", []int{uid})
	must(t, err)
	if overviews[0].SenderBindingAddress != "" || bodies[uid].Sender != "" {
		t.Fatal("multiple senders bound to one signer")
	}
	hits, err := c.SearchMessages(ctx, "", "body", "private plaintext", 10)
	must(t, err)
	if len(hits) != 1 {
		t.Fatal("search exposed encrypted plaintext or missed signed cleartext")
	}
}

func TestNativeClientPollSearchDraftsAndBounds(t *testing.T) {
	ctx := context.Background()
	s, c := newTestClient(t)
	first := importClient(t, s, "first", mailmsg.Message{From: "sender@outside.test", To: []string{"alice@example.test"}, Subject: "first", Body: "<b>html body</b>", Mode: "html"}.Build())
	second := importClient(t, s, "second", testRaw)
	poll, next, err := c.ListUnreadInbox(ctx, "")
	must(t, err)
	if len(poll) != 2 || poll[0].ID != strconv.Itoa(first) || next != strconv.Itoa(second) || poll[0].BodyHTML == "" {
		t.Fatalf("checkpoint/body views: %+v %s", poll, next)
	}
	poll, _, err = c.ListUnreadInbox(ctx, strconv.Itoa(first))
	must(t, err)
	if len(poll) != 1 || poll[0].ID != strconv.Itoa(second) {
		t.Fatal("checkpoint skipped unread")
	}
	for _, field := range []string{"sender", "from", "subject", "body", "all"} {
		query := "body"
		if field == "sender" || field == "from" {
			query = "sender"
		}
		if field == "subject" {
			query = "first"
		}
		hits, err := c.SearchMessages(ctx, "", field, query, 10)
		must(t, err)
		if len(hits) == 0 {
			t.Fatalf("search %s failed", field)
		}
	}
	for _, folder := range []string{"Drafts", "Sent"} {
		d := imapadapter.DraftMessage{To: []string{"recipient@outside.test"}, BCC: []string{"hidden@outside.test"}, Subject: "stored", Body: "copy"}
		if folder == "Drafts" {
			must(t, c.SaveDraft(ctx, d))
		} else {
			d.Raw = testRaw
			must(t, c.SaveSent(ctx, d))
		}
		copies, err := c.ListUnreadMessages(ctx, folder, 10)
		must(t, err)
		if len(copies) != 1 {
			t.Fatal("missing stored copy")
		}
		if folder == "Drafts" && (copies[0].BCC != "hidden@outside.test" || copies[0].Status != "unread") {
			t.Fatal("draft Bcc lost")
		}
		if folder == "Sent" {
			raw, err := c.FetchRawMessage(ctx, folder, mustUID(t, copies[0].MessageID))
			must(t, err)
			if !bytes.Equal(raw, testRaw) {
				t.Fatal("Sent raw rebuilt")
			}
		}
	}
	old := mailmsg.MaxInboundMessageBytes
	mailmsg.MaxInboundMessageBytes = int64(len(testRaw) - 1)
	t.Cleanup(func() { mailmsg.MaxInboundMessageBytes = old })
	bodies, err := c.GetMessageBodies(ctx, "", []int{second})
	must(t, err)
	if !bodies[second].TooLarge {
		t.Fatal("oversized batch body parsed")
	}
	if _, err = c.ListAttachments(ctx, "", second); !errors.Is(err, mailmsg.ErrMessageTooLarge) {
		t.Fatal("oversized attachment parsed")
	}
	if _, err = c.GetMessageBodies(ctx, "", make([]int, 1001)); err == nil {
		t.Fatal("unbounded body count")
	}
	if _, err = c.ListOverviews(ctx, "INBOX\nother", 10); !errors.Is(err, imapadapter.ErrUnsafeMailbox) {
		t.Fatal("unsafe folder accepted")
	}
	if err = c.ApplyLabel(ctx, strconv.Itoa(second), "two labels"); !errors.Is(err, imapadapter.ErrUnsafeKeyword) {
		t.Fatal("unsafe keyword accepted")
	}
	if _, err = c.FetchHeaderFields(ctx, "", []int{second}, "bad:header"); !errors.Is(err, imapadapter.ErrUnsafeHeaderField) {
		t.Fatal("unsafe header accepted")
	}
	if err = c.ApplyInboxAction(ctx, strconv.Itoa(second), "move", "INBOX", ""); err == nil {
		t.Fatal("missing move target defaulted")
	}
}
func mustUID(t *testing.T, id string) int {
	t.Helper()
	uid, err := strconv.Atoi(id)
	must(t, err)
	return uid
}

func TestNativeClientAtomicLabelsAndReadAcrossWriters(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	a := openTest(t, dir, testOwner, testLimits)
	b := openTest(t, dir, testOwner, testLimits)
	ca, err := NewClient(a, "alice@example.test")
	must(t, err)
	cb, err := NewClient(b, "alice@example.test")
	must(t, err)
	uid := importClient(t, a, "one", testRaw)
	id := strconv.Itoa(uid)
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, job := range []func() error{func() error { return ca.ApplyLabel(ctx, id, "Travel") }, func() error { return cb.ApplyLabel(ctx, id, "Work") }, func() error { return cb.ApplyInboxAction(ctx, id, "read", "INBOX", "") }} {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- job() }()
	}
	wg.Wait()
	for range 3 {
		must(t, <-errs)
	}
	list, err := a.List(ctx, "INBOX", 0, 10)
	must(t, err)
	if len(list) != 1 || !list[0].Seen || len(list[0].Labels) != 2 {
		t.Fatalf("concurrent flags lost: %+v", list)
	}
}

func TestNativeClientMalformedMIMEIsPerMessage(t *testing.T) {
	ctx := context.Background()
	s, c := newTestClient(t)
	malformed := []byte("From: sender@outside.test\r\nSubject: malformed\r\nContent-Type: multipart/mixed\r\nX-Trace: custom-header-value\r\n\r\nraw body")
	bad := importClient(t, s, "bad", malformed)
	good := importClient(t, s, "good", testRaw)
	poll, next, err := c.ListUnreadInbox(ctx, "")
	must(t, err)
	if len(poll) != 2 || !poll[0].ParseError || poll[1].ParseError || next != strconv.Itoa(good) {
		t.Fatalf("poison stopped poll: %+v %s", poll, next)
	}
	bodies, err := c.GetMessageBodies(ctx, "", []int{bad, good})
	must(t, err)
	if !bodies[bad].ParseError || bodies[good].Body == "" {
		t.Fatal("poison stopped body batch")
	}
	classic, err := c.ListUnreadMessages(ctx, "", 10)
	must(t, err)
	if len(classic) != 2 {
		t.Fatal("poison stopped classic inbox")
	}
	exact, err := c.FetchRawMessage(ctx, "", bad)
	must(t, err)
	if !bytes.Equal(exact, malformed) {
		t.Fatal("malformed raw discarded")
	}
	hits, err := c.SearchMessages(ctx, "", "all", "custom-header-value", 10)
	must(t, err)
	if len(hits) != 1 || hits[0].UID != bad {
		t.Fatal("all search omitted raw headers")
	}
	hits, err = c.SearchMessages(ctx, "", "all", "alice@example.test", 10)
	must(t, err)
	if len(hits) != 1 || hits[0].UID != good {
		t.Fatal("all search omitted recipients")
	}
}

func TestNativeClientLabelCatalogBoundAtSharedSink(t *testing.T) {
	ctx := context.Background()
	s, c := newTestClient(t)
	id := importClient(t, s, "one", testRaw)
	for _, label := range []string{"Travel", "travel", "TRAVEL"} {
		must(t, c.EnsureLabel(ctx, label))
	}
	labels, err := c.ListLabels(ctx)
	must(t, err)
	if len(labels) != 1 {
		t.Fatal("case variants spent catalog slots")
	}
	if err = s.Update(ctx, "INBOX", int64(id), false, false, []string{"Travel", "travel"}); err == nil {
		t.Fatal("case duplicate labels persisted")
	}
	for i := 0; i < 10; i++ {
		uid := importClient(t, s, "catalog-"+strconv.Itoa(i), testRaw)
		values := []string{}
		for j := 0; j < 100; j++ {
			values = append(values, "L"+strconv.Itoa(i)+"_"+strconv.Itoa(j))
		}
		err = s.Update(ctx, "INBOX", int64(uid), false, false, values)
		if i < 9 {
			must(t, err)
		} else if err == nil {
			t.Fatal("shared Update bypassed label catalog quota")
		}
	}
	labels, err = c.ListLabels(ctx)
	must(t, err)
	if len(labels) != 901 {
		t.Fatalf("partial catalog commit: %d", len(labels))
	}
}
