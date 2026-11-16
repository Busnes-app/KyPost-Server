package api

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"

	"github.com/Busnes-app/kypost-server/backend/internal/kydrive"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

var driveSecret = []byte(strings.Repeat("d", 32))

// fakeDrive is KyDrive's side of contract C, enforced: event type per route,
// signature with a replay guard, the user by subject and issuer, the sharing
// switch, ticket rules and the upload route.
type fakeDrive struct {
	*httptest.Server
	mu       sync.Mutex
	issuer   string
	enabled  map[string]bool // sub -> sharing on; absent is no_user
	seen     map[string]bool
	tickets  map[string]*driveTicketState
	files    map[string]driveFile
	calls    map[string]int
	maxChunk int64
	maxFile  int64
	linkURL  string // overrides the link answer when set
	// guestMax is a new KyDrive's guest policy (status carries guest_links,
	// links honour audience); nil is an old KyDrive that knows neither.
	guestMax *int
	// audiences records the audience of every link request ("-" for none).
	audiences []string
	// guestOff makes a new KyDrive refuse guest links (policy or switch off).
	guestOff bool
	fetchCT  string // overrides the fetch answer's Content-Type when set
	// viewer is a KyDrive with Euro-Office (status viewer:true, view tickets).
	viewer bool
	views  map[string][]byte // viewing copy ID -> bytes
	// viewURL overrides the view upload's answer when set.
	viewURL string
}

type driveTicketState struct {
	sub, name string
	size      int64
	got       bytes.Buffer
}

type driveFile struct {
	owner, name string
	data        []byte
}

