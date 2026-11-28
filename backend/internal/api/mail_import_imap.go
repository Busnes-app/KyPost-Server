package api

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/netguard"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// Self-service import from another mail account over IMAP into the user's own
// native mailbox. POST /api/import/imap proves the account (step-up) for one
// host, port, security and username and mints a grant bound to user, session
// and mailbox. The provider password never travels in that request, whose
// body the KySignOn step-up digests: .../{token}/folders takes it, signs in
// and lists folders, and the grant keeps it in memory only; .../{token}/start
// runs the job, which wipes it after signing in (best effort: see
// dropImportGrantLocked). Status and cancel are the file import's. A re-run is
// safe: duplicates are skipped per folder.

const (
	imapGrantTTL = 10 * time.Minute
	// imapListBytes bounds what one listing reads and so what a grant holds.
	imapListBytes = 4 << 20
	// maxIMAPListings bounds listings dialling out at once, server-wide; a
	// user has at most one, since a new grant cancels the old one's listing.
	maxIMAPListings = 4
	imapSlotWait    = 3 * time.Second
	// A user's failed provider sign-ins, across every grant: the budget is
	// the user's, so minting a new grant (a fresh step-up) does not reset it.
	imapLoginMaxFailures = 6
	imapLoginLockoutFor  = time.Hour
	imapPage             = 200
)

var (
	// imapListTimeout bounds a listing, so a server that never answers holds
	// a slot this long at most.
	imapListTimeout = 45 * time.Second
	// imapImportWindow bounds a whole job.
	imapImportWindow = 4 * time.Hour
	// imapImportBytes bounds what one job downloads, duplicates included:
	// twice what the mailbox can hold.
	imapImportBytes = 2 * nativeMailboxLimits.PayloadBytes
	// Tests replace these to answer DNS and reach a loopback server after
	// the guard approved a public address; production never does.
	imapResolve = net.DefaultResolver.LookupIPAddr
	imapDial    = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	imapRoots   *x509.CertPool // nil: the system's roots
)

var (
	errIMAPPrivate  = errors.New("that server is on a private or internal network; import works only from a public mail provider")
	errIMAPResolve  = errors.New("the server name could not be found")
	errIMAPConnect  = errors.New("could not connect to the server; check the server name and port")
	errIMAPTooMuch  = imapStop("the mail server sent more than twice your mailbox's storage, counting duplicates; choose fewer folders")
	errIMAPTooLong  = imapStop("the import took longer than 4 hours and stopped; import again to continue (messages already imported are skipped as duplicates)")
	errIMAPLost     = imapStop("the connection to the mail server failed; import again to continue (messages already imported are skipped as duplicates)")
	errIMAPTooLarge = imapStop("the mail server sent a message larger than it announced, so the import stopped")
)

// imapStop ends a job; its text is the user's explanation.
type imapStop string

func (e imapStop) Error() string { return string(e) }

// imapAccount is where to sign in; ip is the address the guard approved,
// resolved once and dialled for the listing and the job alike.
type imapAccount struct {
	host     string
	port     int
	startTLS bool
	username string
	ip       net.IP
}

// imapGrant is an unspent IMAP import. A successful listing sets the approved
// address, the provider password sealed to this grant, and the folders it
// listed by name; Listing is set while one is in flight.
type imapGrant struct {
	Host     string                              `json:"host"`
	Port     int                                 `json:"port"`
	StartTLS bool                                `json:"startTLS,omitempty"`
	Username string                              `json:"username"`
	IP       string                              `json:"ip,omitempty"`
	Password []byte                              `json:"password,omitempty"`
	Folders  map[string]imapadapter.RemoteFolder `json:"folders,omitempty"`
	Listing  bool                                `json:"listing,omitempty"`
}

func (g *imapGrant) account() imapAccount {
	return imapAccount{host: g.Host, port: g.Port, startTLS: g.StartTLS, username: g.Username, ip: net.ParseIP(g.IP)}
}

// imapHost normalizes a host name or IP literal.
func imapHost(host string) (string, bool) {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if net.ParseIP(host) != nil {
		return host, true
	}
	if host == "" || len(host) > 253 {
		return "", false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return "", false
			}
		}
	}
	return host, true
}

