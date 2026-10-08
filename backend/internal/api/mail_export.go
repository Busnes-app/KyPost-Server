package api

import (
	"archive/zip"
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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
	// exportSettingsPage is where a failed download sends the browser back.
	exportSettingsPage = "/settings/security?tab=export&export="
	// HTTP/1.0 has no chunking: without a Content-Length an aborted mbox
	// ends like a complete one.
	exportHTTP10 = "mbox export needs HTTP/1.1 between the proxy and KyPost (nginx: proxy_http_version 1.1), where a cut-off download is detectable; or choose EML zip"
)

// exportWriteWindow is the time a reader gets for one message: 30 seconds
// plus the message at 64 KiB/s, so a 25 MiB message fits a slow link while a
// stalled reader loses its slot.
func exportWriteWindow(size int) time.Duration {
	return 30*time.Second + time.Duration(size)*time.Second/(64<<10)
}

type exportGrant struct {
	user, session, mailbox, folder, format, correlation string
	expires                                             time.Time
}

type mailExporter interface {
	ExportFolders(ctx context.Context, folder string) ([]string, error)
	ExportCount(ctx context.Context, folders []string) (int, error)
	Export(ctx context.Context, folders []string, fn func(mailbox.ExportMessage) error) error
}

type exportRefusal struct {
	status int
	error  string
}

func (e *exportRefusal) write(w http.ResponseWriter) {
	body := map[string]any{"error": e.error}
	if e.status == http.StatusForbidden {
		body["administratorIdentity"] = true
	}
	writeJSON(w, e.status, body)
}

// exporter admits the mailbox again; anything not native is 409.
func (s *Server) exporter(ctx context.Context, userID, mailboxID string) (mailExporter, *exportRefusal) {
	return nativeMail[mailExporter](s, ctx, userID, mailboxID, "export is available for native mailboxes; your provider offers its own export")
}

// nativeMail admits the mailbox again and returns its native client as T;
// anything not native is 409 with notNative.
func nativeMail[T any](s *Server, ctx context.Context, userID, mailboxID, notNative string) (T, *exportRefusal) {
	var none T
	_, native, err := s.nativeMailboxAssignment(ctx, userID, mailboxID)
	switch {
	case errors.Is(err, sso.ErrNativeAdministrator):
		return none, &exportRefusal{http.StatusForbidden, "administrator identities have no mailbox; use your everyday identity"}
	case errors.Is(err, sso.ErrNativeMailboxUnknown):
		return none, &exportRefusal{http.StatusNotFound, "mailbox not found"}
	case err != nil:
		return none, &exportRefusal{http.StatusServiceUnavailable, "mailbox authority is unavailable"}
	case native:
		client, err := s.mailboxMailClient(userID, mailboxID)
		if err != nil {
			return none, &exportRefusal{http.StatusServiceUnavailable, "mailbox is unavailable"}
		}
		if c, ok := client.(T); ok {
			return c, nil
		}
	}
	return none, &exportRefusal{http.StatusConflict, notNative}
}

// exportFolders resolves the folders to export.
func exportFolders(ctx context.Context, ex mailExporter, folder string) ([]string, *exportRefusal) {
	folders, err := ex.ExportFolders(ctx, folder)
	switch {
	case err == nil:
		return folders, nil
	case errors.Is(err, mailbox.ErrNotFound):
		return nil, &exportRefusal{http.StatusNotFound, "folder not found"}
	case errors.Is(err, imapadapter.ErrUnsafeMailbox):
		return nil, &exportRefusal{http.StatusBadRequest, "invalid folder"}
	}
	return nil, &exportRefusal{http.StatusServiceUnavailable, "mailbox is unavailable"}
}

// handleExportFolders lists every folder of the selected mailbox.
func (s *Server) handleExportFolders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ac, _ := authFromContext(r)
	ex, refused := s.exporter(r.Context(), ac.UserID, ac.Mailbox)
	if refused != nil {
		refused.write(w)
		return
	}
	folders, refused := exportFolders(r.Context(), ex, "")
	if refused != nil {
		refused.write(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"folders": folders})
}

