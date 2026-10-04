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
)

// EnableNativeMail is startup-only, before request/background goroutines start.
func (s *Server) EnableNativeMail() { s.nativeMail = true }

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
	u, err := s.users.Get(userID)
	if err != nil {
		return sso.NativeAssignment{}, false, err
	}
	if u.NativeMailboxIssuer == "" && u.NativeMailboxSource == "" {
		return sso.NativeAssignment{}, false, nil
	}
	if !s.nativeMail {
		return sso.NativeAssignment{}, true, errors.New("native mailbox mode is disabled; enable KYPOST_NATIVE_MAIL explicitly")
	}
	settings := s.ssoStore.Load()
	if !settings.Enabled {
		return sso.NativeAssignment{}, true, sso.ErrNativeProvisioning
	}
	a, err := s.ssoLifecycle.AdmitNativeMail(ctx, s.stateDir, settings.IssuerURL, userID, s.users)
	return a, true, err
}

func (s *Server) nativeMailboxClient(userID string) (imapadapter.Client, bool, error) {
	a, native, err := s.nativeMailAssignment(context.Background(), userID)
	if err != nil || !native {
		return nil, native, err
	}
	s.userMu.Lock()
	defer s.userMu.Unlock()
	if entry, ok := s.userMail[userID]; ok && entry.updatedAt == a.Source {
		return entry.client, true, nil
	}
	client, err := mailbox.OpenClient(filepath.Join(s.userStateDir(userID), "mailbox"), a.Owner, a.Limits, a.Address, a.Source, func(ctx context.Context) error {
		current, _, err := s.nativeMailAssignment(ctx, userID)
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
	if entry, ok := s.userMail[userID]; ok {
		closeMailClient(entry.client)
	}
	s.userMail[userID] = &serverMailEntry{client: client, updatedAt: a.Source}
	return client, true, nil
}