func newFakeDrive(t *testing.T, issuer string) *fakeDrive {
	d := &fakeDrive{issuer: issuer, enabled: map[string]bool{}, seen: map[string]bool{}, tickets: map[string]*driveTicketState{},
		files: map[string]driveFile{}, calls: map[string]int{}, maxChunk: 64 << 20, maxFile: 100 << 20}
	routes := map[string]string{kydrive.PathTicket: kydrive.EventTicket, kydrive.PathLink: kydrive.EventLink, kydrive.PathStatus: kydrive.EventStatus,
		kydrive.PathFetch: kydrive.EventFetch, kydrive.PathView: kydrive.EventView}
	d.views = map[string][]byte{}
	d.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		refuse := func(status int, code string) {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"error":"refused","code":%q}`, code)
		}
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, kydrive.UploadPrefix) {
			d.upload(w, r, refuse)
			return
		}
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, kydrive.ViewUploadPrefix) {
			d.viewUpload(w, r, refuse)
			return
		}
		eventType, ok := routes[r.URL.Path]
		if r.Method != http.MethodPost || !ok {
			refuse(http.StatusNotFound, "not_found")
			return
		}
		body, _ := io.ReadAll(r.Body)
		h := syncauth.FromRequest(r)
		if h.EventType != eventType {
			refuse(http.StatusUnauthorized, "bad_signature")
			return
		}
		if _, err := syncauth.Verify(driveSecret, h, body, syncauth.Options{}); err != nil {
			refuse(http.StatusUnauthorized, "bad_signature")
			return
		}
		if d.seen[h.EventID] {
			_, _ = w.Write([]byte(`{"outcome":"duplicate"}`))
			return
		}
		d.seen[h.EventID] = true
		d.calls[eventType]++
		var req struct {
			Sub, Issuer, Name string
			FileIDSnake       string `json:"file_id"`
			Size              int64
			Days              *int
			Audience          *string
			MaxBytes          *int64 `json:"max_bytes"`
		}
		if json.Unmarshal(body, &req) != nil {
			refuse(http.StatusBadRequest, "bad_body")
			return
		}
		on, known := d.enabled[req.Sub]
		switch {
		case req.Issuer != d.issuer || !known:
			refuse(http.StatusNotFound, "no_user")
			return
		case !on:
			refuse(http.StatusConflict, "sharing_off")
			return
		}
		switch eventType {
		case kydrive.EventStatus:
			guest := ""
			if d.guestMax != nil {
				guest = fmt.Sprintf(`,"guest_links":true,"guest_max_days":%d`, *d.guestMax)
			}
			if d.viewer {
				guest += `,"viewer":true`
			}
			fmt.Fprintf(w, `{"enabled":true,"free_bytes":%d,"max_file_bytes":%d%s}`, 1<<30, d.maxFile, guest)
		case kydrive.EventView:
			if !d.viewer {
				refuse(http.StatusConflict, "editor_off")
				return
			}
			tok := make([]byte, 32)
			_, _ = rand.Read(tok)
			d.tickets[hex.EncodeToString(tok)] = &driveTicketState{sub: req.Sub, name: req.Name, size: req.Size}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"upload_url":%q,"expires":"2026-12-14T00:00:00Z"}`, d.URL+kydrive.ViewUploadPrefix+hex.EncodeToString(tok))
		case kydrive.EventTicket:
			live := 0
			for _, tk := range d.tickets {
				if tk.sub == req.Sub {
					live++
				}
			}
			switch {
			case req.Name == "" || strings.ContainsAny(req.Name, "/\\"):
				refuse(http.StatusBadRequest, "bad_body")
			case req.Size > d.maxFile:
				refuse(http.StatusRequestEntityTooLarge, "too_large")
			case live >= 8:
				refuse(http.StatusTooManyRequests, "too_many_tickets")
			default:
				tok := make([]byte, 32)
				_, _ = rand.Read(tok)
				d.tickets[hex.EncodeToString(tok)] = &driveTicketState{sub: req.Sub, name: req.Name, size: req.Size}
				w.WriteHeader(http.StatusCreated)
				fmt.Fprintf(w, `{"upload_url":%q,"expires":%q,"max_chunk":%d}`, d.URL+kydrive.UploadPrefix+hex.EncodeToString(tok),
					time.Now().Add(15*time.Minute).UTC().Format(time.RFC3339), d.maxChunk)
			}
		case kydrive.EventLink:
			f, ok := d.files[req.FileIDSnake]
			d.audiences = append(d.audiences, cmpOr(req.Audience, "-"))
			guest := d.guestMax != nil && req.Audience != nil && *req.Audience == "guest"
			if req.Days == nil || (!guest && *req.Days != 0 && *req.Days != 1 && *req.Days != 7 && *req.Days != 30) ||
				(guest && (*req.Days < 1 || *req.Days > *d.guestMax)) {
				refuse(http.StatusBadRequest, "bad_body")
				return
			}
			if !ok || f.owner != req.Sub {
				refuse(http.StatusNotFound, "not_found")
				return
			}
			if guest && d.guestOff {
				refuse(http.StatusConflict, "guest_links_off")
				return
			}
			u := d.URL + "/l/#tok-" + req.FileIDSnake
			if guest {
				u = d.URL + "/g/#gtok-" + req.FileIDSnake
			}
			if d.linkURL != "" {
				u = d.linkURL
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"url":%q,"expires":"2026-12-14T00:00:00Z"}`, u)
		case kydrive.EventFetch:
			f, ok := d.files[req.FileIDSnake]
			switch {
			case req.MaxBytes == nil || *req.MaxBytes <= 0:
				refuse(http.StatusBadRequest, "bad_body")
			case !ok || f.owner != req.Sub:
				refuse(http.StatusNotFound, "not_found")
			case int64(len(f.data)) > *req.MaxBytes:
				refuse(http.StatusRequestEntityTooLarge, "too_large")
			default:
				w.Header().Set("Content-Type", cmp.Or(d.fetchCT, "application/octet-stream"))
				w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.name}))
				_, _ = w.Write(f.data)
			}
		}
	}))
	t.Cleanup(d.Close)
	return d
}

func (d *fakeDrive) upload(w http.ResponseWriter, r *http.Request, refuse func(int, string)) {
	tk := d.tickets[strings.TrimPrefix(r.URL.Path, kydrive.UploadPrefix)]
	if tk == nil || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
		refuse(http.StatusNotFound, "not_found")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, d.maxChunk+1))
	if int64(len(body)) != r.ContentLength || int64(len(body)) > d.maxChunk {
		refuse(http.StatusBadRequest, "bad_body")
		return
	}
	if cr := r.Header.Get("Content-Range"); cr != "" {
		var from, to, total int64
		if _, err := fmt.Sscanf(cr, "bytes %d-%d/%d", &from, &to, &total); err != nil || from != int64(tk.got.Len()) || total != tk.size || to-from+1 != int64(len(body)) {
			refuse(http.StatusBadRequest, "bad_range")
			return
		}
	} else if int64(len(body)) != tk.size {
		refuse(http.StatusBadRequest, "bad_body")
		return
	}
	tk.got.Write(body)
	if int64(tk.got.Len()) < tk.size {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	id := "file-" + strconv.Itoa(len(d.files)+1)
	d.files[id] = driveFile{owner: tk.sub, name: tk.name, data: append([]byte(nil), tk.got.Bytes()...)}
	delete(d.tickets, strings.TrimPrefix(r.URL.Path, kydrive.UploadPrefix))
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, `{"file_id":%q,"name":%q,"size":%d}`, id, tk.name, tk.size)
}

func (d *fakeDrive) viewUpload(w http.ResponseWriter, r *http.Request, refuse func(int, string)) {
	key := strings.TrimPrefix(r.URL.Path, kydrive.ViewUploadPrefix)
	tk := d.tickets[key]
	body, _ := io.ReadAll(r.Body)
	if tk == nil || r.Header.Get("Cookie") != "" || r.Header.Get("Content-Range") != "" || int64(len(body)) != tk.size {
		refuse(http.StatusNotFound, "not_found")
		return
	}
	delete(d.tickets, key)
	id := "view-" + strconv.Itoa(len(d.views)+1)
	d.views[id] = body
	tk.name = id // the copy's name is never needed again
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, `{"view_url":%q,"expires":"2026-12-14T00:00:00Z"}`, cmp.Or(d.viewURL, d.URL+"/view.html?copy="+id))
}

type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type driveFixture struct {
	srv   *Server
	drive *fakeDrive
	ann   users.User
	logs  *syncLog
	fake  *fakeMailClient
}

// newDriveFixture: KyPost signs in through a KyIdentity that lists KyDrive for
// sub-ann only; KyDrive is paired and has ann with sharing on.
func newDriveFixture(t *testing.T) *driveFixture {
	t.Helper()
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, secret, ok := r.BasicAuth(); !ok || id != "kypost" || secret != "suite-secret" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/suite/people" {
			emails := r.URL.Query()["email"]
			if r.URL.Query().Get("sub") != "sub-ann" || len(emails) == 0 || len(emails) > 100 || emails[0] == "down@example.com" {
				http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
				return
			}
			var people []string
			for _, e := range emails {
				if strings.HasSuffix(e, "@example.com") {
					people = append(people, fmt.Sprintf(`{"sub":"s-%s","name":"N","email":%q}`, e, strings.ToUpper(e)))
				}
			}
			fmt.Fprintf(w, `{"people":[%s]}`, strings.Join(people, ","))
			return
		}
		if r.URL.Path != "/api/suite/apps" {
			http.NotFound(w, r)
			return
		}
		apps := `{"id":"kypost","name":"KyPost","url":"https://mail.example.com/"}`
		if r.URL.Query().Get("sub") == "sub-ann" {
			apps += `,{"id":"kydrive","name":"KyDrive","url":"https://drive.example.com/"}`
		}
		fmt.Fprintf(w, `{"apps":[%s]}`, apps)
	}))
	t.Cleanup(idp.Close)
	s := newTestServer(t)
	if err := s.ssoStore.Save(sso.SSOSettings{Enabled: true, IssuerURL: idp.URL + "/", ClientID: "kypost", ClientSecret: "suite-secret"}); err != nil {
		t.Fatal(err)
	}
	logs := &syncLog{}
	logger, err := logging.NewWithOutput(logs)
	if err != nil {
		t.Fatal(err)
	}
	s.logger = logger
	ann := ssoUser(t, s, "ann", idp.URL, "sub-ann")
	drive := newFakeDrive(t, idp.URL)
	drive.enabled["sub-ann"] = true
	base, _ := url.Parse(drive.URL)
	s.SetKyDrive(kydrive.New(base, driveSecret, drive.Client().Transport))
	s.imapConfigKeyPath = filepath.Join(t.TempDir(), "imap-config.key")
	if err := writeIMAPConfigPayload(s.userIMAPConfigPath(ann.ID), s.imapConfigKeyPath, imapConfigPayload{
		Host: "imap.example.com", Port: 993, Username: "ann@example.com", Password: "pw", Mailbox: "INBOX", UpdatedAt: "test",
	}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeMailClient{}
	s.userMu.Lock()
	s.userMail[ann.ID] = &serverMailEntry{client: fake, updatedAt: "test"}
	s.userMu.Unlock()
	return &driveFixture{srv: s, drive: drive, ann: ann, logs: logs, fake: fake}
}

func (f *driveFixture) call(t *testing.T, u users.User, method, path, body string) (int, map[string]any) {
	t.Helper()
	rec := requestBodyAs(f.srv, u.ID, method, path, body)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Header().Get("Cache-Control") != "no-store" && rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("%s %s: not no-store", method, path)
	}
	return rec.Code, out
}

func TestKyDriveStatus(t *testing.T) {
	f := newDriveFixture(t)
	code, st := f.call(t, f.ann, "GET", "/api/kydrive/status", "")
	if code != 200 || st["available"] != true || st["appUrl"] != "https://drive.example.com/" || st["maxFileBytes"] != float64(100<<20) || st["origin"] != f.drive.URL {
		t.Fatalf("status %d %v", code, st)
	}
	// Cached for a minute per user: a second read does not ask KyDrive.
	f.call(t, f.ann, "GET", "/api/kydrive/status", "")
	if n := f.drive.calls[kydrive.EventStatus]; n != 1 {
		t.Fatalf("KyDrive asked %d times", n)
	}

	// Not in the suite app list, no SSO link, or not a KyDrive user: hidden.
	bob := ssoUser(t, f.srv, "bob", f.drive.issuer, "sub-bob")
	local := ssoUser(t, f.srv, "loc", "", "")
	for _, u := range []users.User{bob, local} {
		if code, st := f.call(t, u, "GET", "/api/kydrive/status", ""); code != 200 || st["available"] != false || st["appUrl"] != nil {
			t.Fatalf("%s: %d %v", u.Username, code, st)
		}
	}
	// The switch off in KyDrive: hidden once the cache lapses.
	f.drive.enabled["sub-ann"] = false
	f.srv.driveStatuses.Clear()
	if _, st := f.call(t, f.ann, "GET", "/api/kydrive/status", ""); st["available"] != false {
		t.Fatalf("sharing off still available: %v", st)
	}
	// Unpaired.
	f.srv.SetKyDrive(nil)
	if _, st := f.call(t, f.ann, "GET", "/api/kydrive/status", ""); st["available"] != false || st["origin"] != nil {
		t.Fatalf("unpaired available: %v", st)
	}
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/upload-tickets", `{"name":"a.pdf","size":3}`); code != 409 || out["code"] != "kydrive_off" {
		t.Fatalf("unpaired ticket: %d %v", code, out)
	}
	w := httptest.NewRecorder()
	f.srv.routes().ServeHTTP(w, httptest.NewRequest("GET", "/api/kydrive/status", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no session: %d", w.Code)
	}
}

func TestKyDriveTicketAndLink(t *testing.T) {
	f := newDriveFixture(t)
	code, tk := f.call(t, f.ann, "POST", "/api/kydrive/upload-tickets", `{"name":"report.pdf","size":10}`)
	uploadURL, _ := tk["uploadUrl"].(string)
	if code != 201 || !strings.HasPrefix(uploadURL, f.drive.URL+kydrive.UploadPrefix) || tk["maxChunk"] != float64(64<<20) || tk["expires"] == "" {
		t.Fatalf("ticket %d %v", code, tk)
	}
	// The browser's PUT, then the link.
	req, _ := http.NewRequest(http.MethodPut, uploadURL, strings.NewReader("0123456789"))
	resp, err := f.drive.Client().Do(req)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("upload %v %v", resp, err)
	}
	var file struct {
		FileID string `json:"file_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&file)
	resp.Body.Close()
	code, link := f.call(t, f.ann, "POST", "/api/kydrive/links", `{"fileId":"`+file.FileID+`"}`)
	if code != 201 || link["url"] != f.drive.URL+"/l/#tok-"+file.FileID || link["expires"] != "2026-12-14T00:00:00Z" {
		t.Fatalf("link %d %v", code, link)
	}
	if code, _ := f.call(t, f.ann, "POST", "/api/kydrive/links", `{"fileId":"`+file.FileID+`","days":0}`); code != 201 {
		t.Fatalf("no-expiry link %d", code)
	}
	if logs := f.logs.String(); strings.Contains(logs, strings.TrimPrefix(uploadURL, f.drive.URL)) || strings.Contains(logs, "report.pdf") ||
		strings.Contains(logs, "tok-") || strings.Contains(logs, "sub-ann") {
		t.Fatalf("logs carry a ticket, link, name or subject: %s", logs)
	}

	for name, tc := range map[string]struct {
		path, body string
		status     int
		code       string
	}{
		"empty name":      {"/api/kydrive/upload-tickets", `{"name":"","size":1}`, 400, "bad_request"},
		"path in name":    {"/api/kydrive/upload-tickets", `{"name":"../x","size":1}`, 400, "bad_request"},
		"control in name": {"/api/kydrive/upload-tickets", `{"name":"a\u0007b","size":1}`, 400, "bad_request"},
		"no size":         {"/api/kydrive/upload-tickets", `{"name":"a"}`, 400, "bad_request"},
		"negative size":   {"/api/kydrive/upload-tickets", `{"name":"a","size":-1}`, 400, "bad_request"},
		"oversize":        {"/api/kydrive/upload-tickets", `{"name":"a","size":104857601}`, 413, "too_large"},
		"bad days":        {"/api/kydrive/links", `{"fileId":"file-1","days":5}`, 400, "bad_request"},
		"no file id":      {"/api/kydrive/links", `{}`, 400, "bad_request"},
		"someone else's":  {"/api/kydrive/links", `{"fileId":"file-9"}`, 404, "not_found"},
	} {
		if code, out := f.call(t, f.ann, "POST", tc.path, tc.body); code != tc.status || out["code"] != tc.code {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	for range 8 {
		f.call(t, f.ann, "POST", "/api/kydrive/upload-tickets", `{"name":"a","size":1}`)
	}
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/upload-tickets", `{"name":"a","size":1}`); code != 429 || out["code"] != "too_many_tickets" {
		t.Fatalf("ninth live ticket: %d %v", code, out)
	}

	// A link KyDrive answered off its own origin never reaches the browser.
	f.drive.linkURL = "https://evil.example.com/l/#x"
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/links", `{"fileId":"`+file.FileID+`"}`); code != 502 || out["url"] != nil {
		t.Fatalf("off-origin link: %d %v", code, out)
	}
	// Our signature refused (a rotated secret): unavailable, not passed through.
	f.drive.linkURL = ""
	base, _ := url.Parse(f.drive.URL)
	f.srv.SetKyDrive(kydrive.New(base, []byte(strings.Repeat("x", 32)), f.drive.Client().Transport))
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/links", `{"fileId":"`+file.FileID+`"}`); code != 502 || out["code"] != "kydrive_unavailable" {
		t.Fatalf("wrong secret: %d %v", code, out)
	}
	// The switch turned off in KyDrive between status reads: passed through.
	f.srv.SetKyDrive(kydrive.New(base, driveSecret, f.drive.Client().Transport))
	f.drive.enabled["sub-ann"] = false
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/links", `{"fileId":"`+file.FileID+`"}`); code != 409 || out["code"] != "sharing_off" {
		t.Fatalf("sharing off: %d %v", code, out)
	}
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/links", `{"fileId":"`+file.FileID+`"}`); code != 409 || out["code"] != "kydrive_off" {
		t.Fatalf("after sharing_off the status cache was not dropped: %d %v", code, out)
	}
}

