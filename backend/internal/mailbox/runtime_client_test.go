package mailbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
)

func runtimeTestClient(t *testing.T, admit func(context.Context) error) *Client {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "mailbox#runtime?%")
	s := openTest(t, dir, testOwner, testLimits)
	c, err := NewClient(s, "alice@example.test")
	must(t, err)
	source := c.MailSourceIdentity()
	must(t, s.Close())
	c, err = OpenClient(dir, testOwner, testLimits, "alice@example.test", source, admit)
	must(t, err)
	return c
}

func TestRuntimeClientAdmissionCoversEveryOperation(t *testing.T) {
	ctx := context.Background()
	denied := errors.New("access revoked")
	c := runtimeTestClient(t, func(context.Context) error { return denied })
	calls := map[string]func() error{
		"overviews":          func() error { _, err := c.ListOverviews(ctx, "INBOX", 10); return err },
		"raw":                func() error { _, err := c.FetchRawMessage(ctx, "INBOX", 1); return err },
		"bodies":             func() error { _, err := c.GetMessageBodies(ctx, "INBOX", []int{1}); return err },
		"messages":           func() error { _, err := c.ListUnreadMessages(ctx, "INBOX", 10); return err },
		"poll":               func() error { _, _, err := c.ListUnreadInbox(ctx, ""); return err },
		"attachments":        func() error { _, err := c.ListAttachments(ctx, "INBOX", 1); return err },
		"attachment":         func() error { _, _, err := c.GetAttachment(ctx, "INBOX", 1, 0); return err },
		"headers":            func() error { _, err := c.FetchHeaderFields(ctx, "INBOX", []int{1}, "From"); return err },
		"search":             func() error { _, err := c.SearchMessages(ctx, "INBOX", "body", "test", 10); return err },
		"draft":              func() error { return c.SaveDraft(ctx, imapadapter.DraftMessage{}) },
		"sent":               func() error { return c.SaveSent(ctx, imapadapter.DraftMessage{}) },
		"ensure-label":       func() error { return c.EnsureLabel(ctx, "Primary") },
		"labels":             func() error { _, err := c.ListLabels(ctx); return err },
		"apply-label":        func() error { return c.ApplyLabel(ctx, "1", "Primary") },
		"remove-label":       func() error { return c.RemoveLabel(ctx, "1", "Primary") },
		"action":             func() error { return c.ApplyInboxAction(ctx, "1", "read", "INBOX", "") },
		"folders":            func() error { _, err := c.ListSubfolders(ctx, "INBOX"); return err },
		"create-folder":      func() error { _, err := c.CreateFolder(ctx, "INBOX", "test"); return err },
		"rename-folder":      func() error { _, err := c.RenameFolder(ctx, "INBOX/test", "new"); return err },
		"delete-folder":      func() error { return c.DeleteFolder(ctx, "INBOX/test") },
		"prepare-encryption": func() error { _, err := c.PrepareIncoming(ctx, 1, false); return err },
		"replace-encryption": func() error { _, err := c.ReplaceIncoming(ctx, imapadapter.IncomingSource{}, "", nil); return err },
		"encryption-action":  func() error { return c.ApplyIncomingAction(ctx, imapadapter.IncomingSource{}, 1, "", nil, "read", "") },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, denied) {
				t.Fatalf("operation bypassed admission: %v", err)
			}
		})
	}
}

func TestRuntimeClientSurvivesGCWhileOperationActive(t *testing.T) {
	entered, resume := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	func() {
		c := runtimeTestClient(t, func(context.Context) error { close(entered); <-resume; return nil })
		go func(client *Client) { _, err := client.ListOverviews(context.Background(), "INBOX", 10); done <- err }(c)
	}() // only the in-flight operation retains the client
	<-entered
	for range 3 {
		runtime.GC()
	}
	close(resume)
	if err := <-done; err != nil {
		t.Fatalf("cleanup closed an active operation: %v", err)
	}
}

func TestExistingMailboxNeverCreatesOrInitializes(t *testing.T) {
	source := "native:" + strings.Repeat("a", 64)
	for _, kind := range []string{"missing", "empty", "foreign-source"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "mailbox#old?%")
			if kind != "missing" {
				must(t, os.Mkdir(dir, 0700))
			}
			if kind == "empty" {
				must(t, os.WriteFile(filepath.Join(dir, "mailbox.db"), nil, 0600))
			}
			if kind == "foreign-source" {
				s := openTest(t, dir, testOwner, testLimits)
				must(t, s.Close())
			}
			if s, err := OpenExisting(dir, testOwner, testLimits, source); err == nil {
				s.Close()
				t.Fatal("unacknowledged storage opened")
			}
			if kind == "missing" {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatalf("missing directory recreated: %v", err)
				}
			}
			if kind == "empty" {
				info, err := os.Stat(filepath.Join(dir, "mailbox.db"))
				if err != nil || info.Size() != 0 {
					t.Fatalf("unbound database initialized: %v", err)
				}
			}
		})
	}
}
