package sso

import (
	"encoding/json"
	"testing"
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