// imapAddress resolves host once and refuses it when any address is
// loopback, private, link-local, CGNAT, multicast, unspecified or otherwise
// reserved (netguard, IPv4-mapped forms included): one internal answer
// refuses the name, so it cannot pair a public address with an internal one.
func imapAddress(ctx context.Context, host string) (net.IP, error) {
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = append(ips, ip)
	} else {
		addrs, err := imapResolve(ctx, host)
		if err != nil {
			return nil, errIMAPResolve
		}
		for _, a := range addrs {
			if a.Zone != "" {
				return nil, errIMAPPrivate
			}
			ips = append(ips, a.IP)
		}
	}
	if len(ips) == 0 {
		return nil, errIMAPResolve
	}
	for _, ip := range ips {
		if netguard.IsPrivateOrReserved(ip) {
			return nil, errIMAPPrivate
		}
	}
	return ips[0], nil
}

// open dials the approved address, never the name, verifies TLS for the name
// and allows the session to read maxBytes.
func (a *imapAccount) open(ctx context.Context, password []byte, maxBytes int64) (*imapadapter.ImportSource, error) {
	if a.ip == nil {
		ip, err := imapAddress(ctx, a.host)
		if err != nil {
			return nil, err
		}
		a.ip = ip
	}
	conn, err := imapDial(ctx, "tcp", net.JoinHostPort(a.ip.String(), strconv.Itoa(a.port)))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errIMAPConnect
	}
	return imapadapter.OpenImportSource(ctx, conn, a.host, a.startTLS, imapRoots, a.username, password, maxBytes)
}

// imapFailure is the status and the user's explanation for err; it never
// carries a credential or the server's own words.
func imapFailure(err error) (int, string) {
	switch {
	case errors.Is(err, errIMAPPrivate), errors.Is(err, errIMAPResolve):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, imapadapter.ErrImportLogin):
		return http.StatusBadRequest, "the provider refused the sign-in; check the username and password (Gmail, iCloud and Outlook need an app password)"
	case errors.Is(err, imapadapter.ErrImportTLS):
		return http.StatusBadGateway, "a secure connection could not be established: the server's certificate is not valid for that name, or it does not offer the security you chose"
	case errors.Is(err, imapadapter.ErrImportFolders):
		return http.StatusBadGateway, err.Error()
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, errIMAPConnect):
		return http.StatusGatewayTimeout, errIMAPConnect.Error()
	}
	return http.StatusBadGateway, "the server did not answer as an IMAP server should; check the server name, port and security"
}

// handleIMAPImportStart proves the caller for one account and mints the grant.
func (s *Server) handleIMAPImportStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_, session, ok := s.sessionOf(r)
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "import needs a signed-in browser session"})
		return
	}
	var body struct {
		Mailbox    string `json:"mailbox"`
		Host       string `json:"host"`
		Port       int    `json:"port"`
		Security   string `json:"security"`
		Username   string `json:"username"`
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
	if _, refused := nativeMail[mailImporter](s, r.Context(), ac.UserID, ac.Mailbox, "import is available for native mailboxes"); refused != nil {
		refused.write(w)
		return
	}
	host, ok := imapHost(body.Host)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "enter the mail server's name, such as imap.example.com"})
		return
	}
	// Only the two IMAP ports: nothing else is a mail server worth reaching,
	// and a free port would make this a scanner of other services.
	implicitTLS := body.Security == "tls" && body.Port == 993
	startTLS := body.Security == "starttls" && body.Port == 143
	if !implicitTLS && !startTLS {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "use port 993 with TLS, or port 143 with STARTTLS"})
		return
	}
	username := strings.TrimSpace(body.Username)
	if username == "" || len(username) > 320 || strings.ContainsAny(username, "\x00\r\n") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "enter the username for the other account"})
		return
	}
	if !s.confirmActor(w, r, ac.UserID, body.AuthSecret) {
		return
	}
	if !s.imapAdmissible(w, ac.UserID) {
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
	g := importGrant{User: ac.UserID, Session: sessionHash(session), Mailbox: ac.Mailbox, Correlation: correlation, Expires: time.Now().Add(imapGrantTTL),
		IMAP: &imapGrant{Host: host, Port: body.Port, StartTLS: startTLS, Username: username}}
	// Replacing the user's grant cancels its listing in flight.
	if err := s.putImportGrant(token, g); err != nil {
		writeUnavailable(w)
		return
	}
	// A sealed password does not outlive the grant waiting for the next request.
	time.AfterFunc(imapGrantTTL+time.Second, func() { s.dropImportGrant(token) })
	s.auditImport(importJob{Mailbox: g.Mailbox, Host: host, user: g.User, correlation: correlation}, "authorized", "")
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "expiresInSeconds": int(imapGrantTTL.Seconds()), "target": imapDefaultTarget(host)})
}