func TestKyDriveSaveAttachment(t *testing.T) {
	f := newDriveFixture(t)
	data := bytes.Repeat([]byte("0123456789"), 10)
	f.fake.attachments = map[int][]mailmsg.Attachment{5: {{Name: "scan.pdf", MimeType: "application/pdf", Content: data}}}
	f.drive.maxChunk = 30 // four ordered chunks
	code, out := f.call(t, f.ann, "POST", "/api/kydrive/save-attachment", `{"messageId":"5","index":0}`)
	if code != 201 || out["name"] != "scan.pdf" || out["folder"] != "Mail attachments" || out["appUrl"] != "https://drive.example.com/" {
		t.Fatalf("save %d %v", code, out)
	}
	got := f.drive.files[out["fileId"].(string)]
	if got.owner != "sub-ann" || !bytes.Equal(got.data, data) {
		t.Fatalf("KyDrive holds %q for %s", got.data, got.owner)
	}
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/save-attachment", `{"messageId":"5","index":3}`); code != 404 || out["code"] != "not_found" {
		t.Fatalf("missing part: %d %v", code, out)
	}
	f.drive.maxFile = 10
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/save-attachment", `{"messageId":"5","index":0}`); code != 413 || out["code"] != "too_large" {
		t.Fatalf("too large: %d %v", code, out)
	}
	if logs := f.logs.String(); strings.Contains(logs, "scan.pdf") || strings.Contains(logs, kydrive.UploadPrefix) {
		t.Fatalf("logs carry the name or a ticket: %s", logs)
	}
}

