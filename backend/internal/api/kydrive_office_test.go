package api

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

const docxType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

const officeMail = "From: a@example.com\r\nTo: ann@example.com\r\nSubject: Plan\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nSee attached.\r\n--b\r\nContent-Type: " + docxType + "\r\nContent-Disposition: attachment; filename=plan.docx\r\nContent-Transfer-Encoding: base64\r\n\r\nUEsDBA==\r\n--b--\r\n"

const pgpMail = "From: a@example.com\r\nTo: ann@example.com\r\nSubject: ...\r\nMIME-Version: 1.0\r\nContent-Type: multipart/encrypted; protocol=\"application/pgp-encrypted\"; boundary=b\r\n\r\n--b\r\nContent-Type: application/pgp-encrypted\r\n\r\nVersion: 1\r\n--b\r\nContent-Type: application/octet-stream; name=encrypted.asc\r\n\r\n-----BEGIN PGP MESSAGE-----\r\nxx\r\n-----END PGP MESSAGE-----\r\n--b--\r\n"

func TestOfficeViewable(t *testing.T) {
	for _, tc := range []struct {
		name, ct string
		want     bool
	}{
		{"plan.docx", docxType, true},
		{"PLAN.DOCX", docxType + "; name=PLAN.DOCX", true},
		{"scan.pdf", "application/pdf", true},
		{"scan.pdf", "application/octet-stream", true},
		{"data.csv", "text/csv; charset=utf-8", true},
		{"deck.odp", "application/vnd.oasis.opendocument.presentation", true},
		{"plan.docx", "application/pdf", false},
		{"plan.docx", "text/html", false},
		{"page.html", "text/html", false},
		{"run.exe", "application/octet-stream", false},
		{"data.csv", "text/html", false},
		{"plan.doc", "application/msword", false},
		{"docx", docxType, false},
		{"plan.docx", "", false},
		{"plan.docx", ";;;", false},
	} {
		if got := officeViewable(tc.name, tc.ct); got != tc.want {
			t.Errorf("officeViewable(%q, %q) = %v", tc.name, tc.ct, got)
		}
	}
}

func officeFixture(t *testing.T) (*driveFixture, []byte) {
	f := newDriveFixture(t)
	f.drive.viewer = true
	data := append([]byte("PK\x03\x04"), bytes.Repeat([]byte("d"), 60)...)
	f.fake.attachments = map[int][]mailmsg.Attachment{
		5: {{Name: "plan.docx", MimeType: docxType, Content: data}, {Name: "plan.docx", MimeType: "text/html", Content: data}, {Name: "notes.txt", MimeType: "text/plain", Content: data}},
		6: {{Name: "plan.docx", MimeType: docxType, Content: data}},
	}
	f.fake.rawMessages = map[int][]byte{5: []byte(officeMail), 6: []byte(pgpMail)}
	return f, data
}

func TestKyDriveViewAttachment(t *testing.T) {
	f, data := officeFixture(t)
	if code, out := f.call(t, f.ann, "GET", "/api/kydrive/status", ""); code != 200 || out["viewer"] != true {
		t.Fatalf("status %d %v", code, out)
	}
	code, out := f.call(t, f.ann, "POST", "/api/kydrive/view-attachment", `{"messageId":"5","index":0}`)
	if code != 201 || !strings.HasPrefix(out["viewUrl"].(string), f.drive.URL+"/view.html?copy=view-") {
		t.Fatalf("open %d %v", code, out)
	}
	if got := f.drive.views["view-1"]; !bytes.Equal(got, data) {
		t.Fatalf("KyDrive holds %q", got)
	}
	for _, tc := range []struct{ body, code string }{
		{`{"messageId":"5","index":1}`, "unsupported_type"}, // name and type disagree
		{`{"messageId":"5","index":2}`, "unsupported_type"}, // not a viewable type
		{`{"messageId":"5","index":9}`, "not_found"},        // no such attachment in the caller's mailbox
		{`{"messageId":"6","index":0}`, "encrypted"},        // PGP mail, by its raw message
		{`{"messageId":"","index":0}`, "bad_request"},
		{`{"messageId":"5","index":-1}`, "bad_request"},
	} {
		if _, out := f.call(t, f.ann, "POST", "/api/kydrive/view-attachment", tc.body); out["code"] != tc.code {
			t.Errorf("%s: %v, want %s", tc.body, out, tc.code)
		}
	}
	// Another person naming ann's message and index reads their own mailbox only.
	ben := ssoUser(t, f.srv, "ben", f.drive.issuer, "sub-ben")
	if err := writeIMAPConfigPayload(f.srv.userIMAPConfigPath(ben.ID), f.srv.imapConfigKeyPath, imapConfigPayload{
		Host: "imap.example.com", Port: 993, Username: "ben@example.com", Password: "pw", Mailbox: "INBOX", UpdatedAt: "test",
	}); err != nil {
		t.Fatal(err)
	}
	f.srv.userMu.Lock()
	f.srv.userMail[ben.ID] = &serverMailEntry{client: &fakeMailClient{}, updatedAt: "test"}
	f.srv.userMu.Unlock()
	f.srv.driveStatuses.Store(ben.ID, driveStatusEntry{origin: f.srv.kydrive.Origin(), sub: "sub-ben", st: driveStatus{Available: true, Viewer: true, MaxFileBytes: 1 << 20}, until: time.Now().Add(time.Minute)})
	if code, out := f.call(t, ben, "POST", "/api/kydrive/view-attachment", `{"messageId":"5","index":0}`); code != 404 || out["code"] != "not_found" {
		t.Errorf("another user's message: %d %v", code, out)
	}
	if len(f.drive.views) != 1 {
		t.Fatalf("views %d", len(f.drive.views))
	}
	// A message whose raw bytes cannot be read counts as protected.
	f.fake.rawMessages = nil
	f.fake.attachments[7] = f.fake.attachments[6]
	f.fake.rawMessages = map[int][]byte{5: []byte(officeMail), 7: []byte("Content-Type: multipart/mixed; boundary=\"\r\n\r\n")}
	if _, out := f.call(t, f.ann, "POST", "/api/kydrive/view-attachment", `{"messageId":"7","index":0}`); out["code"] != "encrypted" {
		t.Errorf("unparseable message: %v", out)
	}
	// Over the save-attachment limit (KyDrive's largest file here).
	f.drive.maxFile = 10
	f.srv.driveStatuses.Clear()
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/view-attachment", `{"messageId":"5","index":0}`); code != 413 || out["code"] != "too_large" {
		t.Errorf("oversize: %d %v", code, out)
	}
	f.drive.maxFile = 100 << 20
	f.srv.driveStatuses.Clear()
	// An answer off the paired KyDrive's viewer never reaches the browser.
	f.drive.viewURL = "https://evil.example/view.html?copy=x"
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/view-attachment", `{"messageId":"5","index":0}`); code != 502 {
		t.Errorf("foreign view URL: %d %v", code, out)
	}
	f.drive.viewURL = f.drive.URL + "/view.html?copy=x&next=https://evil.example"
	if code, _ := f.call(t, f.ann, "POST", "/api/kydrive/view-attachment", `{"messageId":"5","index":0}`); code != 502 {
		t.Errorf("view URL with extra query: %d", code)
	}
	if logs := f.logs.String(); strings.Contains(logs, "plan.docx") || strings.Contains(logs, viewTicketPrefix) || strings.Contains(logs, "Plan") {
		t.Fatalf("logs carry a name, subject or ticket: %s", logs)
	}
	if !strings.Contains(f.logs.String(), "kydrive attachment opened") {
		t.Fatal("open not audited")
	}
}

