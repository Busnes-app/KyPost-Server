package api

import (
	"archive/zip"
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// Self-service export of a user's own native mailbox as mboxrd or a zip of
// EML files. POST /api/export proves the account (step-up) and mints a
// single-use download grant bound to user, session, mailbox, folder and
// format; GET /api/export/{token} spends it and streams the file, so the
// browser downloads natively and the server holds one message at a time.

const (
	exportGrantTTL = 5 * time.Minute
	// maxExports bounds streaming downloads server-wide; each user has one.
	maxExports = 2
	// exportWriteWindow is how long the client has to take each message,
	// replacing the server-wide WriteTimeout that would cut a long download.
	exportWriteWindow = 2 * time.Minute
)

type exportGrant struct {
	user, session, mailbox, folder, format string
	expires                                time.Time
}

type mailExporter interface {
	ExportFolders(ctx context.Context, folder string) ([]string, error)
	Export(ctx context.Context, folders []string, fn func(mailbox.ExportMessage) error) error
}

// exporter admits the mailbox again and answers 409 for anything not native.
func (s *Server) exporter(w http.ResponseWriter, r *http.Request, userID, mailboxID string) (mailExporter, bool) {
	_, native, err := s.nativeMailboxAssignment(r.Context(), userID, mailboxID)
	if err != nil {
		if s.refuseNativeAdministrator(w, r, userID, err) {
			return nil, false
		}
		if errors.Is(err, sso.ErrNativeMailboxUnknown) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "mailbox not found"})
			return nil, false
		}
		http.Error(w, "mailbox authority is unavailable", http.StatusServiceUnavailable)
		return nil, false
	}
	if native {
		client, err := s.mailboxMailClient(userID, mailboxID)
		if err != nil {
			http.Error(w, "mailbox is unavailable", http.StatusServiceUnavailable)
			return nil, false
		}
		if ex, ok := client.(mailExporter); ok {
			return ex, true
		}
	}
	writeJSON(w, http.StatusConflict, map[string]any{"error": "export is available for native mailboxes; your provider offers its own export"})
	return nil, false
}

// exportFolders resolves the folders to export, writing 400/404/503.
func exportFolders(w http.ResponseWriter, r *http.Request, ex mailExporter, folder string) ([]string, bool) {
	folders, err := ex.ExportFolders(r.Context(), folder)
	switch {
	case err == nil:
		return folders, true
	case errors.Is(err, mailbox.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "folder not found"})
	case errors.Is(err, imapadapter.ErrUnsafeMailbox):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid folder"})
	default:
		http.Error(w, "mailbox is unavailable", http.StatusServiceUnavailable)
	}
	return nil, false
}

// handleExportFolders lists every folder of the selected mailbox.
func (s *Server) handleExportFolders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ac, _ := authFromContext(r)
	ex, ok := s.exporter(w, r, ac.UserID, ac.Mailbox)
	if !ok {
		return
	}
	if folders, ok := exportFolders(w, r, ex, ""); ok {
		writeJSON(w, http.StatusOK, map[string]any{"folders": folders})
	}
}

