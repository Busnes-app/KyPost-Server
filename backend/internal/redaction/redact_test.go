package redaction

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
)

// Exact outputs of the default patterns. Positive rows must leave no digit or
// address fragment behind; negative rows are what the classifier needs intact.
// Doubled backslashes (`\\b`) once disabled every default silently, which any
// positive row here catches.
var defaultCases = []struct{ in, want string }{
	// email
	{"contact me at yoshi@example.com please", "contact me at [REDACTED_EMAIL] please"},
	{"jürgen@münchen.de", "[REDACTED_EMAIL]"},
	{"Écrire à Zoë.Ä@example.fr.", "Écrire à [REDACTED_EMAIL]."},
	{"<ivan@почта.рф>", "<[REDACTED_EMAIL]>"},
	{"a@xn--mnchen-3ya.de and b@xn--80a1acny.xn--p1ai", "[REDACTED_EMAIL] and [REDACTED_EMAIL]"},
	{"o'brien@example.ie", "[REDACTED_EMAIL]"},
	{"john_doe@example.com", "[REDACTED_EMAIL]"},
	// phone
	{"call 555-123-4567 now", "call [REDACTED_PHONE] now"},
	{"call (555) 123-4567", "call [REDACTED_PHONE]"},
	{"+1 (555) 123-4567", "[REDACTED_PHONE]"},
	{"+44 20 7946 0958", "[REDACTED_PHONE]"},
	{"+44 (0)20 7946 0958", "[REDACTED_PHONE]"},
	{"+49.30.12345678", "[REDACTED_PHONE]"},
	{"+86 138 0013 8000", "[REDACTED_PHONE]"},
	{"0049 30 12345678", "[REDACTED_PHONE]"},
	{"1-800-555-0199", "[REDACTED_PHONE]"},
	{"call 15551234567", "call [REDACTED_PHONE]"},
	{"1-555-123-4567", "[REDACTED_PHONE]"},
	{"1 (555) 123-4567", "[REDACTED_PHONE]"},
	{"1.555.123.4567", "[REDACTED_PHONE]"},
	// Adjacent identifiers: no rule may stop inside the next one.
	{"555-123-4567 555-987-6543", "[REDACTED_PHONE] [REDACTED_PHONE]"},
	{"555-123-4567-555-987-6543", "[REDACTED_PHONE]-[REDACTED_PHONE]"},
	{"+1 555 123 4567 +1 555 987 6543", "[REDACTED_PHONE] [REDACTED_PHONE]"},
	{"+44 20 7946 0958 123-45-6789", "[REDACTED_PHONE]"},
	{"+44 20 7946 0958 555-123-4567", "[REDACTED_PHONE] [REDACTED_PHONE]"},
	{"+44 20 7946\n555\n123\n4567", "[REDACTED_PHONE]\n[REDACTED_PHONE]"},
	{"4111 1111 1111 1111 555-123-4567", "[REDACTED_CARD] [REDACTED_PHONE]"},
	// Accepted over-redaction (docs/CONFIGURATION.md): 0 plus 8 or more digits.
	{"invoice 0012345678", "invoice [REDACTED_PHONE]"},
	// Accepted, as in 0.4: 10-13 digits read as a phone with a bare country code.
	{"call 442079460958", "call [REDACTED_PHONE]"},
	{"uuid 123e4567-e89b-12d3-a456-426614174000", "uuid 123e4567-e89b-12d3-a456-[REDACTED_PHONE]"},
	{"030 12345678", "[REDACTED_PHONE]"},
	{"(030) 1234567", "[REDACTED_PHONE]"},
	{"030/12345678", "[REDACTED_PHONE]"},
	{"020 7946 0958", "[REDACTED_PHONE]"},
	{"01 23 45 67 89", "[REDACTED_PHONE]"},
	{"Tel. 0171-1234567.", "Tel. [REDACTED_PHONE]."},
	// ssn
	{"SSN: 123-45-6789", "SSN: [REDACTED_SSN]"},
	{"123 45 6789", "[REDACTED_SSN]"},
	{"SSN 123456789", "[REDACTED_SSN]"},
	{"Social Security No. 123456789", "[REDACTED_SSN]"},
	// iban
	{"IBAN GB29NWBK60161331926819", "IBAN [REDACTED_IBAN]"},
	{"de89370400440532013000", "[REDACTED_IBAN]"},
	{"DE89 3704 0044 0532 0130 00 is mine", "[REDACTED_IBAN] is mine"},
	{"BE68 5390 0754 7034 from Bob", "[REDACTED_IBAN] from Bob"},
	{"NL91 ABNA 0417 1643 00", "[REDACTED_IBAN]"},
	{"BR15 0000 0000 0000 1093 2840 814P 2", "[REDACTED_IBAN]"},
	{"NO93 8601 1117 947", "[REDACTED_IBAN]"},
	{"FR14 2004 1010 0505 0001 3M02 606", "[REDACTED_IBAN]"},
	// card
	{"card 4111 1111 1111 1111", "card [REDACTED_CARD]"},
	{"4111-1111-1111-1111", "[REDACTED_CARD]"},

	// Negatives: none of these is personal data.
	{"due 2026-10-14", "due 2026-10-14"},
	{"on 14.10.2026 at 10:30", "on 14.10.2026 at 10:30"},
	{"am 01.02.2026 12:30 Uhr", "am 01.02.2026 12:30 Uhr"},
	{"on 01/02/2026 12:30", "on 01/02/2026 12:30"},
	{"on 01-02-2026 12:30", "on 01-02-2026 12:30"},
	{"2026-10-14T10:30:00+02:00", "2026-10-14T10:30:00+02:00"},
	{"10:30 - 11:45", "10:30 - 11:45"},
	{"total $1,299.99 or €0.99", "total $1,299.99 or €0.99"},
	{"order #48213 shipped", "order #48213 shipped"},
	{"order 123456789 shipped", "order 123456789 shipped"},
	{"ZIP 12345-6789, PLZ 80331", "ZIP 12345-6789, PLZ 80331"},
	{"server 192.168.1.1 and 10.0.0.254", "server 192.168.1.1 and 10.0.0.254"},
	{"version 1.2.3, build 10.0.19045", "version 1.2.3, build 10.0.19045"},
	{"room 0123, ext. 4567", "room 0123, ext. 4567"},
	{"up +12% to 1,234", "up +12% to 1,234"},
	{"install react@18.2.0", "install react@18.2.0"},
	{"Flight LH2024 seat 12A", "Flight LH2024 seat 12A"},
	{"meet in Q3 2026", "meet in Q3 2026"},
	{"RT66 road trip with mom", "RT66 road trip with mom"},
	{"C++ 2026 and 1+1=2", "C++ 2026 and 1+1=2"},
	{"Ticket #0001234", "Ticket #0001234"},
}

