package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
)

// The multi-mode restore release (docs/superpowers/specs/2026-11-20-tenancy-design.md,
// "Delete everything" and "PR 6 as built", 6c). A restore empties the orgs cache
// (tenancy.FenceRestored) and leaves native mail held. While the hold stands, each poll
// reads every org from revision 0 and every erased subject, starting the purges they call
// for; once both succeed for this hold, restore_reads records it, and only then may staff
// release the hold.

// erasedSubject is one entry of KyIdentity's GET /api/suite/erased.
type erasedSubject struct {
	Sub      string `json:"sub"`
	OrgID    string `json:"org_id"`
	Revision int64  `json:"revision"`
}

// maxErasedPages bounds one read: 100 pages of 500, as suitedir.Orgs bounds its own.
// ponytail: past 50,000 erased subjects the read fails and the hold cannot be released;
// page it across polls with a stored cursor before any install nears that.
const maxErasedPages = 100

// erasedPurgeError is an erased subject whose purge cannot start, so the restore read is not
// recorded and the hold stays. It names the entry by its revision, never by subject or org.
type erasedPurgeError struct {
	revision int64
	err      error
}

func (e *erasedPurgeError) Error() string {
	return fmt.Sprintf("erased subject at revision %d: purge cannot start: %v", e.revision, e.err)
}

func (e *erasedPurgeError) Unwrap() error { return e.err }

// errErasedMalformed refuses a whole answer with any entry KyPost would not apply. It
// quotes no entry: a subject or org ID is not logged.
var errErasedMalformed = errors.New("suite directory: erased: malformed answer")

// suiteErased reads every erased subject from revision 0, page by page, with the app's
// client credentials, as suitedir does for /api/suite/orgs: HTTPS only (loopback HTTP for
// tests), no redirects, a bounded body, and a malformed entry fails the whole read.
// ponytail: suitedir has no Erased method yet; this moves there when it does.
func suiteErased(ctx context.Context, dir suitedir.Client) ([]erasedSubject, error) {
	base, err := url.Parse(strings.TrimRight(dir.Issuer, "/"))
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" ||
		base.Scheme != "https" && (base.Scheme != "http" || !loopbackHost(base.Hostname())) {
		return nil, suitedir.ErrInsecureIssuer
	}
	client := http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("redirects are refused")
	}}
	var out []erasedSubject
	since := int64(0)
	for page := 0; page < maxErasedPages; page++ {
		u := base.JoinPath("api", "suite", "erased")
		u.RawQuery = url.Values{"since": {strconv.FormatInt(since, 10)}, "limit": {"500"}}.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(dir.ClientID, dir.ClientSecret)
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			var uerr *url.Error
			if errors.As(err, &uerr) {
				err = uerr.Err
			}
			return nil, fmt.Errorf("suite directory: erased: %w", err)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
		_ = resp.Body.Close()
		switch {
		case err != nil:
			return nil, fmt.Errorf("suite directory: erased: %w", err)
		case resp.StatusCode == http.StatusUnauthorized:
			return nil, suitedir.ErrUnauthorized
		case resp.StatusCode != http.StatusOK:
			return nil, fmt.Errorf("suite directory: erased: status %d", resp.StatusCode)
		case len(raw) > 1<<20:
			return nil, errErasedMalformed
		}
		var body struct {
			Subjects []erasedSubject `json:"subjects"`
			Next     string          `json:"next"`
		}
		if json.Unmarshal(raw, &body) != nil || body.Subjects == nil {
			return nil, errErasedMalformed
		}
		for _, e := range body.Subjects {
			if e.Sub == "" || len(e.Sub) > 255 || !config.ValidOrgID(e.OrgID) || e.Revision <= since {
				return nil, errErasedMalformed
			}
			since = e.Revision
		}
		out = append(out, body.Subjects...)
		if body.Next == "" || len(body.Subjects) == 0 {
			return out, nil
		}
		if next, err := strconv.ParseInt(body.Next, 10, 64); err != nil || next != since {
			return nil, errErasedMalformed
		}
	}
	return nil, errors.New("suite directory: erased: more pages than one read allows")
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// restoreReads runs while a restore hold stands: a poll of every org from revision 0, then
// every erased subject, each starting the purge it calls for (a deleted org's by the purge
// runner, an erased member's here). Only when both succeed is the hold's epoch recorded;
// after that the ordinary incremental poll resumes, and later erasures arrive as events.
func (s *Server) restoreReads(ctx context.Context, epoch string, now time.Time) error {
	if read, err := tenancy.RestoreRead(ctx, s.tenancy, epoch); err != nil {
		return err
	} else if read {
		return s.pollOrgs(ctx, now)
	}
	dir, ok := s.ssoStore.Load().SuiteDirectory()
	if !ok {
		return errNoSuiteDirectory
	}
	if err := s.pollOrgsFrom(ctx, dir, 0, now); err != nil {
		return err
	}
	erased, err := suiteErased(ctx, dir)
	if err != nil {
		return err
	}
	for _, e := range erased {
		if err := s.beginPurge(ctx, tenancy.PurgeScope{Org: e.OrgID, Subject: e.Sub}, now); err != nil {
			return &erasedPurgeError{revision: e.Revision, err: err}
		}
	}
	return tenancy.RecordRestoreRead(ctx, s.tenancy, epoch, now)
}

// mailboxPurged is LifecycleStore.Purged. A single install has no purges.
func (s *Server) mailboxPurged(id string) (bool, error) {
	if s.tenancy == nil {
		return false, nil
	}
	return tenancy.MailboxPurged(context.Background(), s.tenancy, id)
}

// heldEpoch is the restore hold's epoch, "" when no hold stands. An unreadable hold is
// still a hold: its reads cannot be recorded, so it is never released this way.
func (s *Server) heldEpoch() (epoch string, held bool) {
	if sso.RequireNativeRestoreReleased(s.stateDir) == nil {
		return "", false
	}
	epoch, _ = sso.NativeRestoreHoldEpoch(s.stateDir)
	return epoch, true
}

// restoreReleaseRefusal is why multi mode refuses to release the hold yet, "" when it may.
func (s *Server) restoreReleaseRefusal(ctx context.Context) (string, error) {
	const why = "KyIdentity has not been read in full since this restore: the hold stays until a poll of every org from revision 0 and the erased-subject list both succeed"
	epoch, held := s.heldEpoch()
	if !held || epoch == "" {
		return why, nil
	}
	read, err := tenancy.RestoreRead(ctx, s.tenancy, epoch)
	if err != nil || read {
		return "", err
	}
	return why, nil
}
