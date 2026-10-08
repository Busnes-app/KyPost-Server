//go:build linux

package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestMailImport(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SECRET_DIR", t.TempDir())
	srv := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "import-one", 1, runtimeDirectoryUser(true)))
	two := scimUser("native-runtime-two", "runtime-two", true)
	two["emails"] = []map[string]any{{"value": "two@example.test", "primary": true}}
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "import-two", 1, two))
	one, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-two")
	if err != nil {
		t.Fatal(err)
	}
	const password = "long-password-for-import"
	for _, id := range []string{one.ID, second.ID} {
		if _, err = srv.users.SetPassword(ctx, id, password, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := srv.users.Create(ctx, "import-admin", password, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	member, err := srv.users.Create(ctx, "import-imap", password, users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	type caller struct{ token, csrf string }
	session := func(id string) caller { tok, csrf := mintSessionForTest(srv, id); return caller{tok, csrf} }
	me, other, adm, imapUser := session(one.ID), session(second.ID), session(admin.ID), session(member.ID)
	srv.sessMu.Lock()
	srv.sessions["second-session"] = Session{UserID: one.ID, IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), CSRFToken: "second-csrf"}
	srv.sessions["sso-session"] = Session{UserID: one.ID, IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), CSRFToken: "sso-csrf", SSOKySignOn: true}
	srv.sessMu.Unlock()
	meAgain, meSSO := caller{"second-session", "second-csrf"}, caller{"sso-session", "sso-csrf"}

	call := func(method, path string, c caller, body string, header map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: c.token})
		req.Header.Set("X-CSRF-Token", c.csrf)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range header {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		return w
	}
	start := func(c caller, fields string) *httptest.ResponseRecorder {
		return call("POST", "/api/import", c, `{`+fields+`"password":"`+password+`"}`, nil)
	}
	grant := func(c caller, fields string) string {
		t.Helper()
		w := start(c, fields)
		var got struct {
			URL      string
			Expires  int `json:"expiresInSeconds"`
			MaxBytes int64
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || !strings.HasPrefix(got.URL, "/api/import/") || got.Expires != 300 || got.MaxBytes != importUploadCap {
			t.Fatalf("start %s: %d %s", fields, w.Code, w.Body)
		}
		return got.URL
	}
	upload := func(c caller, url string, data []byte) *httptest.ResponseRecorder {
		return call("POST", url, c, string(data), map[string]string{"Content-Type": "application/octet-stream"})
	}
	status := func(c caller) importJob {
		t.Helper()
		var st importJob
		if w := call("GET", "/api/import", c, "", nil); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &st) != nil {
			t.Fatal("status", w.Code, w.Body)
		}
		return st
	}
	wait := func(c caller) importJob {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if st := status(c); st.State != "running" && st.State != "uploading" {
				return st
			}
		}
		t.Fatal("import did not finish")
		return importJob{}
	}
	uploads := filepath.Join(srv.stateDir, importDir)
	noTempFiles := func(what string) {
		t.Helper()
		if left, _ := os.ReadDir(uploads); len(left) != 0 {
			t.Fatal(what, "left temp files", left)
		}
	}
	w := call("POST", "/api/admin/mailboxes", adm, `{"user":"`+one.ID+`","address":"sales@example.test","password":"`+password+`"}`, nil)
	var extra sso.NativeMailbox
	if json.Unmarshal(w.Body.Bytes(), &extra) != nil || w.Code != 200 {
		t.Fatal("create extra", w.Code, w.Body)
	}
	openStore := func(mailboxID string) *mailbox.Store {
		t.Helper()
		a, err := srv.ssoLifecycle.AdmitNativeMailbox(ctx, srv.stateDir, "https://idp.example", one.ID, mailboxID, srv.users)
		if err != nil {
			t.Fatal(err)
		}
		store, err := mailbox.OpenExisting(filepath.Join(a.Dir(srv.stateDir), "mailbox"), a.Owner, a.Limits, a.Source)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}

	var limits struct {
		State                     string
		MaxBytes, MaxMessageBytes int64
	}
	if w = call("GET", "/api/import", me, "", nil); json.Unmarshal(w.Body.Bytes(), &limits) != nil || limits.State != "idle" || limits.MaxBytes != nativeMailboxLimits.PayloadBytes || limits.MaxMessageBytes != 25<<20 {
		t.Fatal("fresh status", w.Body)
	}
	// Refusals before any link exists.
	for _, c := range []struct {
		name string
		w    *httptest.ResponseRecorder
		want int
	}{
		{"no step-up", call("POST", "/api/import", me, `{}`, nil), 401},
		{"wrong password", call("POST", "/api/import", me, `{"password":"wrong-password-here"}`, nil), 401},
		{"no csrf", call("POST", "/api/import", caller{me.token, ""}, `{"password":"`+password+`"}`, nil), 403},
		{"unsafe folder", start(me, `"folder":"a\\b",`), 400},
		{"missing parent", start(me, `"folder":"Nope/Child",`), 404},
		{"foreign mailbox", start(other, `"mailbox":"`+extra.ID+`",`), 404},
		{"path mailbox", start(me, `"mailbox":"../users/`+second.ID+`",`), 404},
		{"imap account", start(imapUser, ``), 409},
		{"administrator", start(adm, ``), 409},
		{"administrator naming a user's mailbox", start(adm, `"mailbox":"`+extra.ID+`",`), 404},
	} {
		if c.w.Code != c.want {
			t.Errorf("%s: got %d want %d: %s", c.name, c.w.Code, c.want, c.w.Body)
		}
	}
	if len(srv.importGrants) != 0 {
		t.Fatal("a refused request minted a link")
	}

	// A KySignOn session must confirm this exact request.
	w = call("POST", "/api/import", meSSO, `{}`, nil)
	var challenge struct{ Error, Challenge string }
	if w.Code != 403 || json.Unmarshal(w.Body.Bytes(), &challenge) != nil || challenge.Error != "sso_step_up_required" {
		t.Fatal("KySignOn session skipped step-up", w.Code, w.Body)
	}
	srv.stepUpMu.Lock()
	sc := srv.stepUps[challenge.Challenge]
	sc.verified = true
	srv.stepUps[challenge.Challenge] = sc
	srv.stepUpMu.Unlock()
	if w = call("POST", "/api/import", meSSO, `{"folder":"Other"}`, map[string]string{stepUpHeader: challenge.Challenge}); w.Code != 403 {
		t.Fatal("grant spent on a different request", w.Code, w.Body)
	}
	if w = call("POST", "/api/import", meSSO, `{}`, map[string]string{stepUpHeader: challenge.Challenge}); w.Code != 200 {
		t.Fatal("confirmed KySignOn import", w.Code, w.Body)
	}

	// mbox into the default folder: three good, one without headers, one
	// over the mailbox's 5 MiB message limit.
	big := "Subject: big\r\n\r\n" + strings.Repeat("x", int(nativeMailboxLimits.MessageBytes)) + "\r\n"
	mbox := []byte("From a@example.test Tue Sep  1 10:00:00 2026\r\nFrom: bank@example.test\r\nSubject: one\r\n\r\n>From quoted\r\n\r\n" +
		"From a b\r\nSubject: two\r\n\r\ntwo\r\n\r\n" +
		"From a b\r\nno headers here\r\n\r\n" +
		"From a b\r\n" + big + "\r\n" +
		"From a b\r\nSubject: three\r\n\r\nthree\r\n")
	url := grant(me, ``)
	if len(srv.importGrants) != 1 {
		t.Fatal("a new link did not replace the previous one", len(srv.importGrants))
	}
	if w = upload(caller{me.token, ""}, url, mbox); w.Code != 403 || len(srv.importGrants) != 1 {
		t.Fatal("upload without CSRF", w.Code)
	}
	for _, c := range []caller{meAgain, other, meSSO} {
		if w = upload(c, url, mbox); w.Code != 404 {
			t.Fatal("link crossed sessions or users", c.token, w.Code)
		}
	}
	if w = upload(me, url, mbox); w.Code != 202 {
		t.Fatal("upload", w.Code, w.Body)
	}
	st := wait(me)
	if st.State != "finished" || st.Folder != "Imported" || st.Imported != 3 || st.Skipped != 2 || st.Duplicates != 0 || st.Bytes == 0 {
		t.Fatalf("import status %+v", st)
	}
	if w = upload(me, url, mbox); w.Code != 404 {
		t.Fatal("link reused", w.Code)
	}
	noTempFiles("finished import")
	store := openStore(one.ID)
	list, err := store.List(ctx, "Imported", 0, 10)
	if err != nil || len(list) != 3 || !list[0].Seen || list[2].Subject != "one" {
		t.Fatal("stored", list, err)
	}
	if raw, err := store.Raw(ctx, "Imported", list[2].ID); err != nil || string(raw) != "From: bank@example.test\r\nSubject: one\r\n\r\nFrom quoted\r\n" {
		t.Fatalf("bytes %q %v", raw, err)
	}

	// Re-importing the same file adds nothing.
	if w = upload(me, grant(me, ``), mbox); w.Code != 202 {
		t.Fatal("re-upload", w.Code, w.Body)
	}
	if st = wait(me); st.State != "finished" || st.Imported != 0 || st.Duplicates != 3 || st.Skipped != 2 {
		t.Fatalf("re-import %+v", st)
	}

	// A zip into the extra mailbox's INBOX: stored seen, so the poller,
	// rules, notifications and incoming encryption never take it.
	var zipped bytes.Buffer
	z := zip.NewWriter(&zipped)
	for i, name := range []string{"INBOX/1.eml", "../escape.eml", "readme.txt"} {
		f, _ := z.Create(name)
		fmt.Fprintf(f, "Subject: zipped %d\r\n\r\nbody\r\n", i)
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	if w = upload(me, grant(me, `"mailbox":"`+extra.ID+`","folder":"inbox",`), zipped.Bytes()); w.Code != 202 {
		t.Fatal("zip upload", w.Code, w.Body)
	}
	if st = wait(me); st.State != "finished" || st.Mailbox != extra.ID || st.Folder != "INBOX" || st.Imported != 2 || st.Skipped != 1 {
		t.Fatalf("zip import %+v", st)
	}
	client, refused := nativeMail[*mailbox.Client](srv, ctx, one.ID, extra.ID, "")
	if refused != nil {
		t.Fatal(refused)
	}
	if unread, _, err := client.ListUnreadInbox(ctx, ""); err != nil || len(unread) != 0 {
		t.Fatal("imported mail reached the poller", len(unread), err)
	}

	// Too large for the mailbox: refused and nothing left behind.
	defer func(old int64) { importUploadCap = old }(importUploadCap)
	importUploadCap = 64
	if w = upload(me, grant(me, ``), mbox); w.Code != 413 || status(me).State != "failed" {
		t.Fatal("oversized upload", w.Code, w.Body)
	}
	noTempFiles("oversized upload")
	req := httptest.NewRequest("POST", grant(me, ``), &chunkedOnly{data: mbox})
	req.ContentLength = -1
	req.AddCookie(&http.Cookie{Name: "kypost_session", Value: me.token})
	req.Header.Set("X-CSRF-Token", me.csrf)
	w = httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)
	if w.Code != 413 {
		t.Fatal("oversized chunked upload", w.Code, w.Body)
	}
	noTempFiles("oversized chunked upload")
	importUploadCap = nativeMailboxLimits.PayloadBytes
	if w = upload(me, grant(me, ``), nil); w.Code != 400 || !strings.Contains(w.Body.String(), "empty") {
		t.Fatal("empty upload", w.Code, w.Body)
	}
	noTempFiles("empty upload")

	// The drive reserve: an upload that would cross it is refused up front
	// (507), and a running import stops before the message that would.
	defer func(orig func(string) (uint64, uint64, error)) { fsutil.DiskSpace = orig }(fsutil.DiskSpace)
	fsutil.DiskSpace = func(string) (uint64, uint64, error) { return 10<<30 + uint64(len(mbox)) - 1, 100 << 30, nil }
	if w = upload(me, grant(me, ``), mbox); w.Code != 507 || !strings.Contains(w.Body.String(), "kept for incoming mail") {
		t.Fatal("upload inside the reserve", w.Code, w.Body)
	}
	noTempFiles("reserve upload")
	// Room while the upload is written (its bytes promised), none after.
	fsutil.DiskSpace = func(string) (uint64, uint64, error) {
		if srv.importPending.Load() > 0 {
			return 1 << 50, 1 << 50, nil
		}
		return 10 << 30, 100 << 30, nil
	}
	if w = upload(me, grant(me, ``), mbox); w.Code != 202 {
		t.Fatal("upload above the reserve", w.Code, w.Body)
	}
	if st := wait(me); st.State != "failed" || st.Imported != 0 || !strings.Contains(st.Error, "kept for incoming mail") {
		t.Fatalf("import inside the reserve: %+v", st)
	}
	noTempFiles("reserve import")
	fsutil.DiskSpace = func(string) (uint64, uint64, error) { return 1 << 50, 1 << 50, nil }

	// Concurrency: one per user, maxImports overall; a busy refusal keeps the link.
	url = grant(me, ``)
	for _, busy := range [][]string{{one.ID}, {"someone", "someone-else"}} {
		srv.importMu.Lock()
		saved := srv.imports
		srv.imports = map[string]*importJob{}
		for _, id := range busy {
			srv.imports[id] = &importJob{State: "running"}
		}
		srv.importMu.Unlock()
		if w = upload(me, url, mbox); w.Code != 409 || len(srv.importGrants) != 1 {
			t.Fatal("busy upload", busy, w.Code, w.Body)
		}
		if w = start(me, ``); w.Code != 409 {
			t.Fatal("busy start", busy, w.Code, w.Body)
		}
		srv.importMu.Lock()
		srv.imports = saved
		srv.importMu.Unlock()
	}
	if w = upload(me, url, mbox); w.Code != 202 || wait(me).State != "finished" {
		t.Fatal("upload after the slot freed", w.Code, w.Body)
	}

	// Incoming encryption on: refused up front, and turning it on mid-job
	// stops the job before the next message.
	path := srv.userSettingsPath(one.ID)
	setEncryption := func(on bool) {
		t.Helper()
		if err := config.UpdateUserSettings(path, func(s *config.UserSettings) error { s.EncryptIncoming = on; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	setEncryption(true)
	if w = start(me, ``); w.Code != 409 || !strings.Contains(w.Body.String(), "incoming encryption") {
		t.Fatal("import while encryption is on", w.Code, w.Body)
	}
	setEncryption(false)
	url = grant(me, ``)
	setEncryption(true)
	if w = upload(me, url, mbox); w.Code != 409 || status(me).State != "failed" {
		t.Fatal("upload after encryption turned on", w.Code, w.Body)
	}
	noTempFiles("refused upload")
	setEncryption(false)
	native, refused := nativeMail[mailImporter](srv, ctx, one.ID, "", "")
	if refused != nil {
		t.Fatal(refused)
	}
	fresh := []byte("From a b\r\nSubject: fresh 1\r\n\r\nx\r\n\r\nFrom a b\r\nSubject: fresh 2\r\n\r\nx\r\n")
	job := &importJob{State: "running", Folder: "Imported", user: one.ID}
	srv.runImport(ctx, job, &onImport{mailImporter: native, after: func() {
		// Saved without the lock the import holds while storing.
		s, _ := config.LoadUserSettings(path)
		s.EncryptIncoming = true
		if err := config.SaveUserSettings(path, s); err != nil {
			t.Error(err)
		}
	}}, tempUpload(t, fresh), int64(len(fresh)))
	if job.State != "failed" || job.Imported != 1 || !strings.Contains(job.Error, "incoming encryption was turned on") {
		t.Fatalf("encryption mid-job %+v", job)
	}
	if list, err = store.List(ctx, "Imported", 0, 10); err != nil || len(list) != 4 {
		t.Fatal("plaintext stored after encryption turned on", len(list), err)
	}
	setEncryption(false)

	// Turned on concurrently through the settings lock: no message is stored
	// while the committed setting is on.
	if _, err = native.ImportFolder(ctx, "Concurrent", true); err != nil {
		t.Fatal(err)
	}
	var many bytes.Buffer
	for i := range 300 {
		fmt.Fprintf(&many, "From a b\r\nSubject: concurrent %d\r\n\r\nx\r\n\r\n", i)
	}
	var violated atomic.Bool
	var once sync.Once
	committed := make(chan struct{})
	job = &importJob{State: "running", Folder: "Concurrent", user: one.ID}
	// Checked before and after each store, both inside the import's lock.
	check := func() {
		if s, err := config.LoadUserSettings(path); err != nil || s.EncryptIncoming {
			violated.Store(true)
		}
	}
	srv.runImport(ctx, job, &onImport{mailImporter: native, before: check, after: func() {
		check()
		once.Do(func() {
			go func() {
				defer close(committed)
				if err := config.UpdateUserSettings(path, func(s *config.UserSettings) error { s.EncryptIncoming = true; return nil }); err != nil {
					t.Error(err)
				}
			}()
		})
	}}, tempUpload(t, many.Bytes()), int64(many.Len()))
	<-committed
	if violated.Load() || job.State != "failed" && job.State != "finished" || job.State == "failed" && !strings.Contains(job.Error, "incoming encryption was turned on") {
		t.Fatalf("concurrent encryption %+v violated=%v", job, violated.Load())
	}
	t.Logf("concurrent encryption: %s after %d messages", job.State, job.Imported)
	setEncryption(false)

	// Floods of junk or of duplicates stop at the job's message cap.
	defer func(old int) { importMaxMessages = old }(importMaxMessages)
	importMaxMessages = 50
	for name, data := range map[string][]byte{
		"empty":     bytes.Repeat([]byte("From \n\n"), 100000),
		"duplicate": bytes.Repeat([]byte("From a b\nSubject: flood\n\nx\n\n"), 1000),
	} {
		began := time.Now()
		job = &importJob{State: "running", Folder: "Imported", user: one.ID}
		srv.runImport(ctx, job, native, tempUpload(t, data), int64(len(data)))
		if job.State != "failed" || !strings.Contains(job.Error, "more than 50 messages") || job.Imported+job.Duplicates+job.Skipped != 50 || time.Since(began) > 10*time.Second {
			t.Fatalf("%s flood %+v in %s", name, job, time.Since(began))
		}
	}
	importMaxMessages = 2 * nativeMailboxLimits.Records

	// A trickling upload frees its slot at the whole-upload deadline.
	defer func(old time.Duration) { importUploadWindow = old }(importUploadWindow)
	importUploadWindow = -time.Second
	if w = upload(me, grant(me, ``), mbox); w.Code != 408 || !strings.Contains(w.Body.String(), "took too long") || status(me).State != "failed" {
		t.Fatal("slow upload", w.Code, w.Body)
	}
	noTempFiles("slow upload")
	importUploadWindow = 20 * time.Minute

	// Cancel stops a running job, whose temp file goes with it.
	jobCtx, cancel := context.WithCancel(context.Background())
	job = &importJob{State: "running", Folder: "Imported", user: one.ID, cancel: cancel}
	srv.importMu.Lock()
	srv.imports[one.ID] = job
	srv.importMu.Unlock()
	tmp := tempUpload(t, fresh)
	done := make(chan struct{})
	go func() {
		srv.runImport(jobCtx, job, blocking{}, tmp, int64(len(fresh)))
		close(done)
	}()
	if w = call("POST", "/api/import/cancel", other, "", nil); w.Code != 409 {
		t.Fatal("cancel another user's import", w.Code)
	}
	if w = call("POST", "/api/import/cancel", me, "", nil); w.Code != 200 {
		t.Fatal("cancel", w.Code, w.Body)
	}
	<-done
	if st = status(me); st.State != "cancelled" {
		t.Fatalf("cancelled status %+v", st)
	}
	if _, err = os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("cancelled import kept its upload", err)
	}
	if w = call("POST", "/api/import/cancel", me, "", nil); w.Code != 409 {
		t.Fatal("cancel with nothing running", w.Code)
	}

	// The target folder is created when the upload arrives, not before.
	url = grant(me, `"folder":"Arrives",`)
	if folders, _ := store.Folders(ctx); strings.Contains(fmt.Sprint(folders), "Arrives") {
		t.Fatal("folder created before the upload", folders)
	}
	if w = upload(me, url, mbox); w.Code != 202 || wait(me).Folder != "Arrives" {
		t.Fatal("upload into a new folder", w.Code, w.Body)
	}

	// An upload finishing after shutdown began starts no job.
	req = httptest.NewRequest("POST", grant(me, ``), &onRead{Reader: bytes.NewReader(mbox), fn: srv.cancelImports})
	req.AddCookie(&http.Cookie{Name: "kypost_session", Value: me.token})
	req.Header.Set("X-CSRF-Token", me.csrf)
	w = httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)
	if w.Code != 503 || status(me).State != "failed" {
		t.Fatal("upload during shutdown", w.Code, w.Body)
	}
	if w = upload(me, grant(me, ``), mbox); w.Code != 503 {
		t.Fatal("upload after shutdown", w.Code, w.Body)
	}
	noTempFiles("upload during shutdown")

	// A crash leftover is removed at startup.
	if err = os.MkdirAll(uploads, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(uploads, "upload-123"), mbox, 0o600); err != nil {
		t.Fatal(err)
	}
	srv.Prepare()
	if _, err = os.Stat(uploads); !os.IsNotExist(err) {
		t.Fatal("startup kept leftover uploads", err)
	}
}