// Attach a copy (contract I2): the file the user picked comes back as an
// attachment, signed as kypost.drive.fetch with the attachment limit as
// max_bytes; KyDrive's refusals and an odd answer never reach compose as a file.
func TestKyDriveCopy(t *testing.T) {
	f := newDriveFixture(t)
	f.drive.files["file-1"] = driveFile{owner: "sub-ann", name: "Résumé été.pdf", data: []byte("%PDF-1.7 hello")}
	f.drive.files["file-2"] = driveFile{owner: "sub-bob", name: "bob.pdf", data: []byte("x")}
	f.drive.files["file-3"] = driveFile{owner: "sub-ann", name: "big.bin", data: make([]byte, maxMailAttachmentBytes+1)}
	f.drive.files["file-4"] = driveFile{owner: "sub-ann", name: "a\u0007b", data: []byte("y")}
	code, out := f.call(t, f.ann, "POST", "/api/kydrive/copies", `{"fileId":"file-1"}`)
	if code != 200 || out["name"] != "Résumé été.pdf" || out["mimeType"] != "application/pdf" || out["size"] != float64(14) ||
		out["dataBase64"] != base64.StdEncoding.EncodeToString([]byte("%PDF-1.7 hello")) {
		t.Fatalf("copy %d %v", code, out)
	}
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/copies", `{"fileId":"file-4"}`); code != 200 || out["name"] != "attachment" {
		t.Fatalf("unsafe name: %d %v", code, out["name"])
	}
	for name, tc := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"no file id":     {`{}`, 400, "bad_request"},
		"space in id":    {`{"fileId":"a b"}`, 400, "bad_request"},
		"long id":        {`{"fileId":"` + strings.Repeat("a", 129) + `"}`, 400, "bad_request"},
		"someone else's": {`{"fileId":"file-2"}`, 404, "not_found"},
		"missing":        {`{"fileId":"file-9"}`, 404, "not_found"},
		"over the limit": {`{"fileId":"file-3"}`, 413, "too_large"},
	} {
		if code, out := f.call(t, f.ann, "POST", "/api/kydrive/copies", tc.body); code != tc.status || out["code"] != tc.code || out["dataBase64"] != nil {
			t.Errorf("%s: %d %v", name, code, out["code"])
		}
	}
	f.drive.fetchCT = "text/html"
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/copies", `{"fileId":"file-1"}`); code != 502 || out["dataBase64"] != nil {
		t.Fatalf("wrong content type: %d %v", code, out["code"])
	}
	f.drive.fetchCT = ""
	if logs := f.logs.String(); strings.Contains(logs, "Résumé") || strings.Contains(logs, "sub-ann") || strings.Contains(logs, "hello") {
		t.Fatalf("logs carry a name, subject or content: %s", logs)
	}
	// Switch off in KyDrive passes through; unpaired hides the option.
	f.drive.enabled["sub-ann"] = false
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/copies", `{"fileId":"file-1"}`); code != 409 || out["code"] != "sharing_off" {
		t.Fatalf("sharing off: %d %v", code, out)
	}
	f.srv.SetKyDrive(nil)
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/copies", `{"fileId":"file-1"}`); code != 409 || out["code"] != "kydrive_off" {
		t.Fatalf("unpaired: %d %v", code, out)
	}
}

