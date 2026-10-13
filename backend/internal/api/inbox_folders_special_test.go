package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
)

// The handler finds roles by type assertion, so a renamed method would drop
// them silently; fail the build instead.
var _ specialUseLister = (*imapadapter.APIClient)(nil)

// specialUseClient is a mail client that publishes RFC 6154 roles.
type specialUseClient struct {
	fakeMailClient
	uses map[string]string
	err  error
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
