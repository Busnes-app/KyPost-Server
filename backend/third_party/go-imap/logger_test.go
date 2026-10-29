package imap

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestLogEscapesProtocolLineBreaks(t *testing.T) {
	var out bytes.Buffer
	SetSlogLogger(slog.New(slog.NewTextHandler(&out, nil)))
	defer SetLogger(nil)
	warnLog(1, "IN\nBOX", "server response", "response", "a OK\r\nforged=1", "uid", 7)
	got := out.String()
	if strings.Count(got, "\n") != 1 || !strings.Contains(got, `a OK\\r\\nforged=1`) || !strings.Contains(got, `mailbox=IN\nBOX`) || !strings.Contains(got, "uid=7") {
		t.Fatal(got)
	}
}

func TestTraceCommandHidesCredentials(t *testing.T) {
	for _, command := range []string{`LOGIN "user" "pa\"ss"`, "AUTHENTICATE XOAUTH2 dG9rZW4="} {
		if got := traceCommand("T1", command); strings.Contains(got, "pa") || strings.Contains(got, "dG9r") || !strings.HasSuffix(got, " ****") {
			t.Fatalf("traceCommand(%q) = %q", command, got)
		}
	}
	if got := traceCommand("T1", `SELECT "INBOX"`); got != `T1 SELECT "INBOX"` {
		t.Fatal(got)
	}
}
