package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// Import grants and job status, shared by replicas on PostgreSQL. A grant (an upload link or
// an IMAP import) is an "import" row of auth_challenges, one per user, spent by
// compare-and-delete; in memory it is importGrants. A job runs on the replica that took its
// upload or start. On PostgreSQL its status is the user's import_jobs row, which the runner
// writes at least every importPublishEvery and reads a cancel request back from; a job whose
// replica has stopped reads as interrupted.

const importRow = "import"

var importPublishEvery = time.Second

const importInterrupted = "the server running this import stopped; import again to continue (messages already imported are skipped as duplicates)"

// importListing is a user's IMAP listing in flight on this replica; a new grant cancels it.
type importListing struct {
	token string
	stop  context.CancelFunc
}

// putImportGrant records the user's only grant, cancelling their listing in flight here.
func (s *Server) putImportGrant(token string, g importGrant) error {
	s.importMu.Lock()
	if l, ok := s.importListings[g.User]; ok {
		l.stop()
		delete(s.importListings, g.User)
	}
	s.importMu.Unlock()
	if s.shared != nil {
		return putRow(s.shared, importRow, token, g.User, g, g.Expires, true)
	}
	s.importMu.Lock()
	defer s.importMu.Unlock()
	if s.importGrants == nil {
		s.importGrants = map[string]importGrant{}
	}
	for k, old := range s.importGrants {
		if old.User == g.User || !time.Now().Before(old.Expires) {
			delete(s.importGrants, k)
		}
	}
	s.importGrants[token] = g
	return nil
}

// editImportGrant applies edit to the live grant at token atomically: under importMu in
// memory, as a compare-and-swap on PostgreSQL. An expired grant is not found.
func (s *Server) editImportGrant(token string, edit func(g *importGrant, found bool) (rowEdit, bool)) bool {
	if s.shared != nil {
		return editRow(s.shared, importRow, token, edit)
	}
	s.importMu.Lock()
	defer s.importMu.Unlock()
	g, found := s.importGrants[token]
	if found && !time.Now().Before(g.Expires) {
		delete(s.importGrants, token)
		found = false
	}
	action, verdict := edit(&g, found)
	switch action {
	case rowWrite:
		s.importGrants[token] = g
	case rowDrop:
		delete(s.importGrants, token)
	}
	return verdict
}

// importGrantOf returns the caller's live grant at token; imap selects the kind.
func (s *Server) importGrantOf(token, user, session string, imap bool) (importGrant, bool) {
	var out importGrant
	ok := session != "" && s.editImportGrant(token, func(g *importGrant, found bool) (rowEdit, bool) {
		if !found || g.User != user || g.Session != sessionHash(session) || (g.IMAP != nil) != imap {
			return rowKeep, false
		}
		out = *g
		return rowKeep, true
	})
	return out, ok
}

// dropImportGrant removes the grant at token after it can no longer be live, so a sealed
// password does not wait for the expiry sweep.
func (s *Server) dropImportGrant(token string) {
	if s.shared != nil {
		ctx, cancel := sharedContext()
		defer cancel()
		_ = s.shared.DropChallenge(ctx, rowKeyOf(importRow, token))
		return
	}
	s.importMu.Lock()
	defer s.importMu.Unlock()
	delete(s.importGrants, token)
}

// importRefusal is why userID cannot start an import now, as a status and the user's text;
// status 0 when it may.
func (s *Server) importRefusal(userID string) (int, string) {
	if s.shared != nil {
		ctx, cancel := sharedContext()
		defer cancel()
		return importBusyRefusal(s.shared.ImportBusy(ctx, userID, maxImports))
	}
	s.importMu.Lock()
	defer s.importMu.Unlock()
	if busy := s.importBusy(userID); busy != "" {
		return http.StatusConflict, busy
	}
	return 0, ""
}

func importBusyRefusal(err error) (int, string) {
	switch {
	case err == nil:
		return 0, ""
	case errors.Is(err, state.ErrImportRunning):
		return http.StatusConflict, "an import is already running; wait for it to finish or cancel it"
	case errors.Is(err, state.ErrImportsFull):
		return http.StatusConflict, "the server is busy with other imports; try again in a few minutes"
	}
	return http.StatusServiceUnavailable, "imports are unavailable right now; try again in a moment"
}

// claimImport makes job the user's import, run here, unless importRefusal refuses; undo
// gives the claim back when the grant then cannot be spent.
func (s *Server) claimImport(job *importJob) (status int, refusal string, undo func()) {
	if s.shared == nil {
		return s.claimImportHere(job)
	}
	ctx, cancel := sharedContext()
	defer cancel()
	if status, refusal := importBusyRefusal(s.shared.ClaimImport(ctx, job.user, s.instanceID, encodeImportRow(*job), maxImports)); status != 0 {
		return status, refusal, nil
	}
	s.importMu.Lock()
	if s.imports == nil {
		s.imports = map[string]*importJob{}
	}
	s.imports[job.user] = job
	s.importMu.Unlock()
	go s.followImport(job)
	return 0, "", func() {
		s.importMu.Lock()
		job.State = "idle"
		s.importMu.Unlock()
		s.publishImport(job)
	}
}

// claimImportHere is claimImport in memory: the slots and the user's job are this process's.
func (s *Server) claimImportHere(job *importJob) (int, string, func()) {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	if busy := s.importBusy(job.user); busy != "" {
		return http.StatusConflict, busy, nil
	}
	if s.imports == nil {
		s.imports = map[string]*importJob{}
	}
	prev := s.imports[job.user]
	s.imports[job.user] = job
	return 0, "", func() {
		s.importMu.Lock()
		defer s.importMu.Unlock()
		if s.imports[job.user] == job {
			s.imports[job.user] = prev
		}
	}
}

