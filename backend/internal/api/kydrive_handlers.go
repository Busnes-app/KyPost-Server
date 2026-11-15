package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/mail"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/kydrive"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// KyDrive routes for the web client (docs/KYDRIVE.md). Every one is per-user
// rate limited and answers {"error","code"} refusals; 409 kydrive_off means
// "hide the KyDrive options".
const (
	kydriveBurst        = 30
	kydriveRefillPerSec = 0.5
	// kydriveStatusTTL is how long one user's KyDrive status is reused.
	kydriveStatusTTL = time.Minute
	// kydriveUploadBudget bounds a server-side save to KyDrive.
	kydriveUploadBudget = 10 * time.Minute
)

// SetKyDrive pairs KyDrive. Startup-only; nil or unpaired leaves it off.
func (s *Server) SetKyDrive(c *kydrive.Client) { s.kydrive = c }

func (s *Server) handleAdminKyDrive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.kydrive.Status())
}

func writeDrive(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]any{"error": msg, "code": code})
}

// driveStatus is what the web client needs to show or hide KyDrive.
type driveStatus struct {
	Available    bool   `json:"available"`
	FreeBytes    int64  `json:"freeBytes,omitempty"`
	MaxFileBytes int64  `json:"maxFileBytes,omitempty"`
	AppURL       string `json:"appUrl,omitempty"`
	// Origin is the paired KyDrive, so the client can find its links in a
	// body even while the user's own status is off.
	Origin string `json:"origin,omitempty"`
}

type driveStatusEntry struct {
	origin, sub string
	st          driveStatus
	until       time.Time
}

// driveUser admits the caller to a KyDrive route: rate limit, not an
// administrator identity, and (unless statusOnly) KyDrive available to them. It writes the refusal itself.
func (s *Server) driveUser(w http.ResponseWriter, r *http.Request, statusOnly bool) (users.User, driveStatus, bool) {
	ac, ok := authFromContext(r)
	if !ok {
		writeDrive(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return users.User{}, driveStatus{}, false
	}
	if ok, wait := s.kydriveLimiter.allow(ac.UserID); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeDrive(w, http.StatusTooManyRequests, "rate_limited", "too many KyDrive requests; try again shortly")
		return users.User{}, driveStatus{}, false
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		writeDrive(w, http.StatusServiceUnavailable, "unavailable", "account unavailable")
		return users.User{}, driveStatus{}, false
	}
	// Dedicated administrator identities (native mode: an administrator
	// without a legacy mixed-use mailbox) have no mailbox and no KyDrive here.
	_, native, err := s.nativeMailAssignment(r.Context(), u.ID)
	if s.nativeMail && u.Role == users.RoleAdmin && (!native || err != nil) {
		s.refuseNativeAdministrator(w, r, u.ID, sso.ErrNativeAdministrator)
		return users.User{}, driveStatus{}, false
	}
	st := s.driveStatusFor(r.Context(), u)
	if !st.Available && !statusOnly {
		s.logger.Info("kydrive request refused", "actor", u.ID, "action", r.Method, "target", r.Pattern, "result", "kydrive_off")
		writeDrive(w, http.StatusConflict, "kydrive_off", "KyDrive is not available for this account")
		return users.User{}, driveStatus{}, false
	}
	return u, st, true
}

