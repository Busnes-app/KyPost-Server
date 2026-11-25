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
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
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
	User        string    `json:"user"`
	Session     string    `json:"session"` // sessionHash of the session it was issued to
	Mailbox     string    `json:"mailbox"`
	Folder      string    `json:"folder"`
	Format      string    `json:"format"`
	Correlation string    `json:"correlation"`
	Expires     time.Time `json:"expires"`
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
	if !s.confirmActor(w, r, ac.UserID, body.AuthSecret) {
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
	grant := exportGrant{User: ac.UserID, Session: sessionHash(session), Mailbox: ac.Mailbox, Format: body.Format, Correlation: correlation, Expires: time.Now().Add(exportGrantTTL)}
	if body.Folder != "" {
		grant.Folder = folders[0]
	}
	if err := s.putExportGrant(token, grant); err != nil {
		writeUnavailable(w)
		return
	}
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
	// Taken before the grant is spent, so a busy org keeps its link for a retry.
	release, status, _ := s.takeOrgTransfer(ac.UserID)
	switch {
	case status == http.StatusConflict:
		back("busy&retry=" + url.QueryEscape(token))
		return
	case status != 0:
		back("unavailable")
		return
	}
	defer release()
	g, refusal := s.claimExportGrant(token, ac.UserID, ok, sessionHash(session), r.ProtoAtLeast(1, 1))
	if refusal != "" {
		back(refusal)
		return
	}
	defer s.endExport(g.User)

	ex, refused := s.exporter(r.Context(), g.User, g.Mailbox)
	var folders []string
	if refused == nil {
		folders, refused = exportFolders(r.Context(), ex, g.Folder)
	}
	if refused != nil {
		s.auditExport(g, "refused", 0, 0)
		back("unavailable")
		return
	}
	name, contentType := "kypost-mail-"+time.Now().UTC().Format("20060102T150405Z"), "application/mbox"
	if g.Format == "eml-zip" {
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
	if g.Format == "eml-zip" {
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
	slog.New(s.logger.Handler()).Info("mail export", "actor", g.User, "action", "mail_export", "target", cmp.Or(g.Mailbox, g.User), "folder", cmp.Or(logging.Escape(g.Folder), "(all)"), "format", logging.Escape(g.Format), "messages", int64(messages), "bytes", bytes, "result", result, "correlation_id", g.Correlation)
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

// putExportGrant records the one unspent export grant of its user, dropping expired ones.
func (s *Server) putExportGrant(token string, g exportGrant) error {
	if s.shared != nil {
		return putRow(s.shared, "export", token, g.User, g, g.Expires, true)
	}
	s.exportMu.Lock()
	defer s.exportMu.Unlock()
	if s.exports == nil {
		s.exports = map[string]exportGrant{}
	}
	for k, old := range s.exports {
		if old.User == g.User || !time.Now().Before(old.Expires) {
			delete(s.exports, k)
		}
	}
	s.exports[token] = g
	return nil
}

// claimExportGrant spends token's grant for user's session and takes this process's export
// slot, or names the refusal: expired, proxy (mbox needs HTTP/1.1), busy (the grant is kept
// for a retry). On PostgreSQL the spend is a compare-and-delete, so a grant downloads once
// across replicas; the slots stay per process.
func (s *Server) claimExportGrant(token, user string, signedIn bool, session string, http11 bool) (exportGrant, string) {
	now := time.Now()
	s.exportMu.Lock()
	defer s.exportMu.Unlock()
	var g exportGrant
	refusal := "expired"
	edit := func(v *exportGrant, found bool) (rowEdit, bool) {
		switch {
		case found && !now.Before(v.Expires):
			return rowDrop, false
		case !signedIn || !found || v.User != user || v.Session != session:
			return rowKeep, false
		case v.Format == "mbox" && !http11:
			refusal = "proxy"
			return rowKeep, false
		case s.exporting[v.User] || len(s.exporting) >= maxExports:
			refusal = "busy&retry=" + url.QueryEscape(token)
			return rowKeep, false
		}
		g = *v
		return rowDrop, true
	}
	var spent bool
	if s.shared != nil {
		spent = editRow(s.shared, "export", token, edit)
	} else {
		v, found := s.exports[token]
		action, ok := edit(&v, found)
		if action == rowDrop {
			delete(s.exports, token)
		}
		spent = ok
	}
	if !spent {
		return exportGrant{}, refusal
	}
	if s.exporting == nil {
		s.exporting = map[string]bool{}
	}
	s.exporting[g.User] = true
	return g, ""
}
