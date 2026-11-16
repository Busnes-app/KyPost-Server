package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/kydrive"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// Open in Euro-Office (docs/KYDRIVE.md "Open and Edit in KyDrive"): a received
// Office or PDF attachment goes to KyDrive as a read-only viewing copy, or with
// save-attachment's edit as a saved file opened in its editor. Never for an
// encrypted message: its plaintext must not reach a server-side editor.

// officeTypes is each viewable extension's declared content types.
// application/octet-stream ("unspecified") is accepted for every one; KyDrive
// also checks the bytes.
var officeTypes = map[string][]string{
	"docx": {"application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
	"xlsx": {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
	"pptx": {"application/vnd.openxmlformats-officedocument.presentationml.presentation"},
	"odt":  {"application/vnd.oasis.opendocument.text"},
	"ods":  {"application/vnd.oasis.opendocument.spreadsheet"},
	"odp":  {"application/vnd.oasis.opendocument.presentation"},
	"csv":  {"text/csv", "application/csv", "text/x-csv", "text/comma-separated-values", "application/vnd.ms-excel"},
	"pdf":  {"application/pdf", "application/x-pdf"},
}

// officeViewable is whether name's extension and declared type agree on a
// type Euro-Office can show.
func officeViewable(name, contentType string) bool {
	types := officeTypes[strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))]
	mediaType, _, err := mime.ParseMediaType(contentType)
	return types != nil && err == nil && (mediaType == "application/octet-stream" || slices.Contains(types, mediaType))
}

type driveAttachmentRequest struct {
	MessageID string `json:"messageId"`
	Mailbox   string `json:"mailbox"`
	Index     int    `json:"index"`
	// Edit (save-attachment only) also opens the saved file in KyDrive's editor.
	Edit bool `json:"edit"`
	mail imapadapter.Client
	uid  int
}

// driveAttachment reads the attachment a KyDrive route names, from the
// caller's own mailbox only. It writes the refusal itself.
func (s *Server) driveAttachment(w http.ResponseWriter, r *http.Request) (driveAttachmentRequest, imapadapter.AttachmentInfo, []byte, bool) {
	var req driveAttachmentRequest
	if json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req) != nil || req.Index < 0 {
		writeDrive(w, http.StatusBadRequest, "bad_request", "invalid request")
		return req, imapadapter.AttachmentInfo{}, nil, false
	}
	if req.Mailbox != "" && imapadapter.ValidateMailboxName(req.Mailbox) != nil {
		writeDrive(w, http.StatusBadRequest, "bad_request", "invalid mailbox")
		return req, imapadapter.AttachmentInfo{}, nil, false
	}
	mailClient, err := s.mailFor(r)
	if err != nil {
		writeDrive(w, http.StatusServiceUnavailable, "mailbox_unavailable", "could not open the mailbox")
		return req, imapadapter.AttachmentInfo{}, nil, false
	}
	id, err := imapadapter.ResolveMessageReference(mailClient, strings.TrimSpace(req.MessageID))
	uid, convErr := strconv.Atoi(id)
	if err != nil || convErr != nil || uid <= 0 {
		writeDrive(w, http.StatusBadRequest, "bad_request", "invalid messageId; refresh the mailbox")
		return req, imapadapter.AttachmentInfo{}, nil, false
	}
	req.mail, req.uid = mailClient, uid
	info, content, err := mailClient.GetAttachment(r.Context(), req.Mailbox, uid, req.Index)
	if errors.Is(err, imapadapter.ErrAttachmentNotFound) {
		writeDrive(w, http.StatusNotFound, "not_found", "attachment not found")
		return req, info, nil, false
	}
	if err != nil {
		writeDrive(w, http.StatusBadGateway, "mailbox_unavailable", "could not read the attachment")
		return req, info, nil, false
	}
	return req, info, content, true
}