// driveStatusFor follows the contract's "shown only when": paired, KyDrive in
// the user's suite app list, and KyDrive's status says enabled. Cached per
// user for a minute; a failure reads as unavailable.
func (s *Server) driveStatusFor(ctx context.Context, u users.User) driveStatus {
	origin := s.kydrive.Origin()
	dir, ok := s.ssoStore.Load().SuiteDirectory()
	if origin == "" || !ok || !u.Active || !linkedTo(u, dir.Issuer) {
		return driveStatus{}
	}
	if v, ok := s.driveStatuses.Load(u.ID); ok {
		if e := v.(driveStatusEntry); e.origin == origin && e.sub == u.SSOSub && time.Now().Before(e.until) {
			return e.st
		}
	}
	st := driveStatus{}
	for _, a := range s.suiteAppsFor(ctx, u.ID) {
		if a.ID == "kydrive" {
			st.AppURL = a.URL
		}
	}
	if st.AppURL != "" {
		status, body, err := s.drivePost(ctx, u, kydrive.PathStatus, kydrive.EventStatus, map[string]any{})
		var ans struct {
			Enabled      bool  `json:"enabled"`
			FreeBytes    int64 `json:"free_bytes"`
			MaxFileBytes int64 `json:"max_file_bytes"`
		}
		if err == nil && status == http.StatusOK && json.Unmarshal(body, &ans) == nil && ans.Enabled {
			st.Available, st.FreeBytes, st.MaxFileBytes = true, max(0, ans.FreeBytes), max(0, ans.MaxFileBytes)
		}
	}
	if ctx.Err() == nil {
		s.driveStatuses.Store(u.ID, driveStatusEntry{origin: origin, sub: u.SSOSub, st: st, until: time.Now().Add(kydriveStatusTTL)})
	}
	return st
}

// drivePost signs fields plus the user's subject and issuer to KyDrive with
// a fresh event ID; KyPost does not retry, the user does. Logs IDs only.
func (s *Server) drivePost(ctx context.Context, u users.User, path, eventType string, fields map[string]any) (int, []byte, error) {
	eventID, err := driveEvent(u, fields)
	if err != nil {
		return 0, nil, err
	}
	status, body, err := s.kydrive.Post(ctx, path, eventType, eventID, fields)
	if err != nil {
		s.logger.Warn("kydrive request failed", "actor", u.ID, "event_id", eventID, "action", eventType, "result", "network")
		return 0, nil, err
	}
	s.logger.Info("kydrive answered", "actor", u.ID, "event_id", eventID, "action", eventType, "result", strconv.Itoa(status)+" "+kydrive.Outcome(body))
	return status, body, nil
}

// driveEvent adds the user's subject and issuer to fields and returns a fresh event ID.
func driveEvent(u users.User, fields map[string]any) (string, error) {
	rid, err := fsutil.NewUUIDv4()
	fields["sub"], fields["issuer"] = u.SSOSub, u.SSOIssuer
	return "kypost-drive-" + rid, err
}

// driveErrors are KyDrive refusals the client can act on, passed through.
var driveErrors = map[string]string{
	"too_large":        "the file is larger than KyDrive accepts",
	"quota":            "your KyDrive is full",
	"bad_body":         "KyDrive refused that file name",
	"too_many_tickets": "too many uploads in progress; finish or wait for one",
	"sharing_off":      "KyPost is not allowed to save to your KyDrive; turn it on in KyDrive settings",
	"not_found":        "that file is no longer in your KyDrive",
	"no_user":          "you have no KyDrive account yet",
}

// writeDriveRefusal maps a KyDrive answer that was not the expected success.
func (s *Server) writeDriveRefusal(w http.ResponseWriter, status int, body []byte) {
	code := kydrive.Outcome(body)
	if code == "sharing_off" {
		// The user's switch changed: show or hide on the next status read.
		s.driveStatuses.Clear()
	}
	if msg := driveErrors[code]; msg != "" && (status < 500 || status == http.StatusInsufficientStorage) && status >= 400 {
		writeDrive(w, status, code, msg)
		return
	}
	writeDrive(w, http.StatusBadGateway, "kydrive_unavailable", "KyDrive could not be reached; try again")
}

func (s *Server) handleKyDriveStatus(w http.ResponseWriter, r *http.Request) {
	_, st, ok := s.driveUser(w, r, true)
	if !ok {
		return
	}
	st.Origin = s.kydrive.Origin()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, st)
}

// validDriveName is the boundary check on a file name; KyDrive applies its
// own upload name rules after it.
func validDriveName(name string) bool {
	return name != "" && len(name) <= 255 && utf8.ValidString(name) &&
		!strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '/' || r == '\\' })
}