// publishImport writes job's status row on PostgreSQL and reports whether a cancel was asked
// for. Writes are ordered, so a finished job's row is never overwritten by an older one.
func (s *Server) publishImport(job *importJob) bool {
	if s.shared == nil {
		return false
	}
	s.importPublishMu.Lock()
	defer s.importPublishMu.Unlock()
	s.importMu.Lock()
	snap := *job
	s.importMu.Unlock()
	ctx, cancel := sharedContext()
	defer cancel()
	cancelled, _, err := s.shared.PutImport(ctx, snap.user, s.instanceID, encodeImportRow(snap), snap.active())
	if err != nil {
		s.logger.Warn("mail import status not written", "actor", snap.user, "error", err.Error())
	}
	return cancelled
}

// followImport writes a job's progress while it is active and stops it when a cancel is asked
// for on any replica.
func (s *Server) followImport(job *importJob) {
	t := time.NewTicker(importPublishEvery)
	defer t.Stop()
	for range t.C {
		s.importMu.Lock()
		active := job.active()
		s.importMu.Unlock()
		if !active {
			return
		}
		if s.publishImport(job) {
			s.importMu.Lock()
			if job.cancel != nil {
				job.cancel()
			}
			s.importMu.Unlock()
		}
	}
}

// importStatus is the user's latest import, from any replica on PostgreSQL.
func (s *Server) importStatus(userID string) (importJob, error) {
	status := importJob{State: "idle"}
	if s.shared == nil {
		s.importMu.Lock()
		defer s.importMu.Unlock()
		if j := s.imports[userID]; j != nil {
			status = *j
		}
		return status, nil
	}
	ctx, cancel := sharedContext()
	defer cancel()
	row, found, err := s.shared.Import(ctx, userID)
	if err != nil || !found {
		return status, err
	}
	if status, err = decodeImportRow(row.Data, userID); err != nil {
		return importJob{State: "idle"}, err
	}
	if row.Active && !row.Live {
		status.State, status.Error = "interrupted", importInterrupted
	}
	return status, nil
}

// requestImportCancel stops the user's running import, wherever it runs, and returns its
// status; ok is false when none is running.
func (s *Server) requestImportCancel(userID string) (importJob, bool, error) {
	if s.shared == nil {
		s.importMu.Lock()
		defer s.importMu.Unlock()
		j := s.imports[userID]
		if j == nil || j.State != "running" || j.cancel == nil {
			return importJob{}, false, nil
		}
		j.cancel()
		return *j, true, nil
	}
	ctx, cancel := sharedContext()
	defer cancel()
	row, found, err := s.shared.Import(ctx, userID)
	if err != nil || !found || !row.Active || !row.Live {
		return importJob{}, false, err
	}
	status, err := decodeImportRow(row.Data, userID)
	if err != nil || status.State != "running" {
		return importJob{}, false, err
	}
	if ok, err := s.shared.CancelImport(ctx, userID, row.Instance); err != nil || !ok {
		return importJob{}, false, err
	}
	if row.Instance == s.instanceID {
		s.importMu.Lock()
		if j := s.imports[userID]; j != nil && j.cancel != nil {
			j.cancel()
		}
		s.importMu.Unlock()
	}
	return status, true, nil
}

// sweepImportUploads removes uploads no job can still use: all of them on one process; on
// replicas only loose files and the folders of instances that are not live, never a live
// replica's upload in progress.
func (s *Server) sweepImportUploads() {
	dir := filepath.Join(s.stateDir, importDir)
	if s.instanceID == "" {
		if err := os.RemoveAll(dir); err != nil {
			s.logger.Error("leftover import uploads could not be removed", "error", err.Error())
		}
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.logger.Error("leftover import uploads could not be read", "error", err.Error())
		}
		return
	}
	for _, e := range entries {
		if e.IsDir() && s.instanceLive != nil {
			ctx, cancel := sharedContext()
			live, err := s.instanceLive(ctx, e.Name())
			cancel()
			if err != nil || live {
				continue // a live replica's, or undecidable: kept
			}
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			s.logger.Error("leftover import upload could not be removed", "error", err.Error())
		}
	}
}

// imapSecret is an IMAP grant's sealed provider password, bound to its user and grant so a
// sealed value moved to another row does not open.
type imapSecret struct {
	User     string `json:"u"`
	Grant    string `json:"g"`
	Password []byte `json:"p"`
}

func imapImportKey() ([]byte, error) {
	return cryptutil.LoadOrCreateKey(filepath.Join(config.SecretDir(), "imap-import.key"))
}

func sealIMAPPassword(user, token string, password []byte) ([]byte, error) {
	key, err := imapImportKey()
	if err != nil {
		return nil, err
	}
	return sealJSON(key, imapSecret{user, sessionHash(token), password})
}

func openIMAPPassword(user, token string, sealed []byte) ([]byte, error) {
	key, err := imapImportKey()
	if err != nil {
		return nil, err
	}
	var v imapSecret
	if err := openJSON(key, sealed, &v); err != nil {
		return nil, err
	}
	if v.User != user || v.Grant != sessionHash(token) {
		clear(v.Password)
		return nil, errors.New("sealed IMAP password belongs to another grant")
	}
	return v.Password, nil
}
