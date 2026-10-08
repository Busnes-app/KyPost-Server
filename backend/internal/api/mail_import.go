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
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
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

var (
	// importUploadWindow bounds a whole upload (5 GiB in two hours is
	// 0.7 MiB/s), so a trickling upload frees its slot.
	importUploadWindow = 2 * time.Hour
	// importMaxMessages bounds one job's work, counting duplicates and
	// skipped messages: twice what the mailbox can hold.
	importMaxMessages = 2 * nativeMailboxLimits.Records
	// importMessageBytes is the per-message limit the screen shows.
	importMessageBytes = min(nativeMailboxLimits.MessageBytes, mailmsg.MaxInboundMessageBytes)
)

var errImportIncomingEncryption = errors.New("import is unavailable while incoming encryption is on or a replacement is pending: imported mail would be stored unencrypted")

type mailImporter interface {
	ImportFolder(ctx context.Context, folder string, create bool) (string, error)
	ImportMessage(ctx context.Context, folder string, raw []byte, meta mailbox.ImportMeta) error
}

// importGrant is an unspent upload link, or with imap set an IMAP import.
type importGrant struct {
	user, session, mailbox, folder, correlation string
	expires                                     time.Time
	imap                                        *imapGrant
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
	// An IMAP import's provider host, local folder in progress and folders.
	Host         string `json:"host,omitempty"`
	Current      string `json:"current,omitempty"`
	FoldersDone  int    `json:"foldersDone,omitempty"`
	FoldersTotal int    `json:"foldersTotal,omitempty"`

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
	}{status, importUploadCap, importMessageBytes})
}