// ticket asks KyDrive for an upload ticket and checks the answer: the URL
// must be on the paired KyDrive before it reaches anyone.
func (s *Server) ticket(ctx context.Context, u users.User, name string, size int64) (status int, body []byte, t driveTicket, err error) {
	status, body, err = s.drivePost(ctx, u, kydrive.PathTicket, kydrive.EventTicket, map[string]any{"name": name, "size": size})
	if err != nil || status != http.StatusCreated {
		return status, body, t, err
	}
	if json.Unmarshal(body, &t) != nil || !s.kydrive.OnOrigin(t.UploadURL, kydrive.UploadPrefix) || t.MaxChunk <= 0 {
		s.logger.Warn("kydrive ticket answer refused", "actor", u.ID, "result", "bad_answer")
		return 0, nil, driveTicket{}, errors.New("kydrive: unusable ticket answer")
	}
	return status, body, t, nil
}

type driveTicket struct {
	UploadURL string `json:"upload_url"`
	Expires   string `json:"expires"`
	MaxChunk  int64  `json:"max_chunk"`
}

// handleKyDriveTicket hands the signed-in browser an upload ticket. The URL
// is a credential: it goes in this answer and nowhere else.
func (s *Server) handleKyDriveTicket(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.driveUser(w, r, false)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
		Size *int64 `json:"size"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req) != nil || !validDriveName(req.Name) || req.Size == nil || *req.Size < 0 {
		writeDrive(w, http.StatusBadRequest, "bad_request", "a file name and size are required")
		return
	}
	status, body, t, err := s.ticket(r.Context(), u, req.Name, *req.Size)
	if err != nil {
		writeDrive(w, http.StatusBadGateway, "kydrive_unavailable", "KyDrive could not be reached; try again")
		return
	}
	if status != http.StatusCreated {
		s.writeDriveRefusal(w, status, body)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"uploadUrl": t.UploadURL, "expires": t.Expires, "maxChunk": t.MaxChunk})
}

func validDriveFileID(id string) bool {
	return id != "" && len(id) <= 128 && !strings.ContainsFunc(id, func(r rune) bool { return r <= 0x20 || r >= 0x7f })
}

// handleKyDriveLink makes an organisation link to a file the user uploaded.
func (s *Server) handleKyDriveLink(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.driveUser(w, r, false)
	if !ok {
		return
	}
	var req struct {
		FileID string `json:"fileId"`
		Days   *int   `json:"days"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req) != nil || !validDriveFileID(req.FileID) {
		writeDrive(w, http.StatusBadRequest, "bad_request", "a file id is required")
		return
	}
	days := 30
	if req.Days != nil {
		days = *req.Days
	}
	if days != 0 && days != 1 && days != 7 && days != 30 {
		writeDrive(w, http.StatusBadRequest, "bad_request", "days must be 0, 1, 7 or 30")
		return
	}
	status, body, err := s.drivePost(r.Context(), u, kydrive.PathLink, kydrive.EventLink, map[string]any{"file_id": req.FileID, "days": days})
	if err != nil {
		writeDrive(w, http.StatusBadGateway, "kydrive_unavailable", "KyDrive could not be reached; try again")
		return
	}
	if status != http.StatusCreated {
		s.writeDriveRefusal(w, status, body)
		return
	}
	var ans struct {
		URL     string `json:"url"`
		Expires string `json:"expires"`
	}
	if json.Unmarshal(body, &ans) != nil || !s.kydrive.OnOrigin(ans.URL, kydrive.LinkPrefix) {
		s.logger.Warn("kydrive link answer refused", "actor", u.ID, "result", "bad_answer")
		writeDrive(w, http.StatusBadGateway, "kydrive_unavailable", "KyDrive gave an unusable link")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"url": ans.URL, "expires": ans.Expires})
}

