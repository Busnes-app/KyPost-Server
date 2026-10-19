package mailmsg

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxCalendarReplyBytes bounds one iTIP reply object.
const MaxCalendarReplyBytes = 64 << 10

// MaxITIPBytes bounds one iTIP object KyCalendar hands KyPost to mail.
const MaxITIPBytes = 1 << 20

// maxCalendarParamBytes bounds one property parameter value. Downstream
// iCalendar decoders (go-ical) decode parameter values in quadratic time.
const maxCalendarParamBytes = 1 << 10

// maxCalendarDepth bounds component nesting (VCALENDAR > VEVENT > VALARM ...).
const maxCalendarDepth = 8

// ErrCalendarReply is wrapped by every CalendarReply refusal.
var ErrCalendarReply = errors.New("invalid calendar reply")

// ErrITIP is wrapped by every ParseITIP refusal.
var ErrITIP = errors.New("invalid iTIP object")

// ITIP is what KyPost reads out of one iTIP object (RFC 5546) to decide who
// may receive it. Addresses are lowercased bare mailto: addresses; a
// non-mailto calendar address reads as "".
type ITIP struct {
	Method    string
	UID       string
	Organizer string
	Attendees []string
	Summary   string
}

// calendarScan is the structural pass shared by CalendarReply and ParseITIP.
type calendarScan struct {
	methods []string
	events  int
	// Per top-level VEVENT property values, in order.
	uids, organizers, summaries []string
	attendees                   []string
	lines                       []string
}

// scanCalendar checks that ics is one VCALENDAR in UTF-8 without control
// characters, balanced and bounded, and reads the properties ParseITIP needs.
// It is a structural check, not a full iCalendar parse, and linear in len(ics).
func scanCalendar(ics string, limit int) (calendarScan, string) {
	var c calendarScan
	if len(ics) > limit {
		return c, fmt.Sprintf("exceeds %d KiB", limit>>10)
	}
	if !utf8.ValidString(ics) {
		return c, "is not UTF-8"
	}
	text := strings.ReplaceAll(strings.ReplaceAll(ics, "\r\n", "\n"), "\r", "\n")
	c.lines = strings.Split(strings.TrimRight(text, "\n"), "\n")
	for _, line := range c.lines {
		for _, r := range line {
			if (r < 0x20 && r != '\t') || r == 0x7f {
				return c, "contains a control character"
			}
		}
		if line == "" {
			return c, "contains an empty line"
		}
	}
	if c.lines[0][0] == ' ' || c.lines[0][0] == '\t' {
		return c, "starts with a continuation line"
	}
	// RFC 5545 §3.1: unfold (drop one leading space or tab, join to the
	// previous line) before checking; the folded lines are what is sent.
	var logical []string
	var b strings.Builder
	for i, line := range c.lines {
		if line[0] == ' ' || line[0] == '\t' {
			b.WriteString(line[1:])
		} else {
			if i > 0 {
				logical = append(logical, b.String())
			}
			b.Reset()
			b.WriteString(line)
		}
	}
	logical = append(logical, b.String())

	var stack []string
	for i, line := range logical {
		name, params, value, ok := splitContentLine(line)
		if !ok {
			return c, "has a line without a value"
		}
		for _, p := range params {
			if len(p) > maxCalendarParamBytes {
				return c, "has a parameter value over 1 KiB"
			}
		}
		upper := strings.ToUpper(strings.TrimSpace(value))
		if i == 0 && (name != "BEGIN" || upper != "VCALENDAR") {
			return c, "must start with BEGIN:VCALENDAR"
		}
		if len(stack) == 0 && i != 0 {
			return c, "has content after END:VCALENDAR"
		}
		inEvent := len(stack) == 2 && stack[1] == "VEVENT"
		switch name {
		case "BEGIN":
			if len(stack) >= maxCalendarDepth {
				return c, "is nested too deeply"
			}
			if upper == "VCALENDAR" && len(stack) > 0 {
				return c, "holds more than one VCALENDAR"
			}
			if upper == "VEVENT" {
				c.events++
			}
			stack = append(stack, upper)
		case "END":
			if len(stack) == 0 || stack[len(stack)-1] != upper {
				return c, "has unbalanced BEGIN/END"
			}
			stack = stack[:len(stack)-1]
		case "METHOD":
			if len(stack) != 1 {
				return c, "has METHOD outside the VCALENDAR"
			}
			c.methods = append(c.methods, upper)
		case "UID":
			if inEvent {
				c.uids = append(c.uids, strings.TrimSpace(value))
			}
		case "ORGANIZER":
			if inEvent {
				c.organizers = append(c.organizers, calendarAddress(value))
			}
		case "ATTENDEE":
			if inEvent {
				c.attendees = append(c.attendees, calendarAddress(value))
			}
		case "SUMMARY":
			if inEvent {
				c.summaries = append(c.summaries, unescapeText(value))
			}
		}
	}
	if len(stack) != 0 {
		return c, "must end with END:VCALENDAR"
	}
	return c, ""
}

