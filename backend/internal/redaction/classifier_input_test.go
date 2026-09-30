package redaction

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
)

func TestClassifierInputRedactsBeforeRuneLimits(t *testing.T) {
	e, err := New([]config.Pattern{{Regex: "SECRET", Replacement: "X"}})
	if err != nil {
		t.Fatal(err)
	}
	input := func(n int) string { return "  " + strings.Repeat("界", n-2) + "SECRETafter  " }
	sender, subject, body := e.ClassifierInput(input(256), input(512), input(2000))
	for _, tc := range []struct {
		s     string
		limit int
	}{{sender, 256}, {subject, 512}, {body, 2000}} {
		if !utf8.ValidString(tc.s) || utf8.RuneCountInString(tc.s) != tc.limit || !strings.HasSuffix(tc.s, "Xa") {
			t.Fatalf("redaction/truncation changed: limit=%d tail=%q", tc.limit, tc.s[len(tc.s)-5:])
		}
	}
}