// handleKyDriveSaveAttachment copies one attachment of a message in the
// caller's mailbox to their KyDrive. The part is read server-side (the mail
// parser already holds it, within the 25 MiB inbound cap) and sent to a
// ticket straight from that copy; the client names it by index only.
func (s *Server) handleKyDriveSaveAttachment(w http.ResponseWriter, r *http.Request) {
	u, st, ok := s.driveUser(w, r, false)
	if !ok {
		return
	}
	var req struct {
		MessageID string `json:"messageId"`
		Mailbox   string `json:"mailbox"`
		Index     int    `json:"index"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req) != nil || req.Index < 0 {
		writeDrive(w, http.StatusBadRequest, "bad_request", "invalid request")
		return
	}
	if req.Mailbox != "" && imapadapter.ValidateMailboxName(req.Mailbox) != nil {
		writeDrive(w, http.StatusBadRequest, "bad_request", "invalid mailbox")
		return
	}
	mailClient, err := s.mailFor(r)
	if err != nil {
		writeDrive(w, http.StatusServiceUnavailable, "mailbox_unavailable", "could not open the mailbox")
		return
	}
	id, err := imapadapter.ResolveMessageReference(mailClient, strings.TrimSpace(req.MessageID))
	uid, convErr := strconv.Atoi(id)
	if err != nil || convErr != nil || uid <= 0 {
		writeDrive(w, http.StatusBadRequest, "bad_request", "invalid messageId; refresh the mailbox")
		return
	}
	info, content, err := mailClient.GetAttachment(r.Context(), req.Mailbox, uid, req.Index)
	if errors.Is(err, imapadapter.ErrAttachmentNotFound) {
		writeDrive(w, http.StatusNotFound, "not_found", "attachment not found")
		return
	}
	if err != nil {
		writeDrive(w, http.StatusBadGateway, "mailbox_unavailable", "could not read the attachment")
		return
	}
	name := mailmsg.SanitizeHeaderValue(info.Name)
	if !validDriveName(name) {
		name = "attachment"
	}
	size := int64(len(content))
	status, body, t, err := s.ticket(r.Context(), u, name, size)
	if err != nil {
		writeDrive(w, http.StatusBadGateway, "kydrive_unavailable", "KyDrive could not be reached; try again")
		return
	}
	if status != http.StatusCreated {
		s.writeDriveRefusal(w, status, body)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), kydriveUploadBudget)
	defer cancel()
	status, body, err = s.kydrive.Upload(ctx, t.UploadURL, bytes.NewReader(content), size, t.MaxChunk)
	var ans struct {
		FileID string `json:"file_id"`
		Name   string `json:"name"`
		Size   int64  `json:"size"`
	}
	if err != nil || status != http.StatusCreated || json.Unmarshal(body, &ans) != nil || ans.FileID == "" {
		s.logger.Warn("kydrive save failed", "actor", u.ID, "result", strconv.Itoa(status)+" "+kydrive.Outcome(body))
		if err == nil && status != http.StatusCreated {
			s.writeDriveRefusal(w, status, body)
			return
		}
		writeDrive(w, http.StatusBadGateway, "kydrive_unavailable", "KyDrive did not accept the file; try again")
		return
	}
	s.logger.Info("kydrive save completed", "actor", u.ID, "target", ans.FileID, "result", "saved")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"fileId": ans.FileID, "name": ans.Name, "size": ans.Size, "folder": "Mail attachments", "appUrl": st.AppURL})
}

// kydriveFetchBudget bounds one server-side fetch of a picked file.
const kydriveFetchBudget = 2 * time.Minute

// handleKyDriveCopy fetches a file the user picked in KyDrive's picker
// ("Attach a copy", contract I2) and hands it to compose as an attachment.
// KyDrive refuses anything over the attachment limit, and so does this.
func (s *Server) handleKyDriveCopy(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.driveUser(w, r, false)
	if !ok {
		return
	}
	var req struct {
		FileID string `json:"fileId"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req) != nil || !validDriveFileID(req.FileID) {
		writeDrive(w, http.StatusBadRequest, "bad_request", "a file id is required")
		return
	}
	fields := map[string]any{"file_id": req.FileID, "max_bytes": maxMailAttachmentBytes}
	eventID, err := driveEvent(u, fields)
	if err != nil {
		writeDrive(w, http.StatusServiceUnavailable, "unavailable", "try again")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), kydriveFetchBudget)
	defer cancel()
	status, header, body, err := s.kydrive.Fetch(ctx, eventID, fields)
	if err != nil {
		s.logger.Warn("kydrive request failed", "actor", u.ID, "event_id", eventID, "action", kydrive.EventFetch, "result", "network")
		writeDrive(w, http.StatusBadGateway, "kydrive_unavailable", "KyDrive could not be reached; try again")
		return
	}
	s.logger.Info("kydrive answered", "actor", u.ID, "event_id", eventID, "action", kydrive.EventFetch, "result", strconv.Itoa(status))
	if status == http.StatusRequestEntityTooLarge && kydrive.Outcome(body) == "too_large" {
		writeDrive(w, status, "too_large", "the file is over the attachment limit; share a link instead")
		return
	}
	if status != http.StatusOK {
		s.writeDriveRefusal(w, status, body)
		return
	}
	ctype, _, _ := mime.ParseMediaType(header.Get("Content-Type"))
	if ctype != "application/octet-stream" || len(body) > maxMailAttachmentBytes {
		s.logger.Warn("kydrive fetch answer refused", "actor", u.ID, "event_id", eventID, "result", "bad_answer")
		writeDrive(w, http.StatusBadGateway, "kydrive_unavailable", "KyDrive gave an unusable file")
		return
	}
	name := "attachment"
	if _, params, err := mime.ParseMediaType(header.Get("Content-Disposition")); err == nil {
		if n := mailmsg.SanitizeHeaderValue(params["filename"]); validDriveName(n) {
			name = n
		}
	}
	mimeType := cmp.Or(mime.TypeByExtension(filepath.Ext(name)), "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "mimeType": mimeType, "size": len(body), "dataBase64": base64.StdEncoding.EncodeToString(body)})
}

