package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// The org purge (docs/superpowers/specs/2026-11-20-tenancy-design.md, "Purge (`deleted`)",
// "Delete everything" and "PR 6 as built"). It runs on the api-sweepers owner after each
// org poll, only in multi-tenant mode. Every step deletes whatever of the snapshot still
// exists, so a purge killed anywhere resumes by running every step again.

// errPurgeLeft refuses to mark a purge done while its final check finds something.
var errPurgeLeft = errors.New("the purge's final check found data of the org")

// purgeStepHook is a test seam: called before step i (1-based) runs, and with -i after it
// ran and before its progress is recorded. A non-nil error stops the purge there.
var purgeStepHook func(step int) error

// runPurges starts a purge for every deleted org without one, then runs every live purge.
func (s *Server) runPurges(ctx context.Context, now time.Time) error {
	orgs, err := tenancy.DeletedOrgs(ctx, s.tenancy)
	if err != nil {
		return err
	}
	var errs []error
	for _, org := range orgs {
		if err := s.beginPurge(ctx, tenancy.PurgeScope{Org: org}, now); err != nil {
			errs = append(errs, err)
		}
	}
	live, err := tenancy.LivePurges(ctx, s.tenancy)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, scope := range live {
		if err := s.purge(ctx, scope, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// beginPurge snapshots scope and records its row, once. The staff org is never purged.
func (s *Server) beginPurge(ctx context.Context, scope tenancy.PurgeScope, now time.Time) error {
	if scope.Org == s.staffOrg || !config.ValidOrgID(scope.Org) {
		return nil
	}
	if done, err := tenancy.Purged(ctx, s.tenancy, scope.Org, scope.Subject); err != nil || done {
		return err
	}
	snap, err := s.purgeSnapshot(ctx, scope)
	if err != nil {
		return err
	}
	_, err = tenancy.StartPurge(ctx, s.tenancy, scope, snap, now)
	return err
}

// purgeSnapshot is the union K names, so a crash between provisioning's three writes
// cannot hide an account: user_orgs, the users.json accounts bound to the org, and the
// accounts of the org's subject_orgs subjects; then every ledger mailbox of those
// subjects. A one-user scope takes only that subject and its account. An account bound to
// another org is never taken.
func (s *Server) purgeSnapshot(ctx context.Context, scope tenancy.PurgeScope) (tenancy.PurgeSnapshot, error) {
	var snap tenancy.PurgeSnapshot
	subjects, err := tenancy.OrgSubjects(ctx, s.tenancy, scope.Org, scope.Subject)
	if err != nil {
		return snap, err
	}
	all, err := s.users.List()
	if err != nil {
		return snap, err
	}
	ids := map[string]bool{}
	if scope.Subject == "" {
		bound, err := tenancy.OrgUsers(ctx, s.tenancy, scope.Org)
		if err != nil {
			return snap, err
		}
		for _, id := range bound {
			ids[id] = true
		}
	}
	for _, u := range all {
		subject := tenancy.Subject{Issuer: u.SSOIssuer, Subject: u.SSOSub}
		mine := u.OrgID == scope.Org && (scope.Subject == "" || u.SSOSub == scope.Subject)
		if u.OrgID != "" && u.OrgID != scope.Org || !mine && !slices.Contains(subjects, subject) {
			if ids[u.ID] && u.OrgID != scope.Org {
				return snap, errors.New("a user_orgs row names an account of another org; repair the bindings before the purge")
			}
			continue
		}
		ids[u.ID] = true
		if u.SSOSub != "" && !slices.Contains(subjects, subject) {
			subjects = append(subjects, subject)
		}
	}
	for id := range ids {
		snap.Users = append(snap.Users, id)
	}
	slices.Sort(snap.Users)
	snap.Subjects = subjects
	if snap.Mailboxes, err = s.ssoLifecycle.NativePurgeMailboxes(ctx, purgeOwners(subjects)); errors.Is(err, sso.ErrNoNativeLedger) {
		err = nil
	}
	return snap, err
}

func purgeOwners(subjects []tenancy.Subject) []sso.PurgeOwner {
	out := make([]sso.PurgeOwner, len(subjects))
	for i, s := range subjects {
		out[i] = sso.PurgeOwner{Issuer: s.Issuer, Subject: s.Subject}
	}
	return out
}

// purge runs scope's steps from the first, then the final check, then records it done.
func (s *Server) purge(ctx context.Context, scope tenancy.PurgeScope, now time.Time) error {
	snap, err := tenancy.PurgeMembers(ctx, s.tenancy, scope)
	if err != nil {
		return err
	}
	if s.globalStore == nil || s.backup == nil {
		return errors.New("the purge needs the root state store and the audit log")
	}
	key, err := tenancy.TombstoneKey(ctx, s.tenancy, config.SecretDir())
	if err != nil {
		return err
	}
	gone := func(address string) (bool, error) {
		return tenancy.Tombstoned(ctx, s.tenancy, tenancy.Tombstone(key, address))
	}
	steps := []func() error{
		func() error { return s.purgeBlock(ctx, snap) },
		func() error { return s.purgeReceiving(ctx, snap) },
		func() error { return s.purgeStorage(ctx, snap) },
		func() error { return s.purgeLedger(ctx, scope, snap, key, gone) },
		func() error { return s.purgeRoot(ctx, snap) },
	}
	for i, step := range steps {
		if purgeStepHook != nil {
			if err := purgeStepHook(i + 1); err != nil {
				return err
			}
		}
		if err := step(); err != nil {
			return fmt.Errorf("purge step %d: %w", i+1, err)
		}
		if purgeStepHook != nil {
			if err := purgeStepHook(-(i + 1)); err != nil {
				return err
			}
		}
		if err := tenancy.MarkPurgeStep(ctx, s.tenancy, scope, i+1); err != nil {
			return err
		}
	}
	left, err := s.purgeLeft(ctx, snap, gone)
	if err != nil {
		return err
	}
	for check, n := range left {
		if n > 0 {
			return fmt.Errorf("%w (%s: %d)", errPurgeLeft, check, n)
		}
	}
	scopeName := "org"
	if scope.Subject != "" {
		scopeName = "member"
	}
	if err := s.backup.Audit("tenancy.purge", "system", "org:"+scope.Org, "completed", map[string]any{
		"scope": scopeName, "accounts": len(snap.Users), "mailboxes": len(snap.Mailboxes), "subjects": len(snap.Subjects)}); err != nil {
		return err
	}
	return tenancy.FinishPurge(ctx, s.tenancy, scope, now)
}

// Step 1: end every session, delete agent tokens and scheduled sends, cancel imports.
func (s *Server) purgeBlock(ctx context.Context, snap tenancy.PurgeSnapshot) error {
	for _, id := range snap.Users {
		if err := s.revokeUserSessions(id, ""); err != nil {
			return err
		}
		if err := s.globalStore.DeleteUserAgentTokens(ctx, id); err != nil {
			return err
		}
		if err := s.globalStore.DeleteUserScheduledSends(ctx, id); err != nil {
			return err
		}
		s.importMu.Lock()
		if j := s.imports[id]; j != nil && j.cancel != nil {
			j.cancel()
		}
		s.importMu.Unlock()
	}
	return nil
}

// Step 2: the org's routes and bindings, before storage, so no pending delivery is left
// that an import could write back; then the mailboxes' import leases and retired records.
func (s *Server) purgeReceiving(ctx context.Context, snap tenancy.PurgeSnapshot) error {
	h, err := s.openHolding()
	if err != nil {
		return err
	}
	err = PurgeReceiving(ctx, h, s.tenancy, snap.Mailboxes)
	if h != nil {
		err = errors.Join(err, h.Close())
	}
	return err
}

// PurgeReceiving is purge step 2 over the ingress store h (nil when receiving was never
// initialized). A delivery left with only other, finished bindings is archived by the
// receiving daemon's Completion sweep (RX R8).
func PurgeReceiving(ctx context.Context, h *ingress.Store, db *sql.DB, mailboxes []string) error {
	if h != nil {
		if err := h.PurgeMailboxes(ctx, mailboxes); err != nil {
			return err
		}
	}
	return tenancy.PurgeImports(ctx, db, mailboxes)
}

// purgeDirs is every directory of users and mailboxes: each user's under configDir and
// stateDir, and each mailbox's (a primary one is its user's directory).
func purgeDirs(configDir, stateDir string, users, mailboxes []string) ([]string, error) {
	var dirs []string
	for _, id := range append(slices.Clone(users), mailboxes...) {
		if id == "" || !fsutil.SafePathComponent(id) {
			return nil, errors.New("the purge snapshot holds an unsafe ID")
		}
		dirs = append(dirs, config.UserDir(configDir, id), config.UserDir(stateDir, id), filepath.Join(stateDir, "mailboxes", id))
	}
	slices.Sort(dirs)
	return slices.Compact(dirs), nil
}

// Step 3: storage, then the users' pickup records.
func (s *Server) purgeStorage(ctx context.Context, snap tenancy.PurgeSnapshot) error {
	if err := PurgeStorage(ctx, s.configDir, s.stateDir, snap.Users, snap.Mailboxes); err != nil {
		return err
	}
	if s.pickupStore != nil {
		_, err := s.pickupStore.SenderRecords(snap.Users, true)
		return err
	}
	return nil
}

// PurgeStorage is purge step 3's storage: each mailbox's under its receiving quota lock,
// which fences an import write; then the users' directories (paired devices, CardDAV
// passwords, sealed IMAP and CardDAV client credentials, every per-user file).
func PurgeStorage(ctx context.Context, configDir, stateDir string, users, mailboxes []string) error {
	dirs, err := purgeDirs(configDir, stateDir, users, mailboxes)
	if err != nil {
		return err
	}
	receiving := filepath.Join(stateDir, "receiving")
	_, noReceiving := os.Lstat(receiving)
	locks := map[string]func(){}
	defer func() {
		for _, release := range locks {
			release()
		}
	}()
	if noReceiving == nil {
		for _, id := range mailboxes {
			release, err := fsutil.LockFileContext(ctx, filepath.Join(receiving, "quota-"+id))
			if err != nil {
				return err
			}
			locks[id] = release
		}
	}
	for _, dir := range dirs {
		if dbbackend.Postgres() {
			if err := dbbackend.Remove(dir); err != nil {
				return err
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	for id, release := range locks {
		release()
		delete(locks, id)
		if err := os.Remove(filepath.Join(receiving, "quota-"+id+".lock")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Step 4: tombstone every address, then scrub the ledger under the named trigger
// exception, then drop the tombstoned addresses from the receiving tombstones.
func (s *Server) purgeLedger(ctx context.Context, scope tenancy.PurgeScope, snap tenancy.PurgeSnapshot, key []byte, gone func(string) (bool, error)) error {
	if len(snap.Subjects) > 0 {
		err := s.ssoLifecycle.PurgeNative(ctx, purgeOwners(snap.Subjects),
			func() error { return tenancy.PurgeLive(ctx, s.tenancy, scope) },
			func(addresses []string) error {
				hashes := make([]string, len(addresses))
				for i, a := range addresses {
					hashes[i] = tenancy.Tombstone(key, a)
				}
				return tenancy.AddTombstones(ctx, s.tenancy, hashes)
			})
		if err != nil && !errors.Is(err, sso.ErrNoNativeLedger) {
			return err
		}
	}
	h, err := s.openHolding()
	if err != nil || h == nil {
		return err
	}
	defer h.Close()
	return h.ScrubArchived(ctx, gone)
}

// Step 5: the users' root-store rows, their users.json records, then their user_orgs rows.
func (s *Server) purgeRoot(ctx context.Context, snap tenancy.PurgeSnapshot) error {
	if err := s.globalStore.PurgeUsers(ctx, snap.Users); err != nil {
		return err
	}
	if _, err := s.users.RemoveUsers(snap.Users); err != nil {
		return err
	}
	return tenancy.RemoveUserOrgs(ctx, s.tenancy, snap.Users)
}

// purgeChecks names the final check's parts; tenancy.PurgeCoverage maps every org-owned and
// derived store of the classification to one of them.
var purgeChecks = []string{"users.json", "user_orgs", "root-state", "directories", "schemas", "pickup", "ingress", "ledger", "documents"}

// purgeLeft is the final check: what of the snapshot each part still finds.
func (s *Server) purgeLeft(ctx context.Context, snap tenancy.PurgeSnapshot, gone func(string) (bool, error)) (map[string]int, error) {
	left := map[string]int{}
	for _, check := range purgeChecks {
		left[check] = 0
	}
	var errs []error
	note := func(check string, n int, err error) {
		left[check] += n
		errs = append(errs, err)
	}
	for _, id := range snap.Users {
		if _, err := s.users.Get(id); err == nil {
			note("users.json", 1, nil)
		} else if !errors.Is(err, users.ErrNotFound) {
			note("users.json", 0, err)
		}
	}
	n, err := tenancy.UserOrgRows(ctx, s.tenancy, snap.Users)
	note("user_orgs", n, err)
	n, err = tenancy.DocumentRows(ctx, s.tenancy, snap.Users)
	note("documents", n, err)
	n, err = s.globalStore.UserRows(ctx, snap.Users)
	note("root-state", n, err)
	dirs, err := purgeDirs(s.configDir, s.stateDir, snap.Users, snap.Mailboxes)
	note("directories", 0, err)
	for _, id := range snap.Mailboxes {
		dirs = append(dirs, filepath.Join(s.stateDir, "receiving", "quota-"+id+".lock"))
	}
	for _, dir := range dirs {
		if _, err := os.Lstat(dir); err == nil {
			note("directories", 1, nil)
		} else if !errors.Is(err, os.ErrNotExist) {
			note("directories", 0, err)
		}
	}
	if dbbackend.Postgres() {
		stores, err := dbbackend.Stores(ctx)
		note("schemas", 0, err)
		for _, st := range stores {
			for _, dir := range dirs {
				if strings.HasPrefix(st.LivePath(), dir+string(filepath.Separator)) {
					note("schemas", 1, nil)
				}
			}
		}
	}
	if s.pickupStore != nil {
		n, err = s.pickupStore.SenderRecords(snap.Users, false)
		note("pickup", n, err)
	}
	if h, err := s.openHolding(); err != nil {
		note("ingress", 0, err)
	} else if h != nil {
		n, err = h.PurgeLeft(ctx, snap.Mailboxes, gone)
		note("ingress", n, errors.Join(err, h.Close()))
	}
	n, err = tenancy.ImportRows(ctx, s.tenancy, snap.Mailboxes)
	note("ingress", n, err)
	if len(snap.Subjects) > 0 {
		n, err = s.ssoLifecycle.NativePurgeLeft(ctx, purgeOwners(snap.Subjects), snap.Mailboxes)
		if errors.Is(err, sso.ErrNoNativeLedger) {
			err = nil
		}
		note("ledger", n, err)
	}
	return left, errors.Join(errs...)
}

// eraseMember applies a signed user.erased (multi-tenant only): one member's own Delete
// everything (K "Delete everything"). Their purge is recorded first, so a failure after it
// still purges; then the event applies as a removal, which revokes their credentials now.
// A fence refusal is not a failure: the purge does not depend on the revision.
func (s *Server) eraseMember(w http.ResponseWriter, r *http.Request, issuer string, event syncauth.Event, u sso.DirectoryUser, body []byte, org string) {
	if err := s.beginPurge(r.Context(), tenancy.PurgeScope{Org: org, Subject: u.ID}, time.Now()); err != nil {
		s.logger.Error("member erasure not recorded; the sender retries", "event_id", event.ID)
		http.Error(w, "directory event was not applied", http.StatusInternalServerError)
		return
	}
	removal := event
	removal.Type = "user.deleted"
	status, err := s.ssoLifecycle.ApplyDirectoryUser(issuer, removal, u, sso.EventDigest(event.Type, body), func() (bool, error) {
		return s.applyDirectoryUser(issuer, u)
	})
	var refusal *syncRefusal
	switch {
	case errors.Is(err, sso.ErrDirectoryConflict):
		status = "erased"
	case errors.As(err, &refusal):
		http.Error(w, refusal.Error(), refusal.status)
		return
	case err != nil:
		s.logger.Error("directory event was not applied", "event_id", event.ID, "error", err.Error())
		http.Error(w, "directory event was not applied", http.StatusInternalServerError)
		return
	}
	s.logger.Info("member erasure recorded", "event_id", event.ID)
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "eventId": event.ID, "version": u.Meta.Version})
}
