package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/kycalendar"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// errOOOInput marks a refusal the caller can fix (400).
var errOOOInput = errors.New("invalid out-of-office settings")

type oooInputError struct{ err error }

func (e oooInputError) Error() string { return e.err.Error() }
func (e oooInputError) Unwrap() error { return errOOOInput }

// outOfOfficeView is the wire form (docs/OUT_OF_OFFICE.md).
type outOfOfficeView struct {
	config.OutOfOffice
	Active        bool `json:"active"`
	RemoteAllowed bool `json:"remoteAllowed"`
	// CalendarAvailable: KyCalendar is paired and takes this user's period.
	CalendarAvailable bool `json:"calendarAvailable"`
	// CalendarSync answers a save: "synced", or "failed" when KyCalendar did
	// not take it; absent when KyCalendar is not in use.
	CalendarSync string `json:"calendarSync,omitempty"`
}

func (s *Server) oooRemoteAllowed() bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.OutOfOfficeRemote
}

func (s *Server) oooView(userID string, o config.OutOfOffice) outOfOfficeView {
	o.Remote = o.RemoteMode()
	_, calendar := s.oooCalendarUser(userID)
	return outOfOfficeView{OutOfOffice: o, Active: o.ActiveAt(time.Now()), RemoteAllowed: s.oooRemoteAllowed(), CalendarAvailable: calendar}
}

// oooCalendarUser is the user KyCalendar knows, when it takes their period:
// paired, their KyCalendar switch on, and a live SSO link.
func (s *Server) oooCalendarUser(userID string) (users.User, bool) {
	if !s.kycal.Available() {
		return users.User{}, false
	}
	u, err := s.users.Get(userID)
	if err != nil || u.SSOSub == "" || u.SSOIssuer == "" || u.SSOLinkRevokedAt != 0 {
		return users.User{}, false
	}
	st, err := config.LoadUserSettings(s.userSettingsPath(userID))
	return u, err == nil && st.KyCalendarOn(s.kycal.DefaultOn())
}