func TestDefaultPatterns(t *testing.T) {
	engine, err := New(config.Default().Redaction.Patterns)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, tc := range defaultCases {
		if got := engine.Apply(tc.in); got != tc.want {
			t.Errorf("Apply(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The 0.3-0.4 default list, which installs upgrading to the new defaults lose.
var oldDefaults = []config.Pattern{
	{Name: "email", Regex: `(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`, Replacement: "[REDACTED_EMAIL]"},
	{Name: "phone", Regex: `\b(?:\+?\d{1,3}[\s.-]?)?(?:\(\d{3}\)|\d{3})[\s.-]?\d{3}[\s.-]?\d{4}\b`, Replacement: "[REDACTED_PHONE]"},
	{Name: "ssn", Regex: `\b\d{3}-\d{2}-\d{4}\b`, Replacement: "[REDACTED_SSN]"},
	{Name: "iban", Regex: `\b[A-Z]{2}\d{2}[A-Z0-9]{10,30}\b`, Replacement: "[REDACTED_IBAN]"},
	{Name: "card", Regex: `\b(?:\d[ -]*?){13,19}\b`, Replacement: "[REDACTED_CARD]"},
}

// The new defaults must not leave behind any digit run or address the old
// defaults removed: an upgrade may not un-redact anything.
func TestDefaultsRedactWhatOldDefaultsDid(t *testing.T) {
	oldEngine, err := New(oldDefaults)
	if err != nil {
		t.Fatal(err)
	}
	newEngine, err := New(config.Default().Redaction.Patterns)
	if err != nil {
		t.Fatal(err)
	}
	digits := regexp.MustCompile(`\d+`)
	check := func(in string) {
		old, got := oldEngine.Apply(in), newEngine.Apply(in)
		if old == in {
			return
		}
		kept := map[string]int{}
		for _, run := range digits.FindAllString(old, -1) {
			kept[run]++
		}
		for _, run := range digits.FindAllString(got, -1) {
			if kept[run] == 0 {
				t.Errorf("%q: old defaults gave %q, new defaults leave %q in %q", in, old, run, got)
			}
			kept[run]--
		}
		if strings.Count(got, "@") > strings.Count(old, "@") {
			t.Errorf("%q: old defaults gave %q, new defaults leave an address in %q", in, old, got)
		}
	}
	// Every row, then every pair of identifier rows written back to back, so
	// no rule can stop inside the identifier that follows it. (Next to a date
	// or a price, 0.4's card rule also ate the date; keeping it is intended.)
	for _, a := range defaultCases {
		check(a.in)
		for _, b := range defaultCases {
			if a.in == a.want || b.in == b.want {
				continue
			}
			for _, sep := range []string{" ", "\n", ", "} {
				check(a.in + sep + b.in)
			}
		}
	}
}

// A saved config.yaml list replaces the defaults whole; it is not merged.
func TestConfiguredPatternsReplaceDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "redaction:\n  patterns:\n    - name: only\n      regex: secret\n      replacement: \"[X]\"\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	engine, err := New(cfg.Redaction.Patterns)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const in = "secret jürgen@münchen.de"
	if got, want := engine.Apply(in), "[X] jürgen@münchen.de"; got != want {
		t.Fatalf("Apply = %q, want %q", got, want)
	}
}