// handleImportStart proves the caller, checks the folder and mints the
// upload link; the folder is created once the upload arrives. Only a browser session may import: the link is bound to it.
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
	folder, err := im.ImportFolder(r.Context(), body.Folder, false)
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
			s.dropImportGrantLocked(k)
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
		s.dropImportGrantLocked(token)
	}
	if !ok || !found || g.imap != nil || g.user != ac.UserID || g.session != session || !now.Before(g.expires) {
		s.importMu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "this upload link expired or was already used; start the import again"})
		return
	}
	if s.importClosed {
		s.importMu.Unlock()
		http.Error(w, "the server is shutting down", http.StatusServiceUnavailable)
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
	if _, err = im.ImportFolder(r.Context(), g.folder, true); err != nil {
		_ = os.Remove(path)
		fail(http.StatusConflict, errors.New("the target folder could not be created; start the import again"))
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.importMu.Lock()
	if s.importClosed { // shutdown began during the upload
		s.importMu.Unlock()
		cancel()
		_ = os.Remove(path)
		fail(http.StatusServiceUnavailable, errors.New("the server is shutting down; import again after it restarts"))
		return
	}
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
	space := &uploadSpace{s: s, every: importReserveEvery}
	defer space.release()
	if r.ContentLength > 0 {
		if err := space.reserve(r.ContentLength); err != nil {
			return "", 0, http.StatusInsufficientStorage, errors.New(importFailure(err))
		}
	} else {
		space.chunked = true
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
	space.f = f
	n, err := io.Copy(space, &idleReader{http.MaxBytesReader(w, r.Body, importUploadCap), rc, time.Now().Add(importUploadWindow)})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	var maxErr *http.MaxBytesError
	status := http.StatusBadRequest
	switch {
	case errors.As(err, &maxErr):
		err, status = tooLarge, http.StatusRequestEntityTooLarge
	case errors.Is(err, errUploadTooSlow):
		status = http.StatusRequestTimeout
	case errors.Is(err, fsutil.ErrDriveReserve):
		err, status = errors.New(importFailure(err)), http.StatusInsufficientStorage
	case err != nil:
		err = errors.New("the upload was interrupted; try again")
	case n == 0:
		err = errors.New("the file is empty")
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", 0, status, err
	}
	// A long upload may have outlived the server-wide write timeout.
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return f.Name(), n, 0, nil
}

// importReserveEvery is how many bytes an upload writes between drive
// reserve checks.
var importReserveEvery int64 = 4 << 20

// uploadSpace writes an upload to its temp file without crossing the drive
// reserve. Every upload's promised bytes (its declared length still unwritten,
// or the next importReserveEvery of an upload without one) are held in
// Server.importPending, added before checking, so concurrent uploads count
// each other and at worst refuse together, never pass together.
type uploadSpace struct {
	s                          *Server
	f                          *os.File
	chunked                    bool
	every, promised, unchecked int64
}

func (u *uploadSpace) reserve(n int64) error {
	u.promised += n
	return fsutil.CheckDriveReserve(u.s.stateDir, u.s.importPending.Add(n))
}

func (u *uploadSpace) Write(p []byte) (int, error) {
	if u.unchecked <= 0 {
		var err error
		if u.chunked {
			err = u.reserve(u.every - u.promised)
		} else {
			err = fsutil.CheckDriveReserve(u.s.stateDir, u.s.importPending.Load())
		}
		if err != nil {
			return 0, err
		}
		u.unchecked = u.every
	}
	n, err := u.f.Write(p)
	written := min(int64(n), u.promised)
	u.promised -= written
	u.s.importPending.Add(-written)
	u.unchecked -= int64(n)
	return n, err
}

func (u *uploadSpace) release() { u.s.importPending.Add(-u.promised); u.promised = 0 }

var errUploadTooSlow = errors.New("the upload took too long (two hours at most); try again on a faster connection or with a smaller file")

// idleReader moves the read deadline before every read to importIdle from
// now, never past the whole upload's deadline.
type idleReader struct {
	r        io.Reader
	rc       *http.ResponseController
	deadline time.Time
}

func (i *idleReader) Read(p []byte) (int, error) {
	now := time.Now()
	if !now.Before(i.deadline) {
		return 0, errUploadTooSlow
	}
	// Unsupported only on test writers; the server-wide timeout then applies.
	next := now.Add(importIdle)
	if next.After(i.deadline) {
		next = i.deadline
	}
	_ = i.rc.SetReadDeadline(next)
	n, err := i.r.Read(p)
	if err != nil && !time.Now().Before(i.deadline) && !errors.Is(err, io.EOF) {
		err = errUploadTooSlow
	}
	return n, err
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
		// Junk is skipped inside ReadImport before any lock. Each message then
		// takes the settings lock and mailbox admission on its own: holding
		// either across a batch would block the user's settings writes, or keep
		// storing into a mailbox already disabled, for the whole batch.
		skipped, err = mailbox.ReadImport(f, size, importMessageBytes, importUploadCap, importMaxMessages, func(raw []byte) error {
			return s.countImport(job, int64(len(raw)), s.importOne(ctx, job, im, job.Folder, raw, mailbox.ImportMeta{}))
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

// importOne stores raw under the owner's settings lock with incoming
// encryption re-read, so turning encryption on stops before the next message.
// An import never eats into the drive reserve kept for incoming mail.
func (s *Server) importOne(ctx context.Context, job *importJob, im mailImporter, folder string, raw []byte, meta mailbox.ImportMeta) error {
	if err := fsutil.CheckDriveReserve(s.stateDir, int64(len(raw))); err != nil {
		return err
	}
	return s.withIncomingEncryptionOff(job.user, func() error { return im.ImportMessage(ctx, folder, raw, meta) })
}

// countImport counts one message's outcome; only an error that stops the job
// is returned.
func (s *Server) countImport(job *importJob, size int64, err error) error {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	switch {
	case err == nil:
		job.Imported++
		job.Bytes += size
	case errors.Is(err, mailbox.ErrDuplicate):
		job.Duplicates++
	case errors.Is(err, mailbox.ErrUnimportable):
		job.Skipped++
	default:
		return err
	}
	return nil
}

// importFailure is the user's explanation; it never carries mail content.
func importFailure(err error) string {
	switch {
	case errors.Is(err, mailbox.ErrCapacity):
		return "your mailbox is full; free space and import again (messages already imported are skipped as duplicates)"
	case errors.Is(err, fsutil.ErrDriveReserve):
		return "the server's disk is nearly full and the space left is kept for incoming mail; ask your administrator to free space, then import again (messages already imported are skipped as duplicates)"
	case errors.Is(err, errExtraMailboxIncomingEncryption):
		return "incoming encryption was turned on, so the import stopped rather than store mail unencrypted"
	case errors.Is(err, mailbox.ErrImportArchive):
		return err.Error()
	case errors.Is(err, mailbox.ErrImportTooMany):
		return "the import holds more than " + strconv.Itoa(importMaxMessages) + " messages, counting duplicates and skipped ones (twice what your mailbox holds); split the file, or choose fewer folders"
	case errors.As(err, new(imapStop)):
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

// cancelImports stops every running import at shutdown, and no later
// upload starts one; their temp files go with them, or at the next startup.
func (s *Server) cancelImports() {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	s.importClosed = true
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
// addresses, and for an IMAP import the provider host but never the username.
// The correlation ID is random per grant, never the token.
func (s *Server) auditImport(j importJob, result, reason string) {
	source := "file"
	if j.Host != "" {
		source = "imap"
	}
	slog.New(s.logger.Handler()).Info("mail import", "actor", j.user, "action", "mail_import", "target", cmp.Or(j.Mailbox, j.user), "source", source, "host", j.Host, "folder", j.Folder, "messages", int64(j.Imported), "duplicates", int64(j.Duplicates), "skipped", int64(j.Skipped), "bytes", j.Bytes, "result", result, "reason", reason, "correlation_id", j.correlation)
}