// syncOOOCalendar sends KyCalendar the user's whole out-of-office state
// (docs/KYCALENDAR.md, Out of office): away only while the reply is on with
// both dates and the user asked for it. KyPost is the only source; a failure
// is reported, and the next save sends it again.
func (s *Server) syncOOOCalendar(ctx context.Context, userID string, o config.OutOfOffice) string {
	u, ok := s.oooCalendarUser(userID)
	if !ok {
		return ""
	}
	rev := time.Now().UnixMilli()
	body := map[string]any{"sub": u.SSOSub, "issuer": u.SSOIssuer, "revision": rev, "away": false}
	if start, end := o.Window(); o.Enabled && o.CalendarAway && !start.IsZero() && !end.IsZero() {
		body["away"], body["start"], body["end"] = true, o.Start, o.End
		if zone, err := time.LoadLocation(o.TimeZone); o.TimeZone != "" && err == nil && midnight(start.In(zone)) && midnight(end.In(zone)) {
			body["all_day"], body["time_zone"] = true, o.TimeZone
		}
		body["tentative"], body["comment_local"] = o.CalendarTentative, o.LocalMessage
		if o.RemoteMode() == config.RemoteOn && s.oooRemoteAllowed() {
			body["comment_remote"] = o.RemoteMessage
		}
	}
	id, err := fsutil.NewUUIDv4()
	if err != nil {
		return "failed"
	}
	// Recorded before sending: whatever the answer, KyCalendar may now hold it.
	if remote, _ := body["comment_remote"].(string); remote != "" {
		noted := false
		if s.trackOOORemoteTexts(userID, func(l []config.OOORemoteText) []config.OOORemoteText {
			l, noted = config.NoteOOORemoteSent(l, remote, rev)
			return l
		}) != nil || !noted {
			return "failed" // nothing is sent that could not be revoked later
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	status, answer, err := s.kycal.Post(ctx, "/api/kypost/out-of-office", kycalendar.EventOOO, "kypost-ooo-"+id, body)
	if err != nil || status != http.StatusOK {
		s.logger.Info("kycalendar out-of-office not taken", "user_id", userID, "result", strconv.Itoa(status)+" "+kycalendar.Outcome(answer))
		return "failed"
	}
	if kycalendar.Outcome(answer) == "applied" {
		_ = s.trackOOORemoteTexts(userID, func(l []config.OOORemoteText) []config.OOORemoteText {
			return config.ForgetOOORemoteBefore(l, rev, time.Now())
		})
	}
	return "synced"
}

// trackOOORemoteTexts updates the user's CalendarRemoteTexts under the
// settings lock.
func (s *Server) trackOOORemoteTexts(userID string, change func([]config.OOORemoteText) []config.OOORemoteText) error {
	return config.UpdateUserSettings(s.userSettingsPath(userID), func(st *config.UserSettings) error {
		st.OutOfOffice.CalendarRemoteTexts = change(st.OutOfOffice.CalendarRemoteTexts)
		return nil
	})
}

func midnight(t time.Time) bool { return t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 }

// loadOutOfOffice reads the user's setting.
func (s *Server) loadOutOfOffice(userID string) (config.OutOfOffice, error) {
	st, err := config.LoadUserSettings(s.userSettingsPath(userID))
	return st.OutOfOffice, err
}

// saveOutOfOffice applies change to the stored setting under the settings
// lock and validates the result.
func (s *Server) saveOutOfOffice(userID string, change func(prev config.OutOfOffice) (config.OutOfOffice, error)) (config.OutOfOffice, error) {
	var saved config.OutOfOffice
	err := config.UpdateUserSettings(s.userSettingsPath(userID), func(st *config.UserSettings) error {
		next, err := change(st.OutOfOffice)
		if err != nil {
			return oooInputError{err}
		}
		if next, err = config.NormalizeOutOfOffice(st.OutOfOffice, next, s.oooRemoteAllowed()); err != nil {
			return oooInputError{err}
		}
		st.OutOfOffice, saved = next, next
		return nil
	})
	return saved, err
}

// handleOutOfOffice reads (session or paired device) and replaces (session)
// the caller's out-of-office reply. A dedicated administrator identity has no
// mailbox to reply from.
func (s *Server) handleOutOfOffice(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	if s.nativeMail {
		u, err := s.users.Get(ac.UserID)
		if err != nil {
			writeUserStoreError(w, err)
			return
		}
		if _, native, err := s.nativeMailAssignment(r.Context(), u.ID); u.Role == users.RoleAdmin && (!native || err != nil) {
			s.refuseNativeAdministrator(w, r, u.ID, sso.ErrNativeAdministrator)
			return
		}
	}
	if r.Method == http.MethodGet {
		o, err := s.loadOutOfOffice(ac.UserID)
		if err != nil {
			http.Error(w, "failed to read out-of-office settings", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, s.oooView(ac.UserID, o))
		return
	}
	var req config.OutOfOffice
	dec := json.NewDecoder(io.LimitReader(r.Body, 32<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid out-of-office payload"})
		return
	}
	saved, err := s.saveOutOfOffice(ac.UserID, func(config.OutOfOffice) (config.OutOfOffice, error) { return req, nil })
	switch {
	case errors.Is(err, errOOOInput):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	case err != nil:
		http.Error(w, "failed to save out-of-office settings", http.StatusInternalServerError)
		return
	}
	s.logger.Info("out-of-office settings saved", "user_id", ac.UserID)
	view := s.oooView(ac.UserID, saved)
	view.CalendarSync = s.syncOOOCalendar(r.Context(), ac.UserID, saved)
	writeJSON(w, http.StatusOK, view)
}

// resyncOOOCalendars resends every calendar-tied state after the administrator
// stops outside replies, so KyCalendar drops the stored remote comment. Best
// effort: stripRemoteOOOComment refuses the text at send time either way.
func (s *Server) resyncOOOCalendars() {
	list, err := s.users.List()
	if err != nil {
		s.logger.Error("out-of-office calendar resync: users unavailable")
		return
	}
	for _, u := range list {
		if o, err := s.loadOutOfOffice(u.ID); err == nil && len(o.CalendarRemoteTexts) > 0 {
			s.syncOOOCalendar(context.Background(), u.ID, o)
		}
	}
}

// oooRemoteCommentAllowed: KyCalendar may answer outside organizers with the
// remote text now, which KyPost would have asked it to.
func (s *Server) oooRemoteCommentAllowed(o config.OutOfOffice) bool {
	return s.oooRemoteAllowed() && o.RemoteMode() == config.RemoteOn && o.CalendarAway && o.CalendarTentative && o.ActiveAt(time.Now())
}

// stripRemoteOOOComment drops from a folded iCalendar object every COMMENT
// whose text is one of the user's outside replies, for a REPLY sent while
// they may not go out (KyCalendar may still hold one synced before).
func stripRemoteOOOComment(ics []byte, texts []string) []byte {
	var want []string
	for _, t := range texts {
		if t != "" {
			want = append(want, strings.ReplaceAll(t, "\r\n", "\n"))
		}
	}
	if len(want) == 0 {
		return ics
	}
	lines := strings.Split(strings.TrimSuffix(string(ics), "\r\n"), "\r\n")
	var out []string
	for i := 0; i < len(lines); {
		j := i + 1
		logical := lines[i]
		for ; j < len(lines) && (strings.HasPrefix(lines[j], " ") || strings.HasPrefix(lines[j], "\t")); j++ {
			logical += lines[j][1:]
		}
		if !isCommentWithText(logical, want) {
			out = append(out, lines[i:j]...)
		}
		i = j
	}
	return []byte(strings.Join(out, "\r\n") + "\r\n")
}

// isCommentWithText reports whether an unfolded content line is a COMMENT
// whose TEXT value (RFC 5545 §3.3.11) is one of want.
func isCommentWithText(line string, want []string) bool {
	name, _, _ := strings.Cut(line, ":")
	name, _, _ = strings.Cut(name, ";")
	if !strings.EqualFold(name, "COMMENT") {
		return false
	}
	quoted := false
	for k, r := range line {
		switch {
		case r == '"':
			quoted = !quoted
		case r == ':' && !quoted:
			v := strings.NewReplacer(`\n`, "\n", `\N`, "\n", `\,`, ",", `\;`, ";", `\\`, `\`).Replace(line[k+1:])
			return slices.Contains(want, v)
		}
	}
	return false
}
