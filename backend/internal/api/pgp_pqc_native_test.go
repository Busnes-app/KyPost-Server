//go:build linux

package api

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func equipPQKey(t *testing.T, s *Server, u users.User, addr, pq string) users.User {
	t.Helper()
	v4 := must1(pgpmailtest.GenerateIdentity(u.Username, addr))
	old := must1(pgpmail.InspectPublicKey(v4.ArmoredPublicKey))
	u = must1(s.users.SetPGPIdentityClientProtected(u.ID, old.Fingerprint, old.KeyID, old.ArmoredPublicKey, `{"v":2}`, "generated", "now", nil))
	u = must1(s.users.SetDerivedAuth(context.Background(), u.ID, strings.Repeat("a", 64), base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")), 600000, false))
	info := must1(pgpmail.InspectPQCKey(pq))
	return must1(s.users.CommitPGPKeyring(context.Background(), u.ID, users.PGPKeyringUpdate{
		ExpectedRevision:    u.PGPRevision,
		MaterialGeneration:  1,
		PrimaryFingerprints: []string{u.PGPFingerprint, info.Fingerprint},
		KeyFingerprints:     append(old.KeyFingerprints, info.KeyFingerprints...),
		PublicKey:           u.PGPPublicKey,
		PublicKeyV6:         pq,
		PasswordEnvelope:    `{"v":2}`,
		RecoveryEnvelope:    `{"v":2}`,
		Source:              "generated",
	}))
}

// Native accounts own their primary address, administrator-assigned active
// aliases, and active prepared extra mailboxes. Released aliases and disabled
// extra mailboxes must not resolve for internal PQ discovery.
func TestInternalPQDiscoveryIncludesNativeAliasesAndExtraMailboxes(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SECRET_DIR", t.TempDir())
	srv := newNativeRuntimeServer(t)

	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "pq-native-one", 1, runtimeDirectoryUser(true)))
	two := scimUser("native-runtime-two", "runtime-two", true)
	two["emails"] = []map[string]any{{"value": "two@example.test", "primary": true}}
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "pq-native-two", 1, two))

	sender, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-two")
	if err != nil {
		t.Fatal(err)
	}

	senderV6 := testV6Key(t, "one@example.test", true)
	recipientV6 := testV6Key(t, "two@example.test", true)
	sender = equipPQKey(t, srv, sender, "one@example.test", senderV6)
	recipient = equipPQKey(t, srv, recipient, "two@example.test", recipientV6)

	const aliasAddr = "sales@example.test"
	if _, err := srv.ssoLifecycle.AddNativeAlias(ctx, srv.stateDir, recipient.ID, aliasAddr); err != nil {
		t.Fatalf("AddNativeAlias: %v", err)
	}
	const extraAddr = "billing@example.test"
	extraBox, err := srv.ssoLifecycle.CreateNativeMailbox(ctx, srv.stateDir, recipient.ID, extraAddr)
	if err != nil {
		t.Fatalf("CreateNativeMailbox: %v", err)
	}
	const extraAliasAddr = "invoices@example.test"
	if _, err := srv.ssoLifecycle.AddNativeAlias(ctx, srv.stateDir, extraBox.ID, extraAliasAddr); err != nil {
		t.Fatalf("AddNativeAlias (extra): %v", err)
	}

	for _, addr := range []string{"two@example.test", aliasAddr, extraAddr, extraAliasAddr} {
		chk := askRecipient(t, srv, sender.ID, "check", addr, false)
		if !chk.HasKey || chk.Capability != "pq" {
			t.Fatalf("native address %s not checked as PQ: %+v", addr, chk)
		}
		got := askRecipient(t, srv, sender.ID, "resolve", addr, false)
		if !got.Usable || got.SelectedCapability != "pq" || got.PublicKey != recipientV6 {
			t.Fatalf("native address %s not resolved as PQ: %+v", addr, got)
		}
	}

	// Released alias must no longer resolve to the account.
	if _, err := srv.ssoLifecycle.ReleaseNativeAlias(ctx, srv.stateDir, aliasAddr); err != nil {
		t.Fatalf("ReleaseNativeAlias: %v", err)
	}
	if got := askRecipient(t, srv, sender.ID, "check", aliasAddr, false); got.HasKey || got.Capability == "pq" {
		t.Fatalf("released native alias still discovered: %+v", got)
	}

	// Disabled extra mailbox addresses must no longer resolve.
	if _, err := srv.ssoLifecycle.SetNativeMailboxState(ctx, srv.stateDir, extraBox.ID, false); err != nil {
		t.Fatalf("SetNativeMailboxState(false): %v", err)
	}
	for _, addr := range []string{extraAddr, extraAliasAddr} {
		if got := askRecipient(t, srv, sender.ID, "check", addr, false); got.HasKey || got.Capability == "pq" {
			t.Fatalf("disabled extra mailbox address %s still discovered: %+v", addr, got)
		}
	}
}
