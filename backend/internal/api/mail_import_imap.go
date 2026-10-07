package api

import (
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
	imapGrantTTL    = 10 * time.Minute
	imapListTimeout = 90 * time.Second
	// imapListBytes bounds what one listing reads and so what a grant holds.
	imapListBytes = 4 << 20
	// maxIMAPListings bounds listings dialling out at once, server-wide.
	maxIMAPListings = 4
	// A user's failed provider sign-ins, across every grant: the budget is
	// the user's, so minting a new grant (a fresh step-up) does not reset it.
	imapLoginMaxFailures = 6
	imapLoginLockoutFor  = time.Hour
	imapPage             = 200
)

var (
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

// imapGrant is an unspent IMAP import. password is set by a successful
// listing; folders are what it listed, by name; stop cancels a listing in
// flight.
type imapGrant struct {
	imapAccount
	password []byte
	folders  map[string]imapadapter.RemoteFolder
	listing  bool
	stop     context.CancelFunc
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

// dropImportGrantLocked deletes the grant at token, cancels its listing in
// flight and wipes its held password. The listing holds its own copy and
// wipes it on finding the grant gone. Wiping is best effort: the JSON decoder
// and TLS buffers held copies that only the collector reclaims. Hold importMu.
func (s *Server) dropImportGrantLocked(token string) {
	g, ok := s.importGrants[token]
	delete(s.importGrants, token)
	if ok && g.imap != nil {
		if g.imap.stop != nil {
			g.imap.stop()
		}
		clear(g.imap.password)
		g.imap.password = nil
	}
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
		Password   string `json:"password"` // the KyPost account's, for step-up
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
	if err := s.importEncryptionOff(ac.UserID); err != nil {
		writeImportRefusal(w, err)
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
	if !s.confirmActor(w, r, ac.UserID, body.Password, body.AuthSecret) {
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
	now := time.Now()
	g := importGrant{user: ac.UserID, session: session, mailbox: ac.Mailbox, correlation: correlation, expires: now.Add(imapGrantTTL),
		imap: &imapGrant{imapAccount: imapAccount{host: host, port: body.Port, startTLS: startTLS, username: username}}}
	s.importMu.Lock()
	if s.importGrants == nil {
		s.importGrants = map[string]importGrant{}
	}
	for k, old := range s.importGrants {
		if old.user == ac.UserID || !now.Before(old.expires) {
			s.dropImportGrantLocked(k)
		}
	}
	// Replacing the user's grant cancels its listing in flight.
	s.importGrants[token] = g
	s.importMu.Unlock()
	// A held password must not outlive the grant waiting for the next request.
	time.AfterFunc(imapGrantTTL+time.Second, func() {
		s.importMu.Lock()
		defer s.importMu.Unlock()
		if old, ok := s.importGrants[token]; ok && !time.Now().Before(old.expires) {
			s.dropImportGrantLocked(token)
		}
	})
	s.auditImport(importJob{Mailbox: g.mailbox, Host: host, user: g.user, correlation: correlation}, "authorized", "")
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "expiresInSeconds": int(imapGrantTTL.Seconds()), "target": imapDefaultTarget(host)})
}

// imapAdmissible refuses (and answers) while the server shuts down or the
// user's import, or the server's import slots, are busy.
func (s *Server) imapAdmissible(w http.ResponseWriter, user string) bool {
	s.importMu.Lock()
	closed, busy := s.importClosed, s.importBusy(user)
	s.importMu.Unlock()
	switch {
	case closed:
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the server is shutting down; import again after it restarts"})
	case busy != "":
		writeJSON(w, http.StatusConflict, map[string]any{"error": busy})
	default:
		return true
	}
	return false
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

// imapGrantOf returns the caller's live IMAP grant at token. Hold importMu.
func (s *Server) imapGrantOf(user, session, token string) (importGrant, bool) {
	g, found := s.importGrants[token]
	return g, session != "" && found && g.imap != nil && g.user == user && g.session == session && time.Now().Before(g.expires)
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
	s.importMu.Lock()
	g, ok := s.imapGrantOf(user, session, token)
	if !ok || g.imap.listing {
		s.importMu.Unlock()
		clear(password)
		writeJSON(w, http.StatusNotFound, errIMAPGrant)
		return
	}
	g.imap.listing, g.imap.stop = true, cancel
	account := g.imap.imapAccount
	s.importMu.Unlock()

	var folders []imapadapter.RemoteFolder
	err := s.listIMAP(ctx, user, &account, password, &folders)
	s.importMu.Lock()
	g.imap.listing, g.imap.stop = false, nil
	_, live := s.importGrants[token]
	switch {
	case err != nil || !live:
		s.importMu.Unlock()
		clear(password)
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
		s.auditImport(importJob{Mailbox: g.mailbox, Host: account.host, user: g.user, correlation: g.correlation}, "list_failed", reason)
		writeJSON(w, status, map[string]any{"error": reason})
		return
	}
	clear(g.imap.password)
	g.imap.password, g.imap.ip = password, account.ip
	g.imap.folders = make(map[string]imapadapter.RemoteFolder, len(folders))
	for _, f := range folders {
		g.imap.folders[f.Name] = f
	}
	s.importMu.Unlock()
	s.auditImport(importJob{Mailbox: g.mailbox, Host: account.host, user: g.user, correlation: g.correlation}, "listed", "")
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
	select {
	case s.imapListSlots <- struct{}{}:
		defer func() { <-s.imapListSlots }()
	default:
		return imapRefusal{http.StatusServiceUnavailable, "the server is busy connecting to other mail accounts; try again in a minute", 0}
	}
	if ok, retry := s.imapLoginLockout.tryAttempt(user); !ok {
		return imapRefusal{http.StatusTooManyRequests, "too many failed attempts to sign in to other mail accounts; try again in an hour", retry}
	}
	src, err := account.open(ctx, password, imapListBytes)
	if err != nil {
		return err
	}
	s.imapLoginLockout.cancelAttempt(user)
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
	s.importMu.Lock()
	g, ok := s.imapGrantOf(user, session, token)
	s.importMu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, errIMAPGrant)
		return
	}
	im, refused := nativeMail[mailImporter](s, r.Context(), g.user, g.mailbox, "import is available for native mailboxes")
	if refused != nil {
		refused.write(w)
		return
	}
	if err := s.importEncryptionOff(g.user); err != nil {
		writeImportRefusal(w, err)
		return
	}
	target := strings.TrimSpace(body.Target)
	if target == "" {
		target = imapDefaultTarget(g.imap.host)
	}
	if _, err := mailbox.ImportPath(target, nil); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid target folder: each level needs a name without '.', '/', '\\' or '\"'"})
		return
	}

	s.importMu.Lock()
	g, ok = s.imapGrantOf(user, session, token)
	var refusal string
	status := http.StatusBadRequest
	var folders []imapImportFolder
	switch {
	case !ok:
		refusal, status = errIMAPGrant["error"].(string), http.StatusNotFound
	case g.imap.listing || g.imap.password == nil:
		refusal, status = "list the folders first", http.StatusConflict
	case s.importClosed:
		refusal, status = "the server is shutting down; import again after it restarts", http.StatusServiceUnavailable
	case len(body.Folders) == 0:
		refusal = "choose at least one folder"
	default:
		if refusal = s.importBusy(g.user); refusal != "" {
			status = http.StatusConflict
			break
		}
		seen := map[string]bool{}
		for _, name := range body.Folders {
			f, listed := g.imap.folders[name]
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
		s.importMu.Unlock()
		writeJSON(w, status, map[string]any{"error": refusal})
		return
	}
	account, password := g.imap.imapAccount, g.imap.password
	g.imap.password = nil // the job owns it now
	s.dropImportGrantLocked(token)
	if s.imports == nil {
		s.imports = map[string]*importJob{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &importJob{State: "running", Mailbox: g.mailbox, Folder: target, Host: account.host, FoldersTotal: len(folders), user: g.user, correlation: g.correlation, cancel: cancel}
	s.imports[g.user] = job
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
		// before any page is fetched.
		if seen += n; seen > importMaxMessages {
			return mailbox.ErrImportTooMany
		}
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