// handleExportStart proves the caller and mints the download grant. Only a
// browser session may export: the grant is bound to it.
func (s *Server) handleExportStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_, session, ok := s.sessionOf(r)
	if !ok { // a device credential has no session
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
	if body.Format == "mbox" && !r.ProtoAtLeast(1, 1) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": exportHTTP10})
		return
	}
	r, ok = s.admitMailbox(w, r, body.Mailbox)
	if !ok {
		return
	}
	ac, _ := authFromContext(r)
	ex, refused := s.exporter(r.Context(), ac.UserID, ac.Mailbox)
	if refused != nil {
		refused.write(w)
		return
	}
	folders, refused := exportFolders(r.Context(), ex, body.Folder)
	if refused != nil {
		refused.write(w)
		return
	}
	if !s.confirmActor(w, r, ac.UserID, body.Password, body.AuthSecret) {
		return
	}
	count, err := ex.ExportCount(r.Context(), folders)
	if err != nil {
		http.Error(w, "mailbox is unavailable", http.StatusServiceUnavailable)
		return
	}
	token, err := sso.RandomToken(32)
	if err != nil {
		http.Error(w, "export unavailable", http.StatusInternalServerError)
		return
	}
	correlation, err := sso.RandomToken(8)
	if err != nil {
		http.Error(w, "export unavailable", http.StatusInternalServerError)
		return
	}
	grant := exportGrant{user: ac.UserID, session: session, mailbox: ac.Mailbox, format: body.Format, correlation: correlation, expires: time.Now().Add(exportGrantTTL)}
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
	s.auditExport(grant, "authorized", count, 0)
	writeJSON(w, http.StatusOK, map[string]any{"url": "/api/export/" + token, "expiresInSeconds": int(exportGrantTTL.Seconds()), "messages": count})
}

// handleExportDownload spends a grant and streams the export. The browser
// navigated here, so a refusal sends it back to the export page with a code:
// expired (unknown, expired, spent, other session or user), busy (slot taken;
// the grant is kept and retry names it), proxy (mbox over HTTP/1.0; kept too)
// or unavailable.
func (s *Server) handleExportDownload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	back := func(code string) { http.Redirect(w, r, exportSettingsPage+code, http.StatusSeeOther) }
	ac, _ := authFromContext(r)
	_, session, ok := s.sessionOf(r)
	token := r.PathValue("token")
	now := time.Now()
	s.exportMu.Lock()
	g, found := s.exports[token]
	if found && !now.Before(g.expires) {
		delete(s.exports, token)
	}
	if !ok || !found || g.user != ac.UserID || g.session != session || !now.Before(g.expires) {
		s.exportMu.Unlock()
		back("expired")
		return
	}
	if g.format == "mbox" && !r.ProtoAtLeast(1, 1) {
		s.exportMu.Unlock()
		back("proxy")
		return
	}
	if s.exporting[g.user] || len(s.exporting) >= maxExports {
		s.exportMu.Unlock()
		back("busy&retry=" + url.QueryEscape(token))
		return
	}
	delete(s.exports, token)
	if s.exporting == nil {
		s.exporting = map[string]bool{}
	}
	s.exporting[g.user] = true
	s.exportMu.Unlock()
	defer s.endExport(g.user)

	ex, refused := s.exporter(r.Context(), g.user, g.mailbox)
	var folders []string
	if refused == nil {
		folders, refused = exportFolders(r.Context(), ex, g.folder)
	}
	if refused != nil {
		s.auditExport(g, "refused", 0, 0)
		back("unavailable")
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
		_ = rc.SetWriteDeadline(time.Now().Add(exportWriteWindow(len(m.Raw))))
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
		// Abort the connection: the client sees an unterminated chunked body.
		panic(http.ErrAbortHandler)
	}
	s.auditExport(g, "finished", messages, counted.n)
}

func (s *Server) endExport(user string) {
	s.exportMu.Lock()
	delete(s.exporting, user)
	s.exportMu.Unlock()
}

// auditExport records the export without correspondence: never subjects or
// addresses. The correlation ID is random per grant, never the token.
func (s *Server) auditExport(g exportGrant, result string, messages int, bytes int64) {
	slog.New(s.logger.Handler()).Info("mail export", "actor", g.user, "action", "mail_export", "target", cmp.Or(g.mailbox, g.user), "folder", cmp.Or(g.folder, "(all)"), "format", g.format, "messages", int64(messages), "bytes", bytes, "result", result, "correlation_id", g.correlation)
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