// handleExportStart proves the caller and mints the download grant. Only a
// browser session may export: the grant is bound to it.
func (s *Server) handleExportStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ac, _ := authFromContext(r)
	_, session, ok := s.sessionOf(r)
	if !ok || ac.DeviceID != "" {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "export needs a signed-in browser session"})
		return
	}
	var body struct {
		Mailbox    string `json:"mailbox"`
		Folder     string `json:"folder"`
		Format     string `json:"format"`
		Password   string `json:"password"`
		AuthSecret string `json:"authSecret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Format != "mbox" && body.Format != "eml-zip" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "choose a format: mbox or eml-zip"})
		return
	}
	r, ok = s.admitMailbox(w, r, body.Mailbox)
	if !ok {
		return
	}
	ac, _ = authFromContext(r)
	ex, ok := s.exporter(w, r, ac.UserID, ac.Mailbox)
	if !ok {
		return
	}
	folders, ok := exportFolders(w, r, ex, body.Folder)
	if !ok {
		return
	}
	if !s.confirmActor(w, r, ac.UserID, body.Password, body.AuthSecret) {
		return
	}
	token, err := sso.RandomToken(32)
	if err != nil {
		http.Error(w, "export unavailable", http.StatusInternalServerError)
		return
	}
	grant := exportGrant{user: ac.UserID, session: session, mailbox: ac.Mailbox, format: body.Format, expires: time.Now().Add(exportGrantTTL)}
	if body.Folder != "" {
		grant.folder = folders[0]
	}
	s.exportMu.Lock()
	if s.exports == nil {
		s.exports = map[string]exportGrant{}
	}
	// One unspent grant per user, and nothing expired kept around.
	for k, g := range s.exports {
		if g.user == ac.UserID || !time.Now().Before(g.expires) {
			delete(s.exports, k)
		}
	}
	s.exports[token] = grant
	s.exportMu.Unlock()
	s.auditExport(grant, "authorized", 0, 0)
	writeJSON(w, http.StatusOK, map[string]any{"url": "/api/export/" + token, "expiresInSeconds": int(exportGrantTTL.Seconds())})
}

// handleExportDownload spends a grant and streams the export. Unknown,
// expired, spent, foreign-session and foreign-user tokens are one 404; a busy
// slot answers 429 and leaves the grant for a retry.
func (s *Server) handleExportDownload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ac, _ := authFromContext(r)
	_, session, ok := s.sessionOf(r)
	token := r.PathValue("token")
	now := time.Now()
	s.exportMu.Lock()
	g, found := s.exports[token]
	if found && !now.Before(g.expires) {
		delete(s.exports, token)
	}
	if !ok || ac.DeviceID != "" || !found || g.user != ac.UserID || g.session != session || !now.Before(g.expires) {
		s.exportMu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "export link is unknown, expired or already used"})
		return
	}
	if s.exporting[g.user] || len(s.exporting) >= maxExports {
		s.exportMu.Unlock()
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "another export is running; try again shortly"})
		return
	}
	delete(s.exports, token)
	if s.exporting == nil {
		s.exporting = map[string]bool{}
	}
	s.exporting[g.user] = true
	s.exportMu.Unlock()
	defer s.endExport(g.user)

	ex, ok := s.exporter(w, r, g.user, g.mailbox)
	if !ok {
		return
	}
	folders, ok := exportFolders(w, r, ex, g.folder)
	if !ok {
		return
	}
	name, contentType := "kypost-mail-"+now.UTC().Format("20060102T150405Z"), "application/mbox"
	if g.format == "eml-zip" {
		name, contentType = name+".zip", "application/zip"
	} else {
		name += ".mbox"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	s.auditExport(g, "started", 0, 0)
	counted := &countingWriter{w: w}
	buffered := bufio.NewWriterSize(counted, 64<<10)
	var archive *zip.Writer
	if g.format == "eml-zip" {
		archive = zip.NewWriter(buffered)
	}
	rc := http.NewResponseController(w)
	messages := 0
	err := ex.Export(r.Context(), folders, func(m mailbox.ExportMessage) error {
		// Unsupported only on test writers; the server-wide timeout then applies.
		_ = rc.SetWriteDeadline(time.Now().Add(exportWriteWindow))
		messages++
		if archive != nil {
			return mailbox.WriteEML(archive, m)
		}
		return mailbox.WriteMboxrd(buffered, m)
	})
	if err == nil && archive != nil {
		err = archive.Close()
	}
	if err == nil {
		err = buffered.Flush()
	}
	if err != nil {
		s.auditExport(g, "failed", messages, counted.n)
		// Abort the connection: a clean end would pass a truncated mbox as whole.
		panic(http.ErrAbortHandler)
	}
	s.auditExport(g, "finished", messages, counted.n)
}

func (s *Server) endExport(user string) {
	s.exportMu.Lock()
	delete(s.exporting, user)
	s.exportMu.Unlock()
}

// auditExport records the export without correspondence: never subjects or addresses.
func (s *Server) auditExport(g exportGrant, result string, messages int, bytes int64) {
	s.logger.Info("mail export", "actor", g.user, "action", "mail_export", "mailbox", cmp.Or(g.mailbox, g.user), "folder", cmp.Or(g.folder, "(all)"), "format", g.format, "messages", strconv.Itoa(messages), "bytes", strconv.FormatInt(bytes, 10), "result", result)
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