// imapAdmissible refuses (and answers) while the server shuts down or the
// user's import, or the server's import slots, are busy.
func (s *Server) imapAdmissible(w http.ResponseWriter, user string) bool {
	s.importMu.Lock()
	closed := s.importClosed
	s.importMu.Unlock()
	if closed {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the server is shutting down; import again after it restarts"})
		return false
	}
	if status, busy := s.importRefusal(user); status != 0 {
		writeJSON(w, status, map[string]any{"error": busy})
		return false
	}
	return true
}

func imapDefaultTarget(host string) string {
	return mailbox.ImportFolderDefault + "/" + strings.ReplaceAll(host, ".", "-")
}

// imapCaller is who may use the grant at the request's token: its user and
// browser session (none for a device). Taken before importMu, which ranks
// after sessMu.
func (s *Server) imapCaller(r *http.Request) (user, session string) {
	ac, _ := authFromContext(r)
	if _, session, ok := s.sessionOf(r); ok {
		return ac.UserID, session
	}
	return ac.UserID, ""
}

var errIMAPGrant = map[string]any{"error": "this import expired or was already started; start again"}

// handleIMAPImportFolders signs in with the provider password and lists the
// folders. The grant keeps the password only after a successful sign-in.
func (s *Server) handleIMAPImportFolders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct {
		Password string `json:"password"` // the other account's
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil || body.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "enter the password for the other account"})
		return
	}
	password := []byte(body.Password)
	body.Password = ""
	token := r.PathValue("token")
	user, session := s.imapCaller(r)
	if !s.imapAdmissible(w, user) {
		clear(password)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), imapListTimeout)
	defer cancel()
	var g importGrant
	mine := func(v *importGrant, found bool) bool {
		return found && v.IMAP != nil && v.User == user && session != "" && v.Session == sessionHash(session)
	}
	if !s.editImportGrant(token, func(v *importGrant, found bool) (rowEdit, bool) {
		if !mine(v, found) || v.IMAP.Listing {
			return rowKeep, false
		}
		v.IMAP.Listing = true
		g = *v
		return rowWrite, true
	}) {
		clear(password)
		writeJSON(w, http.StatusNotFound, errIMAPGrant)
		return
	}
	s.importMu.Lock()
	if s.importListings == nil {
		s.importListings = map[string]importListing{}
	}
	s.importListings[user] = importListing{token, cancel}
	s.importMu.Unlock()
	account := g.IMAP.account()

	var folders []imapadapter.RemoteFolder
	err := s.listIMAP(ctx, user, &account, password, &folders)
	s.importMu.Lock()
	if l, ok := s.importListings[user]; ok && l.token == token {
		delete(s.importListings, user)
	}
	s.importMu.Unlock()
	var sealed []byte
	var sealErr error
	if err == nil {
		sealed, sealErr = sealIMAPPassword(user, token, password)
	}
	clear(password)
	listed := make(map[string]imapadapter.RemoteFolder, len(folders))
	for _, f := range folders {
		listed[f.Name] = f
	}
	// The grant keeps the sealed password only after a successful sign-in.
	live := s.editImportGrant(token, func(v *importGrant, found bool) (rowEdit, bool) {
		if !mine(v, found) || !v.IMAP.Listing {
			return rowKeep, false
		}
		v.IMAP.Listing = false
		if err == nil && sealErr == nil {
			v.IMAP.Password, v.IMAP.IP, v.IMAP.Folders = sealed, account.ip.String(), listed
		}
		return rowWrite, true
	})
	if sealErr != nil {
		http.Error(w, "import unavailable", http.StatusServiceUnavailable)
		return
	}
	switch {
	case err != nil || !live:
		var refused imapRefusal
		switch {
		case errors.As(err, &refused):
			if refused.retryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(refused.retryAfter.Seconds())+1))
			}
			writeJSON(w, refused.status, map[string]any{"error": refused.reason})
			return
		case err == nil, !live:
			writeJSON(w, http.StatusNotFound, errIMAPGrant)
			return
		}
		status, reason := imapFailure(err)
		s.auditImport(importJob{Mailbox: g.Mailbox, Host: account.host, user: g.User, correlation: g.Correlation}, "list_failed", reason)
		writeJSON(w, status, map[string]any{"error": reason})
		return
	}
	s.auditImport(importJob{Mailbox: g.Mailbox, Host: account.host, user: g.User, correlation: g.Correlation}, "listed", "")
	type folder struct {
		Name       string   `json:"name"`
		Path       []string `json:"path"`
		Attributes []string `json:"attributes"`
	}
	out := make([]folder, 0, len(folders))
	for _, f := range folders {
		out = append(out, folder{f.Name, f.Path, append([]string{}, f.Attrs...)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"folders": out, "target": imapDefaultTarget(account.host)})
}