// splitContentLine splits "NAME;P=v;Q=\"a:b\":value" (RFC 5545 §3.1) at the
// first colon outside a quoted parameter value, returning the uppercased name
// and each parameter value.
func splitContentLine(line string) (name string, params []string, value string, ok bool) {
	quoted, seg := false, 0
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '"' {
			quoted = !quoted
			continue
		}
		if quoted || (c != ';' && c != ':') {
			continue
		}
		if part := line[seg:i]; seg == 0 {
			name = strings.ToUpper(part)
		} else {
			_, v, _ := strings.Cut(part, "=")
			params = append(params, v)
		}
		seg = i + 1
		if c == ':' {
			return name, params, line[i+1:], true
		}
	}
	return "", nil, "", false
}

// calendarAddress reads a mailto: CAL-ADDRESS as a lowercased bare address.
func calendarAddress(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 7 || !strings.EqualFold(value[:7], "mailto:") {
		return ""
	}
	a, err := mail.ParseAddress(value[7:])
	if err != nil || a.Name != "" || a.Address != strings.TrimSpace(value[7:]) {
		return ""
	}
	return strings.ToLower(a.Address)
}

// unescapeText undoes RFC 5545 §3.3.11 TEXT escapes.
func unescapeText(v string) string {
	r := strings.NewReplacer(`\\`, `\`, `\;`, ";", `\,`, ",", `\n`, " ", `\N`, " ")
	return r.Replace(v)
}

// CalendarReply checks that ics is one iTIP REPLY (RFC 5546): a single
// VCALENDAR whose METHOD is REPLY, holding exactly one VEVENT, in UTF-8 with
// no control characters, and returns it CRLF-normalised. It is a structural
// check, not a full iCalendar parse; the organizer's calendar validates the
// rest. Build base64-encodes the bytes, so no content can reach a header.
func CalendarReply(ics string) ([]byte, error) {
	c, reason := scanCalendar(ics, MaxCalendarReplyBytes)
	switch {
	case reason != "":
	case len(c.methods) != 1 || c.methods[0] != "REPLY":
		reason = "needs exactly one METHOD:REPLY"
	case c.events != 1:
		reason = "needs exactly one VEVENT"
	}
	if reason != "" {
		return nil, fmt.Errorf("%w: calendar reply %s", ErrCalendarReply, reason)
	}
	return []byte(strings.Join(c.lines, "\r\n") + "\r\n"), nil
}

// ParseITIP checks that ics is one iTIP object whose single METHOD is method
// (REQUEST, CANCEL or REPLY), whose VEVENTs (one or more: a series and its
// overrides) share one UID and one ORGANIZER, and returns what it names plus
// the CRLF-normalised bytes to mail.
func ParseITIP(ics, method string) (ITIP, []byte, error) {
	c, reason := scanCalendar(ics, MaxITIPBytes)
	it := ITIP{Method: method}
	if reason == "" {
		reason = c.itip(&it)
	}
	if reason != "" {
		return ITIP{}, nil, fmt.Errorf("%w: calendar %s", ErrITIP, reason)
	}
	return it, []byte(strings.Join(c.lines, "\r\n") + "\r\n"), nil
}

func (c calendarScan) itip(it *ITIP) string {
	switch {
	case len(c.methods) != 1 || c.methods[0] != it.Method:
		return "needs exactly one METHOD matching the request"
	case c.events == 0:
		return "needs a VEVENT"
	case len(c.uids) != c.events || len(c.organizers) != c.events:
		return "needs one UID and one ORGANIZER in every VEVENT"
	}
	it.UID, it.Organizer = c.uids[0], c.organizers[0]
	for i := range c.uids {
		if c.uids[i] != it.UID || c.organizers[i] != it.Organizer {
			return "VEVENTs must share one UID and ORGANIZER"
		}
	}
	if it.UID == "" || it.Organizer == "" {
		return "needs a UID and a mailto: ORGANIZER"
	}
	for _, a := range c.attendees {
		if a != "" {
			it.Attendees = append(it.Attendees, a)
		}
	}
	if len(c.summaries) > 0 {
		it.Summary = c.summaries[0]
	}
	return ""
}

// ITIPSubject is the Subject for a mailed iTIP object: its SUMMARY without
// control characters, cut to 200 bytes on a rune boundary, else a fallback
// naming the method.
func ITIPSubject(it ITIP) string {
	s := strings.Join(strings.FieldsFunc(it.Summary, unicode.IsControl), " ")
	s = strings.TrimSpace(s)
	for len(s) > 200 {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	if s != "" {
		return s
	}
	switch it.Method {
	case "CANCEL":
		return "Cancelled"
	case "REPLY":
		return "Reply"
	}
	return "Invitation"
}