// officeAttachment admits an attachment to Euro-Office: KyDrive has a viewer,
// the type is viewable, it is within the save-attachment limit, and the
// message is not encrypted by any verdict rawEncrypted reaches. It writes the
// refusal itself.
func (s *Server) officeAttachment(w http.ResponseWriter, r *http.Request, u users.User, st driveStatus, req driveAttachmentRequest, info imapadapter.AttachmentInfo, content []byte) bool {
	refuse := func(status int, code, msg string) bool {
		s.logger.Info("kydrive office refused", "actor", u.ID, "action", r.Pattern, "result", code)
		writeDrive(w, status, code, msg)
		return false
	}
	if !st.Viewer {
		return refuse(http.StatusConflict, "editor_off", "KyDrive has no document viewer")
	}
	if !officeViewable(info.Name, info.MimeType) {
		return refuse(http.StatusUnsupportedMediaType, "unsupported_type", "this attachment cannot be opened in KyDrive")
	}
	if limit := min(st.MaxFileBytes, mailmsg.MaxInboundMessageBytes); int64(len(content)) > limit || st.MaxFileBytes <= 0 {
		return refuse(http.StatusRequestEntityTooLarge, "too_large", "the file is larger than KyDrive accepts")
	}
	if s.messageProtected(r.Context(), req.mail, req.Mailbox, req.uid) {
		return refuse(http.StatusConflict, "encrypted", "attachments of encrypted mail cannot be opened in KyDrive")
	}
	return true
}

// messageProtected is the agent tools' encryption verdict (rawEncrypted) on
// the whole raw message; a message that cannot be read counts as protected.
func (s *Server) messageProtected(ctx context.Context, client imapadapter.Client, mailbox string, uid int) bool {
	raw, err := client.FetchRawMessage(ctx, mailbox, uid)
	return err != nil || rawEncrypted(raw)
}

// handleKyDriveViewAttachment sends one attachment to KyDrive as a viewing
// copy and answers the KyDrive page that shows it.
func (s *Server) handleKyDriveViewAttachment(w http.ResponseWriter, r *http.Request) {
	u, st, ok := s.driveUser(w, r, false)
	if !ok {
		return
	}
	req, info, content, ok := s.driveAttachment(w, r)
	if !ok || !s.officeAttachment(w, r, u, st, req, info, content) {
		return
	}
	name := mailmsg.SanitizeHeaderValue(info.Name)
	if !validDriveName(name) {
		// Keep the extension: KyDrive and Euro-Office read the type from it.
		name = "attachment" + strings.ToLower(filepath.Ext(name))
	}
	status, body, err := s.drivePost(r.Context(), u, kydrive.PathView, kydrive.EventView, map[string]any{"name": name, "size": len(content)})
	var t struct {
		UploadURL string `json:"upload_url"`
	}
	if err == nil && status == http.StatusCreated && (json.Unmarshal(body, &t) != nil || !s.kydrive.OnOrigin(t.UploadURL, kydrive.ViewUploadPrefix)) {
		s.logger.Warn("kydrive view answer refused", "actor", u.ID, "result", "bad_answer")
		err = errors.New("kydrive: unusable view ticket")
	}
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
	status, body, err = s.kydrive.UploadView(ctx, t.UploadURL, content)
	var ans struct {
		ViewURL string `json:"view_url"`
	}
	if err == nil && status == http.StatusCreated && (json.Unmarshal(body, &ans) != nil || !s.viewURL(ans.ViewURL)) {
		err = errors.New("kydrive: unusable view answer")
	}
	if err != nil || status != http.StatusCreated {
		s.logger.Warn("kydrive attachment open failed", "actor", u.ID, "action", "open", "result", strconv.Itoa(status)+" "+kydrive.Outcome(body))
		if err == nil {
			s.writeDriveRefusal(w, status, body)
			return
		}
		writeDrive(w, http.StatusBadGateway, "kydrive_unavailable", "KyDrive did not accept the file; try again")
		return
	}
	s.logger.Info("kydrive attachment opened", "actor", u.ID, "action", "open", "result", "viewing_copy")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"viewUrl": ans.ViewURL})
}

// viewURL is exactly https://<paired KyDrive>/view.html?copy=<id>.
func (s *Server) viewURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && s.kydrive.OnOrigin(raw, "/view") && u.Path == "/view.html" && u.Fragment == "" &&
		len(u.Query()) == 1 && validDriveFileID(u.Query().Get("copy"))
}
