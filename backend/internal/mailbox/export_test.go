package mailbox

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// unmboxrd is an independent mboxrd reader: a line starting "From " opens a
// message, one '>' is removed from ^>+From  lines, and the blank line closing
// each message is dropped.
func unmboxrd(t *testing.T, data []byte) (separators []string, messages [][]byte) {
	t.Helper()
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			t.Fatalf("unterminated line %q", data)
		}
		line := data[:i+1]
		data = data[i+1:]
		switch {
		case bytes.HasPrefix(line, []byte("From ")):
			separators = append(separators, string(line))
			messages = append(messages, nil)
		case len(messages) == 0:
			t.Fatalf("content before the first separator: %q", line)
		default:
			if bytes.HasPrefix(bytes.TrimLeft(line, ">"), []byte("From ")) && line[0] == '>' {
				line = line[1:]
			}
			messages[len(messages)-1] = append(messages[len(messages)-1], line...)
		}
	}
	for i, m := range messages {
		if !bytes.HasSuffix(m, []byte("\n")) {
			t.Fatalf("message %d lost its closing blank line", i)
		}
		messages[i] = m[:len(m)-1]
	}
	return separators, messages
}

func TestMboxrdQuotingRoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	hostile := []byte("From: a@example.test\r\nSubject: quoting\r\n\r\nFrom x\r\n>From x\r\n>>From y\r\nFrom\r\n From z\r\nFromage\r\nbody\r\n")
	in := []ExportMessage{
		{ID: 1, Sender: "bounce@example.test", At: at, Raw: hostile},
		{ID: 2, At: at, Raw: testRaw},
		{ID: 3, Sender: "spaced sender@example.test", Raw: []byte("Subject: lf only\n\nFrom lf\n")},
	}
	var out bytes.Buffer
	for _, m := range in {
		if err := WriteMboxrd(&out, m); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"\r\n>From x\r\n>>From x\r\n>>>From y\r\nFrom\r\n From z\r\nFromage\r\n", "\n>From lf\n"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("quoting missing %q in %q", want, out.String())
		}
	}
	seps, got := unmboxrd(t, out.Bytes())
	wantSeps := []string{"From bounce@example.test Tue Sep  1 10:00:00 2026\n", "From MAILER-DAEMON Tue Sep  1 10:00:00 2026\n", "From MAILER-DAEMON Thu Jan  1 00:00:00 1970\n"}
	if fmt.Sprint(seps) != fmt.Sprint(wantSeps) || len(got) != len(in) {
		t.Fatalf("separators %q", seps)
	}
	for i := range in {
		if !bytes.Equal(got[i], in[i].Raw) {
			t.Fatalf("message %d changed:\n%q\n%q", i, got[i], in[i].Raw)
		}
	}
}

func TestZipFolderSanitizes(t *testing.T) {
	long := strings.Repeat("é", 100)
	for in, want := range map[string]string{
		"INBOX":            "INBOX",
		"INBOX/Receipts":   "INBOX/Receipts",
		"../../etc/passwd": "_/_/etc/passwd",
		"/abs/path":        "_/abs/path",
		"C:\\Windows":      "C__Windows",
		"a\x00b":           "a_b",
		"..\\..\\x":        "_.._x",
		" . ":              "_",
		"Привет/日本":        "Привет/日本",
		"rtl\u202egnp.exe": "rtl_gnp.exe",
		long:               strings.Repeat("é", 32),
	} {
		got := ZipFolder(in)
		if got != want {
			t.Errorf("ZipFolder(%q) = %q, want %q", in, got, want)
		}
		if strings.HasPrefix(got, "/") || strings.ContainsRune(got, 0) || strings.ContainsRune(got, '\\') {
			t.Errorf("ZipFolder(%q) = %q escapes", in, got)
		}
		for _, part := range strings.Split(got, "/") {
			if part == "" || part == "." || part == ".." || len(part) > 64 {
				t.Errorf("ZipFolder(%q) has segment %q", in, part)
			}
		}
	}
}

func TestNativeExportPagesExactBytesToZip(t *testing.T) {
	ctx := context.Background()
	s, c := newTestClient(t)
	must(t, s.CreateFolder(ctx, "Work"))
	var want [][]byte
	// More than two pages, so paging must continue past exportPage.
	for i := range 2*exportPage + 7 {
		raw := []byte(fmt.Sprintf("From: s@example.test\r\nSubject: %d\r\n\r\nFrom line %d\r\n\x00binary\r\n", i, i))
		importClient(t, s, fmt.Sprint(i), raw)
		want = append(want, raw)
	}
	work, err := s.Append(ctx, "Work", bytes.NewReader(testRaw), false)
	must(t, err)

	if _, err = c.ExportFolders(ctx, "Missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing folder exported", err)
	}
	if f, err := c.ExportFolders(ctx, "inbox"); err != nil || fmt.Sprint(f) != "[INBOX]" {
		t.Fatal("folder not canonical", f, err)
	}
	folders, err := c.ExportFolders(ctx, "")
	must(t, err)
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	calls := 0
	must(t, c.Export(ctx, folders, func(m ExportMessage) error {
		calls++
		return WriteEML(z, m)
	}))
	must(t, z.Close())
	if calls != len(want)+1 {
		t.Fatal("messages exported", calls)
	}
	r, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	must(t, err)
	for i, f := range r.File {
		rc, err := f.Open()
		must(t, err)
		got, err := io.ReadAll(rc)
		must(t, err)
		expect, name := testRaw, fmt.Sprintf("Work/%010d.eml", work)
		if i < len(want) {
			expect, name = want[i], fmt.Sprintf("INBOX/%010d.eml", i+1)
		}
		if f.Name != name || !bytes.Equal(got, expect) {
			t.Fatalf("entry %d %s: %q", i, f.Name, got)
		}
	}

	// A consumer error stops the export.
	stop := errors.New("stop")
	if err = c.Export(ctx, folders, func(ExportMessage) error { return stop }); !errors.Is(err, stop) {
		t.Fatal("consumer error lost", err)
	}
}