const viewTicketPrefix = "/api/suite/view/"

func TestKyDriveEditAttachment(t *testing.T) {
	f, data := officeFixture(t)
	code, out := f.call(t, f.ann, "POST", "/api/kydrive/save-attachment", `{"messageId":"5","index":0,"edit":true}`)
	if code != 201 || out["editUrl"] != f.drive.URL+"/editor.html?file="+out["fileId"].(string) {
		t.Fatalf("edit %d %v", code, out)
	}
	if got := f.drive.files[out["fileId"].(string)]; !bytes.Equal(got.data, data) {
		t.Fatalf("saved %q", got.data)
	}
	saved := len(f.drive.files)
	for _, body := range []string{`{"messageId":"6","index":0,"edit":true}`, `{"messageId":"5","index":1,"edit":true}`} {
		if code, out := f.call(t, f.ann, "POST", "/api/kydrive/save-attachment", body); code < 400 || out["editUrl"] != nil {
			t.Errorf("%s: %d %v", body, code, out)
		}
	}
	if len(f.drive.files) != saved {
		t.Fatal("a refused edit saved a file")
	}
	// Plain save is unchanged for encrypted mail: no editor, no checks added.
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/save-attachment", `{"messageId":"6","index":0}`); code != 201 || out["editUrl"] != nil {
		t.Errorf("plain save: %d %v", code, out)
	}
	if !strings.Contains(f.logs.String(), "kydrive attachment edit") || strings.Contains(f.logs.String(), "plan.docx") {
		t.Errorf("edit audit: %s", f.logs.String())
	}
}

// Without a viewer, or with KyDrive off for the user, neither action works.
func TestKyDriveOfficeNeedsAViewer(t *testing.T) {
	f, _ := officeFixture(t)
	f.drive.viewer = false
	if code, out := f.call(t, f.ann, "GET", "/api/kydrive/status", ""); code != 200 || out["viewer"] != nil {
		t.Fatalf("status %d %v", code, out)
	}
	for _, path := range []string{"/api/kydrive/view-attachment", "/api/kydrive/save-attachment"} {
		if code, out := f.call(t, f.ann, "POST", path, `{"messageId":"5","index":0,"edit":true}`); code != 409 || out["code"] != "editor_off" {
			t.Errorf("%s: %d %v", path, code, out)
		}
	}
	f.drive.viewer = true
	f.drive.enabled["sub-ann"] = false
	f.srv.driveStatuses.Clear()
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/view-attachment", `{"messageId":"5","index":0}`); code != 409 || out["code"] != "kydrive_off" {
		t.Errorf("switch off: %d %v", code, out)
	}
	f.srv.SetKyDrive(nil)
	f.srv.driveStatuses.Clear()
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/view-attachment", `{"messageId":"5","index":0}`); code != 409 || out["code"] != "kydrive_off" {
		t.Errorf("unpaired: %d %v", code, out)
	}
}