func TestKyDriveRateLimit(t *testing.T) {
	f := newDriveFixture(t)
	f.srv.kydriveLimiter = newKeyedLimiter(1, 0)
	f.call(t, f.ann, "GET", "/api/kydrive/status", "")
	rec := requestAs(f.srv, f.ann.ID, "GET", "/api/kydrive/status")
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), `"rate_limited"`) {
		t.Fatalf("second request: %d %s", rec.Code, rec.Body)
	}
}

// Dedicated administrator identities have no mailbox, so no KyDrive either.
func TestKyDriveRefusesAdministratorIdentity(t *testing.T) {
	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "admin-create", 1, adminRuntimeUser(true)))
	u := assertMailboxless(t, s, "native-runtime-one", users.RoleAdmin)
	base, _ := url.Parse("https://drive.example.com")
	s.SetKyDrive(kydrive.New(base, driveSecret, http.DefaultTransport))
	for _, path := range []string{"/api/kydrive/status", "/api/kydrive/upload-tickets", "/api/kydrive/links", "/api/kydrive/save-attachment"} {
		method := map[bool]string{true: "GET", false: "POST"}[strings.HasSuffix(path, "status")]
		assertAdministratorRefusal(t, path, requestBodyAs(s, u.ID, method, path, `{}`))
	}
}

