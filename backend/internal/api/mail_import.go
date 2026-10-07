package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// Self-service import of mbox and EML files into the user's own native
// mailbox. POST /api/import proves the account (step-up) and mints a
// single-use upload link bound to user, session, mailbox and folder; the
// upload streams to a temp file and a background job stores the messages.
// A job lost to a restart leaves only its temp file, removed at startup;
// re-importing is safe because duplicates are skipped.

const (
	importGrantTTL = 5 * time.Minute
	// maxImports bounds uploading and running imports server-wide; each user has one.
	maxImports = 2
	// importIdle is how long an upload may send nothing before it is cut off.
	importIdle = time.Minute
	importDir  = "imports"
)

// importUploadCap is the largest upload: the mailbox's whole storage quota,
// since nothing larger can fit. A zip may expand to the same size.
var importUploadCap = nativeMailboxLimits.PayloadBytes

var errImportIncomingEncryption = errors.New("import is unavailable while incoming encryption is on or a replacement is pending: imported mail would be stored unencrypted")

type mailImporter interface {
	ImportFolder(ctx context.Context, folder string) (string, error)
	ImportMessage(ctx context.Context, folder string, raw []byte) error
}

type importGrant struct {
	user, session, mailbox, folder, correlation string
	expires                                     time.Time
}

// importJob is a user's latest import; the JSON fields are its status.
type importJob struct {
	State      string `json:"state"` // idle, uploading, running, finished, failed, cancelled
	Mailbox    string `json:"mailbox"`
	Folder     string `json:"folder"`
	Imported   int    `json:"imported"`
	Duplicates int    `json:"duplicates"`
	Skipped    int    `json:"skipped"`
	Bytes      int64  `json:"bytes"`
	Error      string `json:"error,omitempty"`

	user, correlation string
	cancel            context.CancelFunc
}

func (j *importJob) active() bool { return j.State == "uploading" || j.State == "running" }

// importBusy names why userID cannot start an import now. Hold importMu.
func (s *Server) importBusy(userID string) string {
	active := 0
	for user, j := range s.imports {
		if j.active() {
			if user == userID {
				return "an import is already running; wait for it to finish or cancel it"
			}
			active++
		}
	}
	if active >= maxImports {
		return "the server is busy with other imports; try again in a few minutes"
	}
	return ""
}

// importEncryptionOff refuses while the user's incoming encryption is on or
// pending: imported mail is seen and never swept, so it would stay plaintext.
func (s *Server) importEncryptionOff(userID string) error {
	u, err := s.users.Get(userID)
	if err != nil {
		return err
	}
	if err = s.refuseExtraBesideIncomingEncryption(u); errors.Is(err, errExtraMailboxIncomingEncryption) {
		return errImportIncomingEncryption
	}
	return err
}

func (s *Server) handleImportStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ac, _ := authFromContext(r)
	s.importMu.Lock()
	status := importJob{State: "idle"}
	if j := s.imports[ac.UserID]; j != nil {
		status = *j
	}
	s.importMu.Unlock()
	writeJSON(w, http.StatusOK, struct {
		importJob
		MaxBytes        int64 `json:"maxBytes"`
		MaxMessageBytes int64 `json:"maxMessageBytes"`
	}{status, importUploadCap, min(nativeMailboxLimits.MessageBytes, mailmsg.MaxInboundMessageBytes)})
}

