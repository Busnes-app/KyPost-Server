package imap

import (
	"bytes"
	"crypto/tls"
	"log/slog"
	"net"
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

func TestVerboseAuthNeverLogsCredentials(t *testing.T) {
	var out bytes.Buffer
	SetSlogLogger(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer SetLogger(nil)
	Verbose = true
	defer func() { Verbose = false }()
	client, server := net.Pipe()
	_ = server.Close()
	d := &Dialer{conn: tls.Client(client, &tls.Config{InsecureSkipVerify: true})}
	_ = d.Login("user", `pa"ss-secret`)
	_ = d.Authenticate("user", "token-secret")
	if got := out.String(); strings.Contains(got, "secret") || !strings.Contains(got, "LOGIN ****") || !strings.Contains(got, "AUTHENTICATE XOAUTH2 ****") {
		t.Fatal(got)
	}
}