func TestAdminKyDriveStatus(t *testing.T) {
	f := newDriveFixture(t)
	admin, err := f.srv.users.Create(t.Context(), "root", "root-testpassword", users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if rec := requestAs(f.srv, admin.ID, "GET", "/api/admin/kydrive"); rec.Code != 200 ||
		rec.Body.String() != `{"source":"env","connected":true,"url":"`+f.drive.URL+`"}`+"\n" {
		t.Fatalf("admin status %d %s", rec.Code, rec.Body)
	}
	if rec := requestAs(f.srv, f.ann.ID, "GET", "/api/admin/kydrive"); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin got %d", rec.Code)
	}
}

// connect-src names the paired KyDrive only while it is paired.
func TestCSPConnectsToKyDriveOnlyWhilePaired(t *testing.T) {
	f := newDriveFixture(t)
	csp := func() string {
		w := httptest.NewRecorder()
		f.srv.routes().ServeHTTP(w, httptest.NewRequest("GET", "/api/health", nil))
		return w.Header().Get("Content-Security-Policy")
	}
	if !strings.Contains(csp(), "connect-src 'self' "+f.drive.URL+";") {
		t.Fatalf("paired CSP: %s", csp())
	}
	f.srv.SetKyDrive(nil)
	if p := csp(); !strings.Contains(p, "connect-src 'self';") || strings.Contains(p, f.drive.URL) {
		t.Fatalf("unpaired CSP: %s", p)
	}
}

