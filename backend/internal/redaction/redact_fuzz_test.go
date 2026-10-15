package redaction

import (
	"regexp"
	"testing"
	"unicode/utf8"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
)

var marker = regexp.MustCompile(`\[REDACTED_[A-Z]+\]`)

// FuzzDefaultPatterns runs the default patterns over arbitrary mail text.
// Redaction only removes text: with the markers stripped, the output is never
// longer than the input, and valid UTF-8 stays valid.
func FuzzDefaultPatterns(f *testing.F) {
	engine, err := New(config.Default().Redaction.Patterns)
	if err != nil {
		f.Fatal(err)
	}
	for _, s := range []string{
		"contact me at yoshi@urlxl.com please",
		"call 555-123-4567 now",
		"SSN: 123-45-6789",
		"IBAN GB29NWBK60161331926819",
		"card 4111 1111 1111 1111",
		"already [REDACTED_EMAIL] here",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, in string) {
		out := engine.Apply(in)
		if rest := marker.ReplaceAllString(out, ""); len(rest) > len(in) {
			t.Fatalf("output grew beyond its markers: %q -> %q", in, out)
		}
		if utf8.ValidString(in) && !utf8.ValidString(out) {
			t.Fatalf("redaction broke UTF-8: %q -> %q", in, out)
		}
	})
}