// handleImportStart proves the caller, prepares the folder and mints the
// upload link. Only a browser session may import: the link is bound to it.
func (s *Server) handleImportStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_, session, ok := s.sessionOf(r)
	if !ok { // a device credential has no session
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "import needs a signed-in browser session"})
		return
	}
	var body struct {
		Mailbox    string `json:"mailbox"`
		Folder     string `json:"folder"`
		Password   string `json:"password"`
		AuthSecret string `json:"authSecret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid import request"})
		return
	}
	r, ok = s.admitMailbox(w, r, body.Mailbox)
	if !ok {
		return
	}
	ac, _ := authFromContext(r)
	im, refused := nativeMail[mailImporter](s, r.Context(), ac.UserID, ac.Mailbox, "import is available for native mailboxes")
	if refused != nil {
		refused.write(w)
		return
	}
	if err := s.importEncryptionOff(ac.UserID); err != nil {
		writeImportRefusal(w, err)
		return
	}
	if !s.confirmActor(w, r, ac.UserID, body.Password, body.AuthSecret) {
		return
	}
	s.importMu.Lock()
	busy := s.importBusy(ac.UserID)
	s.importMu.Unlock()
	if busy != "" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": busy})
		return
	}
	folder, err := im.ImportFolder(r.Context(), body.Folder)
	switch {
	case errors.Is(err, mailbox.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "the parent folder does not exist"})
		return
	case errors.Is(err, imapadapter.ErrUnsafeMailbox):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid folder name"})
		return
	case err != nil:
		http.Error(w, "mailbox is unavailable", http.StatusServiceUnavailable)
		return
	}
	token, err := sso.RandomToken(32)
	if err != nil {
		http.Error(w, "import unavailable", http.StatusInternalServerError)
		return
	}
	correlation, err := sso.RandomToken(8)
	if err != nil {
		http.Error(w, "import unavailable", http.StatusInternalServerError)
		return
	}
	g := importGrant{user: ac.UserID, session: session, mailbox: ac.Mailbox, folder: folder, correlation: correlation, expires: time.Now().Add(importGrantTTL)}
	s.importMu.Lock()
	if s.importGrants == nil {
		s.importGrants = map[string]importGrant{}
	}
	// One unspent link per user, and nothing expired kept around.
	for k, old := range s.importGrants {
		if old.user == ac.UserID || !time.Now().Before(old.expires) {
			delete(s.importGrants, k)
		}
	}
	s.importGrants[token] = g
	s.importMu.Unlock()
	s.auditImport(importJob{Mailbox: g.mailbox, Folder: folder, user: g.user, correlation: correlation}, "authorized", "")
	writeJSON(w, http.StatusOK, map[string]any{"url": "/api/import/" + token, "expiresInSeconds": int(importGrantTTL.Seconds()), "folder": folder, "maxBytes": importUploadCap})
}

// handleImportUpload spends the link, streams the body to a temp file and
// starts the job, answering 202 with its status.
func (s *Server) handleImportUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ac, _ := authFromContext(r)
	_, session, ok := s.sessionOf(r)
	token := r.PathValue("token")
	now := time.Now()
	s.importMu.Lock()
	g, found := s.importGrants[token]
	if found && !now.Before(g.expires) {
		delete(s.importGrants, token)
	}
	if !ok || !found || g.user != ac.UserID || g.session != session || !now.Before(g.expires) {
		s.importMu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "this upload link expired or was already used; start the import again"})
		return
	}
	if busy := s.importBusy(g.user); busy != "" { // the link is kept for a retry
		s.importMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"error": busy})
		return
	}
	delete(s.importGrants, token)
	if s.imports == nil {
		s.imports = map[string]*importJob{}
	}
	job := &importJob{State: "uploading", Mailbox: g.mailbox, Folder: g.folder, user: g.user, correlation: g.correlation}
	s.imports[g.user] = job
	s.importMu.Unlock()

	fail := func(status int, err error) {
		s.finishImport(job, "failed", err.Error())
		writeJSON(w, status, map[string]any{"error": err.Error()})
	}
	im, refused := nativeMail[mailImporter](s, r.Context(), g.user, g.mailbox, "import is available for native mailboxes")
	if refused != nil {
		fail(refused.status, errors.New(refused.error))
		return
	}
	if err := s.importEncryptionOff(g.user); err != nil {
		if !errors.Is(err, errImportIncomingEncryption) {
			fail(http.StatusServiceUnavailable, errors.New("your settings are unavailable"))
			return
		}
		fail(http.StatusConflict, err)
		return
	}
	path, size, status, err := s.receiveImport(w, r)
	if err != nil {
		fail(status, err)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.importMu.Lock()
	job.State, job.cancel = "running", cancel
	started := *job
	s.importMu.Unlock()
	s.auditImport(started, "started", "")
	go s.runImport(ctx, job, im, path, size)
	writeJSON(w, http.StatusAccepted, started)
}

// receiveImport streams the body to a fresh temp file under the state
// directory, refusing more than importUploadCap and cutting off an upload
// idle for importIdle. The file is removed on every failure.
func (s *Server) receiveImport(w http.ResponseWriter, r *http.Request) (string, int64, int, error) {
	tooLarge := errors.New("the file is larger than your mailbox's storage (" + strconv.FormatInt(importUploadCap>>20, 10) + " MiB) and cannot fit")
	if r.ContentLength > importUploadCap {
		return "", 0, http.StatusRequestEntityTooLarge, tooLarge
	}
	dir := filepath.Join(s.stateDir, importDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, http.StatusServiceUnavailable, errors.New("import storage is unavailable")
	}
	f, err := os.CreateTemp(dir, "upload-*")
	if err != nil {
		return "", 0, http.StatusServiceUnavailable, errors.New("import storage is unavailable")
	}
	rc := http.NewResponseController(w)
	n, err := io.Copy(f, idleReader{http.MaxBytesReader(w, r.Body, importUploadCap), rc})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	var maxErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxErr):
		err = tooLarge
	case err != nil:
		err = errors.New("the upload was interrupted; try again")
	case n == 0:
		err = errors.New("the file is empty")
	}
	if err != nil {
		_ = os.Remove(f.Name())
		status := http.StatusBadRequest
		if maxErr != nil {
			status = http.StatusRequestEntityTooLarge
		}
		return "", 0, status, err
	}
	// A long upload may have outlived the server-wide write timeout.
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return f.Name(), n, 0, nil
}

// idleReader moves the read deadline before every read, so an upload may
// take as long as it keeps sending.
type idleReader struct {
	r  io.Reader
	rc *http.ResponseController
}

func (i idleReader) Read(p []byte) (int, error) {
	// Unsupported only on test writers; the server-wide timeout then applies.
	_ = i.rc.SetReadDeadline(time.Now().Add(importIdle))
	return i.r.Read(p)
}

// runImport stores every message of the uploaded file, then removes it.
// Each message is stored under the owner's settings lock with incoming
// encryption re-read, so turning encryption on stops the import before the
// next message.
func (s *Server) runImport(ctx context.Context, job *importJob, im mailImporter, path string, size int64) {
	defer func() { _ = os.Remove(path) }()
	f, err := os.Open(path)
	if err == nil {
		defer f.Close()
		var skipped int
		skipped, err = mailbox.ReadImport(f, size, mailmsg.MaxInboundMessageBytes, importUploadCap, func(raw []byte) error {
			err := s.withIncomingEncryptionOff(job.user, func() error { return im.ImportMessage(ctx, job.Folder, raw) })
			s.importMu.Lock()
			defer s.importMu.Unlock()
			switch {
			case err == nil:
				job.Imported++
				job.Bytes += int64(len(raw))
			case errors.Is(err, mailbox.ErrDuplicate):
				job.Duplicates++
			case errors.Is(err, mailbox.ErrUnimportable):
				job.Skipped++
			default:
				return err
			}
			return nil
		})
		s.importMu.Lock()
		job.Skipped += skipped
		s.importMu.Unlock()
	}
	switch {
	case err == nil:
		s.finishImport(job, "finished", "")
	case ctx.Err() != nil:
		s.finishImport(job, "cancelled", "")
	default:
		s.finishImport(job, "failed", importFailure(err))
	}
}

// importFailure is the user's explanation; it never carries mail content.
func importFailure(err error) string {
	switch {
	case errors.Is(err, mailbox.ErrCapacity):
		return "your mailbox is full; free space and import again (messages already imported are skipped as duplicates)"
	case errors.Is(err, errExtraMailboxIncomingEncryption):
		return "incoming encryption was turned on, so the import stopped rather than store mail unencrypted"
	case errors.Is(err, mailbox.ErrImportArchive):
		return err.Error()
	case errors.Is(err, sso.ErrNativeMailboxUnknown) || errors.Is(err, sso.ErrNativeAdministrator):
		return "the mailbox is no longer available"
	}
	return "the import failed; try again (messages already imported are skipped as duplicates)"
}

func (s *Server) finishImport(job *importJob, state, reason string) {
	s.importMu.Lock()
	job.State, job.Error = state, reason
	if job.cancel != nil {
		job.cancel()
		job.cancel = nil
	}
	done := *job
	s.importMu.Unlock()
	s.auditImport(done, state, reason)
}

func (s *Server) handleImportCancel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ac, _ := authFromContext(r)
	s.importMu.Lock()
	j := s.imports[ac.UserID]
	running := j != nil && j.State == "running" && j.cancel != nil
	var status importJob
	if running {
		j.cancel()
		status = *j
	}
	s.importMu.Unlock()
	if !running {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "no import is running"})
		return
	}
	s.auditImport(status, "cancel_requested", "")
	writeJSON(w, http.StatusOK, status)
}

// cancelImports stops every running import at shutdown; their temp files
// go with them, or at the next startup.
func (s *Server) cancelImports() {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	for _, j := range s.imports {
		if j.cancel != nil {
			j.cancel()
		}
	}
}

func writeImportRefusal(w http.ResponseWriter, err error) {
	if errors.Is(err, errImportIncomingEncryption) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	http.Error(w, "your settings are unavailable", http.StatusServiceUnavailable)
}

// auditImport records the import without correspondence: never subjects or
// addresses. The correlation ID is random per upload link, never the token.
func (s *Server) auditImport(j importJob, result, reason string) {
	slog.New(s.logger.Handler()).Info("mail import", "actor", j.user, "action", "mail_import", "target", cmp.Or(j.Mailbox, j.user), "folder", j.Folder, "messages", int64(j.Imported), "duplicates", int64(j.Duplicates), "skipped", int64(j.Skipped), "bytes", j.Bytes, "result", result, "reason", reason, "correlation_id", j.correlation)
}
