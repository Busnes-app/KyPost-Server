package sso

import (
	"encoding/json"
	"errors"
	"github.com/Busnes-app/ky-primitives/syncauth"
	"os"
	"testing"
	"time"
)

func TestHasAdminRole(t *testing.T) {
	cases := map[string]bool{
		`["kypost.admin"]`:                                true,
		`[{"value":"kypost.admin","primary":true}]`:       true,
		`["notes.admin",{"value":"kypost.user"},"admin"]`: false,
		`["KYPOST.ADMIN"]`:                                false,
		`[]`:                                              false,
		`null`:                                            false,
		`"kypost.admin"`:                                  false,
		`[42,{"name":"kypost.admin"}]`:                    false,
	}
	for raw, want := range cases {
		if got := HasAdminRole(json.RawMessage(raw)); got != want {
			t.Errorf("HasAdminRole(%s) = %v, want %v", raw, got, want)
		}
	}
}

func TestDirectoryUserRevision(t *testing.T) {
	valid := func() DirectoryUser {
		active := true
		u := DirectoryUser{Schemas: []string{scimUserSchema}, ID: "sub", ExternalID: "sub", UserName: "sub", Active: &active}
		u.Meta.Version = `W/"7"`
		return u
	}
	if n, err := valid().Revision("user.updated"); err != nil || n != 7 {
		t.Fatalf("valid: %d %v", n, err)
	}
	inactive := valid()
	*inactive.Active = false
	inactive.UserName = ""
	if _, err := inactive.Revision("user.deleted"); err != nil {
		t.Errorf("an inactive deletion without a userName is valid: %v", err)
	}

	bad := map[string]func(u *DirectoryUser){
		"strong etag":   func(u *DirectoryUser) { u.Meta.Version = `"7"` },
		"zero":          func(u *DirectoryUser) { u.Meta.Version = `W/"0"` },
		"padded":        func(u *DirectoryUser) { u.Meta.Version = `W/"07"` },
		"no schema":     func(u *DirectoryUser) { u.Schemas = nil },
		"control in id": func(u *DirectoryUser) { u.ID, u.ExternalID = "a\x00b", "a\x00b" },
		"externalId":    func(u *DirectoryUser) { u.ExternalID = "other" },
		"no active":     func(u *DirectoryUser) { u.Active = nil },
		"padded name":   func(u *DirectoryUser) { u.UserName = " x" },
	}
	for name, mutate := range bad {
		u := valid()
		mutate(&u)
		if _, err := u.Revision("user.updated"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := valid().Revision("user.deleted"); err == nil {
		t.Error("an active deletion was accepted")
	}
	if _, err := valid().Revision("group.created"); err == nil {
		t.Error("an unsupported event type was accepted")
	}
}

func TestDirectoryDesiredResourceSurvivesRetryAndFailure(t *testing.T) {
	dir := t.TempDir()
	store := NewLifecycleStore(dir)
	active := true
	resource := DirectoryUser{Schemas: []string{scimUserSchema}, ID: "subject", ExternalID: "subject", UserName: "name", Active: &active}
	resource.Meta.Version = `W/"1"`
	resource.Emails = append(resource.Emails, struct {
		Value   string `json:"value"`
		Primary bool   `json:"primary"`
	}{"primary@example.test", true})
	ev := syncauth.Event{ID: "one", Type: "user.created", At: time.Now()}
	applied := 0
	apply := func() (bool, error) { applied++; return false, nil }
	if _, err := store.ApplyDirectoryUser("https://idp.example", ev, resource, "digest-one", apply); err != nil {
		t.Fatal(err)
	}
	store = NewLifecycleStore(dir)
	verify := func(revision int64, address string) {
		t.Helper()
		st, ok, err := store.Directory("https://idp.example", resource.ID)
		if err != nil || !ok || st.Revision != revision || st.Resource == nil || st.Resource.Email() != address {
			t.Fatalf("desired resource: %+v %v", st, err)
		}
	}
	verify(1, "primary@example.test")
	if _, err := store.ApplyDirectoryUser("https://idp.example", ev, resource, "digest-one", apply); err != nil || applied != 1 {
		t.Fatal("retry reapplied resource", err)
	}
	resource.Meta.Version = `W/"2"`
	resource.Emails[0].Value = "new@example.test"
	ev.ID = "two"
	ev.Type = "user.updated"
	if _, err := store.ApplyDirectoryUser("https://idp.example", ev, resource, "digest-two", func() (bool, error) { return false, errors.New("failed account change") }); err == nil {
		t.Fatal("failed callback acknowledged")
	}
	verify(1, "primary@example.test")
	backup := store.path + ".saved"
	if _, err := store.ApplyDirectoryUser("https://idp.example", ev, resource, "digest-two", func() (bool, error) {
		if err := os.Rename(store.path, backup); err != nil {
			return false, err
		}
		return false, os.Mkdir(store.path, 0700) // JSON publication now cannot rename over a directory.
	}); err == nil {
		t.Fatal("failed persistence acknowledged")
	}
	if err := os.Remove(store.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, store.path); err != nil {
		t.Fatal(err)
	}
	verify(1, "primary@example.test")
	if _, err := store.ApplyDirectoryUser("https://idp.example", ev, resource, "digest-two", apply); err != nil {
		t.Fatal(err)
	}
	verify(2, "new@example.test")
	ev.ID = "stale"
	resource.Meta.Version = `W/"1"`
	if _, err := store.ApplyDirectoryUser("https://idp.example", ev, resource, "digest-one", apply); !errors.Is(err, ErrDirectoryConflict) {
		t.Fatal("stale resource admitted", err)
	}
	verify(2, "new@example.test")
	ev.ID = "three"
	if _, err := store.ApplyDirectory("https://idp.example", ev, resource.ID, 3, "digest-three", false, apply); err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Directory("https://idp.example", resource.ID)
	if err != nil || st.Resource != nil || st.Active {
		t.Fatal("compatibility event retained stale resource", err)
	}
}