// The KyDrive picker answers through window.opener, so COOP lets popups keep
// it while paired and is same-origin otherwise.
func TestCOOPAllowsPopupsOnlyWhileKyDrivePaired(t *testing.T) {
	f := newDriveFixture(t)
	coop := func() string {
		w := httptest.NewRecorder()
		f.srv.routes().ServeHTTP(w, httptest.NewRequest("GET", "/api/health", nil))
		return w.Header().Get("Cross-Origin-Opener-Policy")
	}
	if got := coop(); got != "same-origin-allow-popups" {
		t.Fatalf("paired COOP %q", got)
	}
	f.srv.SetKyDrive(nil)
	if got := coop(); got != "same-origin" {
		t.Fatalf("unpaired COOP %q", got)
	}
}

func TestKyDriveRecipients(t *testing.T) {
	f := newDriveFixture(t)
	many := make([]string, 0, 150)
	for i := range 150 {
		many = append(many, fmt.Sprintf(`"p%d@example.com"`, i))
	}
	for name, tc := range map[string]struct {
		body    string
		status  int
		outside string
	}{
		"mixed":         {`{"addresses":["ann@example.com","Bob@Example.NET","bob@example.net"]}`, 200, `["bob@example.net"]`},
		"all inside":    {`{"addresses":["ann@example.com"]}`, 200, `[]`},
		"over 100":      {`{"addresses":[` + strings.Join(many, ",") + `,"x@other.test"]}`, 200, `["x@other.test"]`},
		"display name":  {`{"addresses":["Ann <ann@example.com>"]}`, 400, ""},
		"none":          {`{"addresses":[]}`, 400, ""},
		"identity down": {`{"addresses":["down@example.com"]}`, 503, ""},
	} {
		rec := requestBodyAs(f.srv, f.ann.ID, "POST", "/api/kydrive/recipients", tc.body)
		var out struct{ Outside json.RawMessage }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != tc.status || (tc.outside != "" && string(out.Outside) != tc.outside) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	local := ssoUser(t, f.srv, "loc", "", "")
	if rec := requestBodyAs(f.srv, local.ID, "POST", "/api/kydrive/recipients", `{"addresses":["ann@example.com"]}`); rec.Code != 409 {
		t.Fatalf("unlinked account: %d %s", rec.Code, rec.Body)
	}
	if logs := f.logs.String(); strings.Contains(logs, "bob@example.net") || strings.Contains(logs, "down@example.com") {
		t.Fatalf("logs carry an address: %s", logs)
	}
}

func cmpOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

// stepUp marks ann's test session as having stepped up at t.
func (f *driveFixture) stepUp(at time.Time) {
	f.srv.sessMu.Lock()
	defer f.srv.sessMu.Unlock()
	tok := "session-token-" + f.ann.ID
	sess := f.srv.sessions[tok]
	sess.SteppedUpAt = at
	f.srv.sessions[tok] = sess
}

// Guest links (contract K): only when KyDrive's status offers them, within its
// maximum days, after a step-up in the last ten minutes; the answer must be a
// /g/ link on the paired origin.
func TestKyDriveGuestLinks(t *testing.T) {
	f := newDriveFixture(t)
	maxDays := 14
	f.drive.guestMax = &maxDays
	f.drive.files["file-1"] = driveFile{owner: "sub-ann", name: "plan.pdf", data: []byte("x")}
	if code, st := f.call(t, f.ann, "GET", "/api/kydrive/status", ""); code != 200 || st["guestLinks"] != true || st["guestMaxDays"] != float64(14) {
		t.Fatalf("status %d %v", code, st)
	}
	guest := func(days int) (int, map[string]any) {
		return f.call(t, f.ann, "POST", "/api/kydrive/links", fmt.Sprintf(`{"fileId":"file-1","days":%d,"audience":"guest"}`, days))
	}
	// No step-up, then a lapsed one: refused before KyDrive is asked.
	if code, out := guest(7); code != 403 || out["code"] != "step_up_required" {
		t.Fatalf("no step-up: %d %v", code, out)
	}
	f.stepUp(time.Now().Add(-guestStepUpWindow - time.Second))
	if code, out := guest(7); code != 403 || out["code"] != "step_up_required" {
		t.Fatalf("lapsed step-up: %d %v", code, out)
	}
	if len(f.drive.audiences) != 0 {
		t.Fatalf("KyDrive was asked without a step-up: %v", f.drive.audiences)
	}
	f.stepUp(time.Now().Add(-9 * time.Minute))
	code, out := guest(7)
	if code != 201 || out["url"] != f.drive.URL+"/g/#gtok-file-1" {
		t.Fatalf("guest link %d %v", code, out)
	}
	for name, tc := range map[string]struct {
		body string
		code string
	}{
		"zero days":    {`{"fileId":"file-1","days":0,"audience":"guest"}`, "bad_request"},
		"over the max": {`{"fileId":"file-1","days":15,"audience":"guest"}`, "bad_request"},
		"bad audience": {`{"fileId":"file-1","days":7,"audience":"public"}`, "bad_request"},
	} {
		if code, out := f.call(t, f.ann, "POST", "/api/kydrive/links", tc.body); code != 400 || out["code"] != tc.code {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	// An organisation link answered to a guest request never reaches the browser.
	f.drive.linkURL = f.drive.URL + "/l/#tok-file-1"
	if code, out := guest(7); code != 502 || out["url"] != nil {
		t.Fatalf("org link for a guest request: %d %v", code, out)
	}
	f.drive.linkURL = ""
	// Organisation links still send no audience, and refuse a guest-only day count.
	if code, _ := f.call(t, f.ann, "POST", "/api/kydrive/links", `{"fileId":"file-1","days":30}`); code != 201 {
		t.Fatalf("organisation link %d", code)
	}
	if code, _ := f.call(t, f.ann, "POST", "/api/kydrive/links", `{"fileId":"file-1","days":14}`); code != 400 {
		t.Fatalf("organisation link with 14 days: %d", code)
	}
	if want := []string{"guest", "guest", "-"}; strings.Join(f.drive.audiences, ",") != strings.Join(want, ",") {
		t.Fatalf("KyDrive saw audiences %v, want %v", f.drive.audiences, want)
	}
	if logs := f.logs.String(); strings.Contains(logs, "gtok-") || strings.Contains(logs, "plan.pdf") {
		t.Fatalf("logs carry a link or name: %s", logs)
	}
	// The policy or the user's switch turned off in KyDrive: passed through,
	// and the cached status is dropped so the option goes.
	f.drive.guestOff = true
	if code, out := guest(7); code != 409 || out["code"] != "guest_links_off" {
		t.Fatalf("guest links off in KyDrive: %d %v", code, out)
	}
	f.drive.guestOff = false
	f.drive.guestMax = nil
	if _, st := f.call(t, f.ann, "GET", "/api/kydrive/status", ""); st["guestLinks"] != nil {
		t.Fatalf("status still offers guest links: %v", st)
	}
	// KyDrive downgraded while a status still said yes: its organisation link is refused.
	f.drive.guestMax = &maxDays
	f.srv.driveStatuses.Clear()
	f.call(t, f.ann, "GET", "/api/kydrive/status", "")
	f.drive.guestMax = nil
	if code, out := guest(7); code != 502 || out["url"] != nil {
		t.Fatalf("downgraded KyDrive: %d %v", code, out)
	}
}

// An old KyDrive, whose status has no guest_links, never gets an audience:
// a guest request is refused here, as is any other user's or an unpaired one.
func TestKyDriveGuestLinksNeedANewKyDrive(t *testing.T) {
	f := newDriveFixture(t)
	f.drive.files["file-1"] = driveFile{owner: "sub-ann", name: "plan.pdf", data: []byte("x")}
	f.stepUp(time.Now())
	if _, st := f.call(t, f.ann, "GET", "/api/kydrive/status", ""); st["guestLinks"] != nil || st["guestMaxDays"] != nil {
		t.Fatalf("old KyDrive status offers guest links: %v", st)
	}
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/links", `{"fileId":"file-1","days":7,"audience":"guest"}`); code != 409 || out["code"] != "guest_links_off" {
		t.Fatalf("guest link from an old KyDrive: %d %v", code, out)
	}
	if len(f.drive.audiences) != 0 {
		t.Fatalf("an old KyDrive was sent a link request: %v", f.drive.audiences)
	}
	// guest_links:false, or a nonsense max, reads the same.
	zero := 0
	f.drive.guestMax = &zero
	f.srv.driveStatuses.Clear()
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/links", `{"fileId":"file-1","days":1,"audience":"guest"}`); code != 409 || out["code"] != "guest_links_off" {
		t.Fatalf("guest_max_days 0: %d %v", code, out)
	}
	f.srv.SetKyDrive(nil)
	if code, out := f.call(t, f.ann, "POST", "/api/kydrive/links", `{"fileId":"file-1","days":1,"audience":"guest"}`); code != 409 || out["code"] != "kydrive_off" {
		t.Fatalf("unpaired: %d %v", code, out)
	}
}