// maxDriveRecipients bounds one outside-recipient check (the send route's own cap).
const maxDriveRecipients = 500

// handleKyDriveRecipients answers which addresses are not people in the
// organisation (suitedir.Resolve), so cannot open an organisation link. The
// client asks before sending a message that carries KyDrive links.
func (s *Server) handleKyDriveRecipients(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.driveUser(w, r, true)
	if !ok {
		return
	}
	var req struct {
		Addresses []string `json:"addresses"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req) != nil || len(req.Addresses) == 0 || len(req.Addresses) > maxDriveRecipients {
		writeDrive(w, http.StatusBadRequest, "bad_request", "1 to 500 addresses are required")
		return
	}
	var addrs []string
	for _, raw := range req.Addresses {
		a, err := mail.ParseAddress(raw)
		if err != nil || a.Name != "" || a.Address != strings.TrimSpace(raw) {
			writeDrive(w, http.StatusBadRequest, "bad_request", "addresses must be bare addresses")
			return
		}
		if addr := strings.ToLower(a.Address); !slices.Contains(addrs, addr) {
			addrs = append(addrs, addr)
		}
	}
	dir, ok := s.ssoStore.Load().SuiteDirectory()
	if !ok || !linkedTo(u, dir.Issuer) {
		writeDrive(w, http.StatusConflict, "kydrive_off", "this account cannot look up organisation members")
		return
	}
	known := map[string]bool{}
	for chunk := range slices.Chunk(addrs, 100) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		people, err := dir.Resolve(ctx, u.SSOSub, chunk)
		cancel()
		if err != nil {
			// suitedir errors carry neither the query (addresses, sub) nor the credentials.
			s.logger.Warn("kydrive recipient check failed", "actor", u.ID, "error", err.Error())
			writeDrive(w, http.StatusServiceUnavailable, "unavailable", "could not check the recipients; try again")
			return
		}
		for _, p := range people {
			known[strings.ToLower(p.Email)] = true
		}
	}
	outside := []string{}
	for _, a := range addrs {
		if !known[a] {
			outside = append(outside, a)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"outside": outside})
}