// imapRefusal is a listing refused before any dial.
type imapRefusal struct {
	status     int
	reason     string
	retryAfter time.Duration
}

func (e imapRefusal) Error() string { return e.reason }

// listIMAP signs in and lists folders within the server-wide listing slots
// and the user's sign-in budget, which only a successful sign-in refunds.
func (s *Server) listIMAP(ctx context.Context, user string, account *imapAccount, password []byte, folders *[]imapadapter.RemoteFolder) error {
	// A short wait lets a listing just cancelled by a new grant hand back its
	// slot before this one is refused.
	wait := time.NewTimer(imapSlotWait)
	defer wait.Stop()
	select {
	case s.imapListSlots <- struct{}{}:
		defer func() { <-s.imapListSlots }()
	case <-wait.C:
		return imapRefusal{http.StatusServiceUnavailable, "the server is busy connecting to other mail accounts; try again in a minute", 0}
	case <-ctx.Done():
		return ctx.Err()
	}
	ok, retry, strike := s.imapLoginLockout.tryAttempt(user)
	if !ok {
		return imapRefusal{http.StatusTooManyRequests, "too many failed attempts to sign in to other mail accounts; try again in an hour", retry}
	}
	src, err := account.open(ctx, password, imapListBytes)
	if err != nil {
		return err
	}
	s.imapLoginLockout.cancelAttempt(strike)
	*folders, err = src.Folders()
	_ = src.Close()
	return err
}

type imapImportFolder struct {
	remote, local string
}

