package api

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// EnableNativeMail is startup-only, before request/background goroutines start.
func (s *Server) EnableNativeMail() { s.nativeMail = true }

// refuseNativeAdministrator answers the administrator-separation refusal with
// 403, never 401: the identity is valid, so a client must not drop its
// session or credential. It reports false for any other error.
func (s *Server) refuseNativeAdministrator(w http.ResponseWriter, r *http.Request, userID string, err error) bool {
	if !errors.Is(err, sso.ErrNativeAdministrator) {
		return false
	}
	// The route pattern, never the path: a path can carry a download token.
	s.logger.Info("native mail refused for administrator identity", "actor", userID, "action", r.Method, "target", r.Pattern, "result", "refused")
	writeJSON(w, http.StatusForbidden, map[string]any{"error": "administrator identities have no mailbox; use your everyday identity", "administratorIdentity": true})
	return true
}

// refuseAdministratorIMAP keeps native-mode administrators off ordinary mail
// setup. Stored IMAP configuration is left as is: removing it belongs to the
// mixed-use migration. IMAP-only deployments are unaffected.
func (s *Server) refuseAdministratorIMAP(w http.ResponseWriter, r *http.Request, userID string) bool {
	if !s.nativeMail {
		return false
	}
	u, err := s.users.Get(userID)
	if err != nil {
		writeUserStoreError(w, err)
		return true
	}
	return u.Role == users.RoleAdmin && s.refuseNativeAdministrator(w, r, userID, sso.ErrNativeAdministrator)
}

var errNativeRelayUnavailable = errors.New("native sending requires an administrator-configured domain relay; sending is not enabled yet")

// All outbound configuration consumers share this refusal. Installing an IMAP
// credential file cannot make a native account send through a legacy path.
func (s *Server) outboundMailConfig(userID string) (mailmsg.IMAPConfigPayload, bool, error) {
	if s.users != nil {
		u, err := s.users.Get(userID)
		if err != nil {
			return mailmsg.IMAPConfigPayload{}, false, err
		}
		if u.NativeMailboxIssuer != "" || u.NativeMailboxSource != "" {
			return mailmsg.IMAPConfigPayload{}, false, errNativeRelayUnavailable
		}
	}
	return mailmsg.ReadIMAPConfigPayload(s.userIMAPConfigPath(userID), s.imapConfigKeyPath)
}

func outboundConfigError(w http.ResponseWriter, err error) {
	if errors.Is(err, errNativeRelayUnavailable) {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	http.Error(w, "failed to read mail configuration", http.StatusInternalServerError)
}

func (s *Server) nativeMailAssignment(ctx context.Context, userID string) (sso.NativeAssignment, bool, error) {
	return s.nativeMailboxAssignment(ctx, userID, "")
}

// nativeMailboxAssignment is sso.AdmitUserMailbox ("" is the primary).
func (s *Server) nativeMailboxAssignment(ctx context.Context, userID, mailboxID string) (sso.NativeAssignment, bool, error) {
	return sso.AdmitUserMailbox(ctx, s.nativeMail, s.ssoStore, s.ssoLifecycle, s.users, s.stateDir, userID, mailboxID)
}

// The client cache is keyed by mailbox ID (a primary's is its user ID).
func (s *Server) nativeMailboxClient(userID, mailboxID string) (imapadapter.Client, bool, error) {
	a, native, err := s.nativeMailboxAssignment(context.Background(), userID, mailboxID)
	if err != nil || !native {
		return nil, native, err
	}
	key := a.Owner.Mailbox
	s.userMu.Lock()
	defer s.userMu.Unlock()
	if entry, ok := s.userMail[key]; ok && entry.updatedAt == a.Source {
		return entry.client, true, nil
	}
	client, err := mailbox.OpenClient(filepath.Join(a.Dir(s.stateDir), "mailbox"), a.Owner, a.Limits, a.Address, a.Source, func(ctx context.Context) error {
		current, _, err := s.nativeMailboxAssignment(ctx, userID, mailboxID)
		if err != nil {
			return err
		}
		if current.Source != a.Source {
			return sso.ErrNativeProvisioning
		}
		return nil
	})
	if err != nil {
		return nil, true, err
	}
	if entry, ok := s.userMail[key]; ok {
		closeMailClient(entry.client)
	}
	s.userMail[key] = &serverMailEntry{client: client, updatedAt: a.Source}
	return client, true, nil
}
