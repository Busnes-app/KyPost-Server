package config

import (
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaxOutOfOfficeRunes bounds each reply text (docs/OUT_OF_OFFICE.md).
const MaxOutOfOfficeRunes = 4000

// Remote reply modes.
const (
	RemoteOff      = "off"
	RemoteOn       = "on"
	RemoteContacts = "contacts"
)

// OutOfOffice is the user's automatic reply. Texts are plain text; Start and
// End are RFC 3339 UTC instants or "". Period is server-set: it changes
// whenever the reply is turned on or its dates move, which restarts the
// once-per-sender window.
type OutOfOffice struct {
	Enabled       bool   `yaml:"enabled" json:"enabled"`
	Start         string `yaml:"start,omitempty" json:"start"`
	End           string `yaml:"end,omitempty" json:"end"`
	TimeZone      string `yaml:"timeZone,omitempty" json:"timeZone"`
	LocalMessage  string `yaml:"localMessage,omitempty" json:"localMessage"`
	Remote        string `yaml:"remote,omitempty" json:"remote"`
	RemoteMessage string `yaml:"remoteMessage,omitempty" json:"remoteMessage"`
	Period        string `yaml:"period,omitempty" json:"-"`
	// CalendarAway shows the dated period as an out-of-office event in
	// KyCalendar; CalendarTentative also answers new invitations TENTATIVE
	// during it (docs/KYCALENDAR.md, Out of office).
	CalendarAway      bool `yaml:"calendarAway,omitempty" json:"calendarAway"`
	CalendarTentative bool `yaml:"calendarTentative,omitempty" json:"calendarTentative"`
	// CalendarRemoteTexts are the remote comments KyCalendar may still hold:
	// server-only, kept so a stale one can be refused at send time.
	CalendarRemoteTexts []OOORemoteText `yaml:"calendarRemoteTexts,omitempty" json:"-"`
}

// OOORemoteText is a remote comment sent to KyCalendar, with the newest sync
// revision that carried it and, once a newer state was applied, when.
type OOORemoteText struct {
	Text       string `yaml:"text"`
	Revision   int64  `yaml:"revision"`
	Superseded int64  `yaml:"superseded,omitempty"` // unix ms; 0 while KyCalendar may still use it
}

// MaxOOORemoteTexts bounds CalendarRemoteTexts; past it a sync carrying a new
// text is refused rather than sent unrevocably.
const MaxOOORemoteTexts = 32

// OOORemoteRetention is how long a superseded text stays refused: KyCalendar
// abandons an unsent REPLY 24 hours after building it (its outboxGiveUp),
// plus an hour for clock skew.
const OOORemoteRetention = 25 * time.Hour

// NoteOOORemoteSent records that the sync at rev carries text, before it is
// sent; false when the list is full.
func NoteOOORemoteSent(list []OOORemoteText, text string, rev int64) ([]OOORemoteText, bool) {
	for i := range list {
		if list[i].Text == text {
			list[i].Revision, list[i].Superseded = max(list[i].Revision, rev), 0
			return list, true
		}
	}
	if len(list) >= MaxOOORemoteTexts {
		return list, false
	}
	return append(list, OOORemoteText{Text: text, Revision: rev}), true
}

// ForgetOOORemoteBefore marks superseded the texts no sync at or after rev
// carried, once KyCalendar applied rev (it keeps only the newest revision, so
// answers may arrive in any order), and drops those superseded longer than
// OOORemoteRetention ago, when no queued REPLY can still carry them.
func ForgetOOORemoteBefore(list []OOORemoteText, rev int64, now time.Time) []OOORemoteText {
	ms := now.UnixMilli()
	for i := range list {
		if list[i].Revision < rev && list[i].Superseded == 0 {
			list[i].Superseded = ms
		}
	}
	return slices.DeleteFunc(list, func(t OOORemoteText) bool {
		return t.Superseded != 0 && ms-t.Superseded > OOORemoteRetention.Milliseconds()
	})
}

// RemoteMode is Remote with "" read as off.
func (o OutOfOffice) RemoteMode() string {
	if o.Remote == "" {
		return RemoteOff
	}
	return o.Remote
}

// Window is the parsed period; a zero time is an open end.
func (o OutOfOffice) Window() (start, end time.Time) {
	start, _ = time.Parse(time.RFC3339, o.Start)
	end, _ = time.Parse(time.RFC3339, o.End)
	return start, end
}

// ActiveAt reports whether replies go out at now.
func (o OutOfOffice) ActiveAt(now time.Time) bool {
	if !o.Enabled {
		return false
	}
	start, end := o.Window()
	return (start.IsZero() || !now.Before(start)) && (end.IsZero() || now.Before(end))
}

// NormalizeOutOfOffice validates next against prev and returns what to store,
// with Period carried or renewed. remoteAllowed is the administrator switch:
// while it is off the remote mode may still be stored, but turning the reply
// on needs a text for a reply that would actually be sent.
func NormalizeOutOfOffice(prev, next OutOfOffice, remoteAllowed bool) (OutOfOffice, error) {
	out := OutOfOffice{Enabled: next.Enabled, Remote: next.RemoteMode(), CalendarAway: next.CalendarAway, CalendarTentative: next.CalendarAway && next.CalendarTentative}
	switch out.Remote {
	case RemoteOff, RemoteOn, RemoteContacts:
	default:
		return prev, errors.New("remote must be off, on or contacts")
	}
	var err error
	if out.Start, err = normalInstant(next.Start); err != nil {
		return prev, errors.New("start must be an RFC 3339 time")
	}
	if out.End, err = normalInstant(next.End); err != nil {
		return prev, errors.New("end must be an RFC 3339 time")
	}
	if start, end := out.Window(); !start.IsZero() && !end.IsZero() && !end.After(start) {
		return prev, errors.New("end must be after start")
	}
	out.TimeZone = strings.TrimSpace(next.TimeZone)
	if out.TimeZone != "" {
		if _, err := time.LoadLocation(out.TimeZone); err != nil || len(out.TimeZone) > 64 || out.TimeZone == "Local" {
			return prev, errors.New("timeZone must be an IANA time zone")
		}
	}
	if out.LocalMessage, err = replyText(next.LocalMessage); err != nil {
		return prev, fmt.Errorf("local message: %w", err)
	}
	if out.RemoteMessage, err = replyText(next.RemoteMessage); err != nil {
		return prev, fmt.Errorf("remote message: %w", err)
	}
	if out.Enabled {
		remoteSends := remoteAllowed && out.Remote != RemoteOff
		if strings.TrimSpace(out.LocalMessage) == "" || remoteSends && strings.TrimSpace(out.RemoteMessage) == "" {
			return prev, errors.New("write the message before turning the reply on")
		}
	}
	out.Period, out.CalendarRemoteTexts = prev.Period, prev.CalendarRemoteTexts
	if out.Enabled && (!prev.Enabled || out.Start != prev.Start || out.End != prev.End || out.Period == "") {
		out.Period = rand.Text()
	}
	return out, nil
}

func normalInstant(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return "", err
	}
	return t.UTC().Format(time.RFC3339), nil
}

func replyText(v string) (string, error) {
	v = strings.ReplaceAll(v, "\r\n", "\n")
	switch {
	case !utf8.ValidString(v):
		return "", errors.New("not valid UTF-8")
	case utf8.RuneCountInString(v) > MaxOutOfOfficeRunes:
		return "", fmt.Errorf("longer than %d characters", MaxOutOfOfficeRunes)
	case strings.ContainsFunc(v, func(r rune) bool { return (unicode.IsControl(r) && r != '\n' && r != '\t') || isBidiControl(r) }):
		return "", errors.New("contains control characters")
	}
	return v, nil
}