// chunkedOnly hides its length, as a chunked upload does.
type chunkedOnly struct{ data []byte }

func (c *chunkedOnly) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.data)
	c.data = c.data[n:]
	return n, nil
}

// onRead calls fn on the first read, as a shutdown arriving mid-upload.
type onRead struct {
	io.Reader
	fn   func()
	done bool
}

func (o *onRead) Read(p []byte) (int, error) {
	if !o.done {
		o.done = true
		o.fn()
	}
	return o.Reader.Read(p)
}

type onImport struct {
	mailImporter
	before, after func()
}

func (o *onImport) ImportMessage(ctx context.Context, folder string, raw []byte, meta mailbox.ImportMeta) error {
	if o.before != nil {
		o.before()
	}
	err := o.mailImporter.ImportMessage(ctx, folder, raw, meta)
	if o.after != nil {
		o.after()
	}
	return err
}

// blocking stores nothing and waits for cancellation.
type blocking struct{}

func (blocking) ImportFolder(context.Context, string, bool) (string, error) { return "", nil }
func (blocking) ImportMessage(ctx context.Context, _ string, _ []byte, _ mailbox.ImportMeta) error {
	<-ctx.Done()
	return ctx.Err()
}

func tempUpload(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "upload")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A paired device cannot start an import, a promoted KyIdentity administrator
// gets the administrator 403, and a migrated legacyMixedUse administrator
// imports into their own primary mailbox.
func TestMailImportIdentities(t *testing.T) {
	ctx := context.Background()
	const password = "long-password-for-import"
	const imapBody = `{"host":"imap.example.com","port":993,"security":"tls","username":"u","password":"` + password + `"}`
	startAt := func(path, body string, s *Server, userID string, device ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if device != nil {
			setDeviceHeaders(r, device[0], device[1])
		} else {
			authRequestAs(s, r, userID)
		}
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return w
	}
	start := func(s *Server, userID string, device ...string) *httptest.ResponseRecorder {
		return startAt("/api/import", `{"password":"`+password+`"}`, s, userID, device...)
	}
	startIMAP := func(s *Server, userID string, device ...string) *httptest.ResponseRecorder {
		return startAt("/api/import/imap", imapBody, s, userID, device...)
	}

	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "everyday-create", 1, adminRuntimeUser(false)))
	u, err := s.users.GetBySSOSubIssuer(separationIssuer, "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.users.SetPassword(ctx, u.ID, password, false, nil); err != nil {
		t.Fatal(err)
	}
	deviceID, secret := pairNativeDevice(t, s, u.ID, "import-device")
	if w := start(s, u.ID, deviceID, secret); w.Code != 403 || !strings.Contains(w.Body.String(), "browser session") || len(s.importGrants) != 0 {
		t.Fatal("device started an import", w.Code, w.Body)
	}
	if w := startIMAP(s, u.ID, deviceID, secret); w.Code != 403 || !strings.Contains(w.Body.String(), "browser session") || len(s.importGrants) != 0 {
		t.Fatal("device started an IMAP import", w.Code, w.Body)
	}
	if w := startIMAP(s, u.ID); w.Code != 200 {
		t.Fatal("everyday IMAP import", w.Code, w.Body)
	}
	if w := start(s, u.ID); w.Code != 200 {
		t.Fatal("everyday import", w.Code, w.Body)
	}
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.updated", "everyday-promote", 2, adminRuntimeUser(true)))
	assertAdministratorRefusal(t, "promoted import", start(s, u.ID))
	assertAdministratorRefusal(t, "promoted IMAP import", startIMAP(s, u.ID))

	legacy := newNativeRuntimeServer(t)
	legacyMixedUseAdmin(t, legacy, runtimeDirectoryUser(true))
	lu, err := legacy.users.GetBySSOSubIssuer(separationIssuer, "native-runtime-one")
	if err != nil || lu.Role != users.RoleAdmin {
		t.Fatal("legacy administrator", lu.Role, err)
	}
	if _, err = legacy.users.SetPassword(ctx, lu.ID, password, false, nil); err != nil {
		t.Fatal(err)
	}
	if w := start(legacy, lu.ID); w.Code != 200 {
		t.Fatal("legacy administrator import", w.Code, w.Body)
	}
}
