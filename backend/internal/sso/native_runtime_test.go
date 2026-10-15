//go:build linux

package sso

import (
	"context"
	"errors"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestAdmitUserMailbox(t *testing.T) {
	ctx := context.Background()
	f := newAddressFixture(t, "one")
	native := f.ids["one"]
	extra, err := f.life.CreateNativeMailbox(ctx, f.root, native, "sales@example.test")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := f.accounts.Create(ctx, "plain", "plain-password-123456", users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	settings := NewStore(f.config)
	for _, tc := range []struct {
		name          string
		mode, sso     bool
		user, mailbox string
		wantNative    bool
		wantErr       error
		wantMailbox   string
	}{
		{"unknown user", true, true, "no-such-user", "", false, users.ErrNotFound, ""},
		{"imap primary", true, true, plain.ID, "", false, nil, ""},
		{"imap extra", true, true, plain.ID, extra.ID, false, ErrNativeMailboxUnknown, ""},
		{"imap primary, mode off", false, false, plain.ID, "", false, nil, ""},
		{"native primary", true, true, native, "", true, nil, native},
		{"native primary by id", true, true, native, native, true, nil, native},
		{"native extra", true, true, native, extra.ID, true, nil, extra.ID},
		{"native unknown extra", true, true, native, "mbx-unknown", true, ErrNativeMailboxUnknown, ""},
		{"native primary, mode off", false, true, native, "", true, ErrNativeMailDisabled, ""},
		{"native extra, mode off", false, true, native, extra.ID, true, ErrNativeMailDisabled, ""},
		{"native primary, sso off", true, false, native, "", true, ErrNativeProvisioning, ""},
		{"native extra, sso off", true, false, native, extra.ID, true, ErrNativeProvisioning, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := settings.Save(SSOSettings{Enabled: tc.sso, IssuerURL: nativeIssuer, ClientID: "kypost"}); err != nil {
				t.Fatal(err)
			}
			a, gotNative, err := AdmitUserMailbox(ctx, tc.mode, settings, f.life, f.accounts, f.root, tc.user, tc.mailbox)
			if gotNative != tc.wantNative || !errors.Is(err, tc.wantErr) || (tc.wantErr == nil) != (err == nil) || a.Owner.Mailbox != tc.wantMailbox {
				t.Fatalf("got (%q, %v, %v), want (%q, %v, %v)", a.Owner.Mailbox, gotNative, err, tc.wantMailbox, tc.wantNative, tc.wantErr)
			}
		})
	}
}
