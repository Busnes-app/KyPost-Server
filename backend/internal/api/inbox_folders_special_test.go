package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
)

// The handler finds roles by type assertion, so a renamed method would drop
// them silently; fail the build instead.
var _ specialUseLister = (*imapadapter.APIClient)(nil)

// specialUseClient is a mail client that publishes RFC 6154 roles.
type specialUseClient struct {
	fakeMailClient
	uses    map[string]string
	err     error
	changed []string
}

func (c *specialUseClient) RenameFolder(_ context.Context, folder, _ string) (string, error) {
	c.changed = append(c.changed, folder)
	return folder, nil
}

func (c *specialUseClient) DeleteFolder(_ context.Context, folder string) error {
	c.changed = append(c.changed, folder)
	return nil
}

func (c *specialUseClient) SpecialUseFolders(context.Context) (map[string]string, error) {
	return c.uses, c.err
}

func folderJSON(t *testing.T, folders []inboxFolder) string {
	t.Helper()
	b, err := json.Marshal(folders)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInboxFoldersReportSpecialUseByExactPath(t *testing.T) {
	client := &specialUseClient{uses: map[string]string{
		"INBOX.Elementos enviados": "sent",
		"Entwürfe":                 "drafts",
	}}
	paths := []string{"INBOX.Elementos enviados", "Entwürfe", "INBOX.Sent", "Work"}

	got := map[string]string{}
	for _, f := range toInboxFolders(paths, specialUseOf(context.Background(), client)) {
		got[f.Path] = f.Special
	}
	want := map[string]string{"INBOX.Elementos enviados": "sent", "Entwürfe": "drafts", "INBOX.Sent": "", "Work": ""}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("special = %v, want %v", got, want)
	}
}

func TestInboxFoldersOmitSpecialWithoutTheCapability(t *testing.T) {
	for name, client := range map[string]imapadapter.Client{
		"no capability": &fakeMailClient{},
		"lookup fails":  &specialUseClient{uses: map[string]string{"Sent": "sent"}, err: errors.New("imap down")},
	} {
		t.Run(name, func(t *testing.T) {
			got := folderJSON(t, toInboxFolders([]string{"Sent"}, specialUseOf(context.Background(), client)))
			if want := `[{"path":"Sent","deletable":false}]`; got != want {
				t.Fatalf("folders = %s, want %s", got, want)
			}
		})
	}
}

// A localized Sent folder is protected by its role, not its name.
func TestInboxFoldersProtectSpecialUseFolders(t *testing.T) {
	uses := map[string]string{"INBOX.Elementos enviados": "sent"}
	listed := toInboxFolders([]string{"INBOX.Elementos enviados", "INBOX.Work"}, uses)
	if listed[0].Deletable || !listed[1].Deletable {
		t.Fatalf("deletable = %v, %v; want false, true", listed[0].Deletable, listed[1].Deletable)
	}

	requests := map[string]func(folder string) *http.Request{
		"rename": func(folder string) *http.Request {
			return httptest.NewRequest(http.MethodPut, "/api/inbox/folders",
				strings.NewReader(`{"folder":"`+folder+`","name":"Renamed"}`))
		},
		"delete": func(folder string) *http.Request {
			return httptest.NewRequest(http.MethodDelete, "/api/inbox/folders?folder="+strings.ReplaceAll(folder, " ", "%20"), nil)
		},
	}
	for name, request := range requests {
		t.Run(name, func(t *testing.T) {
			client := &specialUseClient{uses: uses}
			rec := httptest.NewRecorder()
			serveInboxFolders(rec, request("INBOX.Elementos enviados"), client)
			if rec.Code != http.StatusBadRequest || len(client.changed) != 0 {
				t.Fatalf("special-use folder: status %d, changed %v; want 400, none", rec.Code, client.changed)
			}

			// A failed role lookup must not block an ordinary folder.
			client = &specialUseClient{uses: uses, err: errors.New("imap down")}
			rec = httptest.NewRecorder()
			serveInboxFolders(rec, request("INBOX.Work"), client)
			if rec.Code != http.StatusOK || len(client.changed) != 1 {
				t.Fatalf("ordinary folder: status %d, changed %v; want 200, one; body=%s", rec.Code, client.changed, rec.Body.String())
			}
		})
	}
}