// handleIMAPImportRun spends the grant and starts the job on the chosen
// folders, under target.
func (s *Server) handleIMAPImportRun(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct {
		Folders []string `json:"folders"`
		Target  string   `json:"target"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid import request"})
		return
	}
	token := r.PathValue("token")
	user, session := s.imapCaller(r)
	g, ok := s.importGrantOf(token, user, session, true)
	if !ok {
		writeJSON(w, http.StatusNotFound, errIMAPGrant)
		return
	}
	im, refused := nativeMail[mailImporter](s, r.Context(), g.User, g.Mailbox, "import is available for native mailboxes")
	if refused != nil {
		refused.write(w)
		return
	}
	target := strings.TrimSpace(body.Target)
	if target == "" {
		target = imapDefaultTarget(g.IMAP.Host)
	}
	if _, err := mailbox.ImportPath(target, nil); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid target folder: each level needs a name without '.', '/', '\\' or '\"'"})
		return
	}

	s.importMu.Lock()
	closed := s.importClosed
	s.importMu.Unlock()
	var refusal string
	status := http.StatusBadRequest
	var folders []imapImportFolder
	switch {
	case g.IMAP.Listing || g.IMAP.Password == nil:
		refusal, status = "list the folders first", http.StatusConflict
	case closed:
		refusal, status = "the server is shutting down; import again after it restarts", http.StatusServiceUnavailable
	case len(body.Folders) == 0:
		refusal = "choose at least one folder"
	default:
		seen := map[string]bool{}
		for _, name := range body.Folders {
			f, listed := g.IMAP.Folders[name]
			if !listed || seen[name] {
				refusal = "choose folders from the list"
				break
			}
			seen[name] = true
			local, err := mailbox.ImportPath(target, f.Path)
			if err != nil {
				refusal = "the folder " + strings.Join(f.Path, "/") + " has a name that cannot be stored here; leave it out"
				break
			}
			folders = append(folders, imapImportFolder{name, local})
		}
	}
	if refusal != "" {
		writeJSON(w, status, map[string]any{"error": refusal})
		return
	}
	account := g.IMAP.account()
	job := &importJob{State: "running", Mailbox: g.Mailbox, Folder: target, Host: account.host, FoldersTotal: len(folders), user: g.User, correlation: g.Correlation}
	if status, busy, undo := s.claimImport(job); status != 0 { // the grant is kept for a retry
		writeJSON(w, status, map[string]any{"error": busy})
		return
	} else if !s.editImportGrant(token, func(v *importGrant, found bool) (rowEdit, bool) {
		if !found || v.IMAP == nil || v.User != g.User || v.Session != g.Session || v.IMAP.Listing || !bytes.Equal(v.IMAP.Password, g.IMAP.Password) {
			return rowKeep, false
		}
		return rowDrop, true // the job owns the password now
	}) {
		undo()
		writeJSON(w, http.StatusNotFound, errIMAPGrant)
		return
	}
	password, err := openIMAPPassword(g.User, token, g.IMAP.Password)
	if err != nil {
		s.finishImport(job, "failed", "the import could not start; start it again")
		http.Error(w, "import unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.importMu.Lock()
	job.cancel = cancel
	if s.importClosed { // shutdown began meanwhile: the job ends as cancelled
		cancel()
	}
	started := *job
	s.importMu.Unlock()
	s.auditImport(started, "started", "")
	go s.runIMAPImport(ctx, job, im, account, password, folders)
	writeJSON(w, http.StatusAccepted, started)
}

// runIMAPImport copies the folders and records how it ended.
func (s *Server) runIMAPImport(ctx context.Context, job *importJob, im mailImporter, account imapAccount, password []byte, folders []imapImportFolder) {
	window, stop := context.WithTimeout(ctx, imapImportWindow)
	defer stop()
	err := s.copyIMAP(window, job, im, account, password, folders)
	switch {
	case err == nil:
		s.finishImport(job, "finished", "")
	case ctx.Err() != nil:
		s.finishImport(job, "cancelled", "")
	case window.Err() != nil:
		s.finishImport(job, "failed", string(errIMAPTooLong))
	default:
		s.finishImport(job, "failed", importFailure(err))
	}
}

func (s *Server) copyIMAP(ctx context.Context, job *importJob, im mailImporter, account imapAccount, password []byte, folders []imapImportFolder) error {
	src, err := account.open(ctx, password, imapImportBytes)
	clear(password)
	if err != nil {
		_, reason := imapFailure(err)
		return imapStop(reason)
	}
	defer func() { _ = src.Close() }()
	// lost names why a session failed: its byte budget, or anything else.
	lost := func(err error) error {
		if errors.Is(err, imapadapter.ErrImportBudget) {
			return errIMAPTooMuch
		}
		return errIMAPLost
	}
	seen := 0
	for i, f := range folders {
		s.importMu.Lock()
		job.Current, job.FoldersDone = f.local, i
		s.importMu.Unlock()
		if err = importFolderPath(ctx, im, f.local); err != nil {
			return err
		}
		n, err := src.Examine(f.remote)
		if errors.Is(err, imapadapter.ErrImportBudget) {
			return errIMAPTooMuch
		} else if err != nil {
			return imapStop("the mail server would not open " + f.local + "; leave that folder out and import again")
		}
		// Every sequence number walked counts, so a huge EXISTS is refused
		// before any page is fetched; compared before adding, so it cannot wrap.
		if n > importMaxMessages-seen {
			return mailbox.ErrImportTooMany
		}
		seen += n
		for lo := 1; lo <= n; lo += imapPage {
			msgs, err := src.Messages(lo, min(n, lo+imapPage-1))
			if err != nil {
				return lost(err)
			}
			for _, m := range msgs {
				// Refused by its announced size, never downloaded.
				if m.Size <= 0 || m.Size > importMessageBytes {
					if err = s.countImport(job, 0, mailbox.ErrUnimportable); err != nil {
						return err
					}
					continue
				}
				// Held to its announced size: more stops the job.
				raw, err := src.Fetch(m.UID, m.Size)
				switch {
				case err == nil:
					err = s.importOne(ctx, job, im, f.local, raw, mailbox.ImportMeta{Unseen: !m.Seen, Starred: m.Flagged, Received: m.Received})
				case errors.Is(err, imapadapter.ErrImportGone):
					err = mailbox.ErrUnimportable
				case errors.Is(err, imapadapter.ErrImportTooLarge):
					return errIMAPTooLarge
				default:
					return lost(err)
				}
				if err = s.countImport(job, int64(len(raw)), err); err != nil {
					return err
				}
			}
		}
	}
	s.importMu.Lock()
	job.Current, job.FoldersDone = "", len(folders)
	s.importMu.Unlock()
	return nil
}

// importFolderPath creates folder and each missing parent, top down.
func importFolderPath(ctx context.Context, im mailImporter, folder string) error {
	for i := range folder {
		if folder[i] == '/' {
			if _, err := im.ImportFolder(ctx, folder[:i], true); err != nil {
				return err
			}
		}
	}
	_, err := im.ImportFolder(ctx, folder, true)
	return err
}
