//go:build linux

package sso

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// nativeRole applies a directory revision carrying or lacking the
// administrator role and mirrors the role onto the account, as the webhook does.
func nativeRole(t *testing.T, s *LifecycleStore, accounts *users.Store, sub, address string, revision int, active, admin bool) {
	t.Helper()
	roles := "[]"
	if admin {
		roles = fmt.Sprintf("[%q]", AdminAppRole)
	}
	raw := fmt.Sprintf(`{"schemas":[%q],"id":%q,"externalId":%q,"userName":%q,"active":%t,"roles":%s,"emails":[{"value":%q,"primary":true}],"meta":{"version":%q}}`, scimUserSchema, sub, sub, sub, active, roles, address, fmt.Sprintf(`W/"%d"`, revision))
	var u DirectoryUser
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: fmt.Sprintf("%s-%d", sub, revision), Type: "user.updated", At: time.Now()}
	if _, err := s.ApplyDirectoryUser(nativeIssuer, ev, u, EventDigest(ev.Type, []byte(raw)), func() (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	if accounts == nil {
		return
	}
	existing, err := accounts.GetBySSOSubIssuer(nativeIssuer, sub)
	if err != nil {
		t.Fatal(err)
	}
	role := users.RoleUser
	if active && admin {
		role = users.RoleAdmin
	}
	if _, err := accounts.SetRole(existing.ID, role); err != nil {
		t.Fatal(err)
	}
}

func legacyFlag(t *testing.T, s *LifecycleStore, sub string) bool {
	t.Helper()
	a, ok, err := s.NativeAssignment(nativeIssuer, sub)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	return a.LegacyMixedUse
}

func TestNativeAdministratorGetsNoMailbox(t *testing.T) {
	config, root := t.TempDir(), t.TempDir()
	life := NewLifecycleStore(config)
	domains := provenNativeDomain(t, config)
	accounts, err := users.LoadOrMigrate(context.Background(), config, filepath.Join(config, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	nativeRole(t, life, nil, "boss", "boss@example.test", 1, true, true)
	if _, err := life.AllocateNativeAccount(context.Background(), root, nativeIssuer, "boss", domains, accounts, nativeLimits); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("administrator allocated a mailbox", err)
	}
	if _, reserved, err := life.NativeAssignment(nativeIssuer, "boss"); err != nil || reserved {
		t.Fatal("administrator address reserved", reserved, err)
	}
	if _, err := accounts.GetBySSOSubIssuer(nativeIssuer, "boss"); !errors.Is(err, users.ErrNotFound) {
		t.Fatal("allocation published an account", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "users")); len(entries) != 0 {
		t.Fatal("administrator storage prepared", entries)
	}
}

func TestNativeAdmissionRefusesPromotedSubject(t *testing.T) {
	ctx := context.Background()
	config, root := t.TempDir(), t.TempDir()
	life := NewLifecycleStore(config)
	domains := provenNativeDomain(t, config)
	accounts, err := users.LoadOrMigrate(ctx, config, filepath.Join(config, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	nativeDesired(t, life, "one", "one@example.test", 1, true)
	u, err := life.AllocateNativeAccount(ctx, root, nativeIssuer, "one", domains, accounts, nativeLimits)
	if err != nil {
		t.Fatal(err)
	}
	a, err := life.AdmitNativeMail(ctx, root, nativeIssuer, u.ID, accounts)
	if err != nil {
		t.Fatal(err)
	}
	store, err := mailbox.Open(filepath.Join(root, "users", u.ID, "mailbox"), a.Owner, nativeLimits)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: test@example.test\r\nSubject: kept\r\n\r\nretained")
	id, err := store.Append(ctx, "INBOX", bytes.NewReader(raw), false)
	_ = store.Close()
	if err != nil {
		t.Fatal(err)
	}
	admit := func() error {
		_, err := life.AdmitNativeMail(ctx, root, nativeIssuer, u.ID, accounts)
		called := false
		access := life.WithNativeMailAccess(ctx, root, nativeIssuer, accounts, []string{u.ID}, func(map[string]NativeAssignment) error { called = true; return nil })
		if (err == nil) != (access == nil) || called != (access == nil) {
			t.Fatal("admission paths disagree", err, access, called)
		}
		return err
	}
	nativeRole(t, life, accounts, "one", "one@example.test", 2, true, true)
	if err := admit(); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("promoted subject admitted", err)
	}
	// A re-reconcile of the existing mailbox neither grants access nor sets the flag.
	if _, err := life.AllocateNativeAccount(ctx, root, nativeIssuer, "one", domains, accounts, nativeLimits); err != nil || legacyFlag(t, life, "one") {
		t.Fatal("promotion reconcile", err)
	}
	if err := admit(); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("promoted subject admitted after reconcile", err)
	}
	// Demotion: the account was everyday originally, so it is admitted again.
	nativeRole(t, life, accounts, "one", "one@example.test", 3, true, false)
	if err := admit(); err != nil {
		t.Fatal("demoted subject refused", err)
	}
	store, err = mailbox.Open(filepath.Join(root, "users", u.ID, "mailbox"), a.Owner, nativeLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if got, err := store.Raw(ctx, "INBOX", id); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("mail not retained across promotion", err)
	}
}

func TestNativeLegacyMixedUseClearedOnlyByDemotion(t *testing.T) {
	ctx := context.Background()
	config, keyPath, ids := v1Fixture(t)
	if _, err := MigrateNative(ctx, config, keyPath); err != nil {
		t.Fatal(err)
	}
	life := NewLifecycleStore(config)
	accounts, err := users.LoadOrMigrate(ctx, config, filepath.Join(config, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := life.NativeAssignment(nativeIssuer, "boss")
	if err != nil {
		t.Fatal(err)
	}
	root := a.StateRoot
	admit := func() error {
		_, err := life.AdmitNativeMail(ctx, root, nativeIssuer, ids["boss"], accounts)
		return err
	}
	if err := admit(); err != nil || !legacyFlag(t, life, "boss") {
		t.Fatal("legacy mixed-use administrator refused", err)
	}
	// Deactivation is not demotion, even when the inactive resource lists no
	// roles: the flag survives offboarding and return.
	nativeRole(t, life, nil, "boss", "boss@example.test", 3, false, false)
	if !legacyFlag(t, life, "boss") {
		t.Fatal("deactivation cleared legacyMixedUse")
	}
	nativeRole(t, life, nil, "boss", "boss@example.test", 4, true, true)
	if err := admit(); err != nil {
		t.Fatal("reactivated legacy administrator refused", err)
	}
	nativeRole(t, life, accounts, "boss", "boss@example.test", 5, true, false)
	if legacyFlag(t, life, "boss") {
		t.Fatal("demotion kept legacyMixedUse")
	}
	if err := admit(); err != nil {
		t.Fatal("demoted subject refused", err)
	}
	nativeRole(t, life, accounts, "boss", "boss@example.test", 6, true, true)
	if err := admit(); !errors.Is(err, ErrNativeProvisioning) || legacyFlag(t, life, "boss") {
		t.Fatal("re-promotion restored legacy access", err)
	}
}

// A flag left on a demoted subject (a skipped clear, or a crafted snapshot)
// fails snapshot validation, and reconcile clears it.
func TestNativeLegacyMixedUseReconcileAndSnapshot(t *testing.T) {
	ctx := context.Background()
	config, keyPath, _ := v1Fixture(t)
	if _, err := MigrateNative(ctx, config, keyPath); err != nil {
		t.Fatal(err)
	}
	life := NewLifecycleStore(config)
	accounts, err := users.LoadOrMigrate(ctx, config, filepath.Join(config, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := life.NativeAssignment(nativeIssuer, "boss")
	if err != nil {
		t.Fatal(err)
	}
	root := a.StateRoot
	list, err := accounts.List()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := life.ValidateNativeSnapshot(root, list); err != nil {
		t.Fatal("legacy administrator snapshot refused", err)
	}
	nativeRole(t, life, accounts, "boss", "boss@example.test", 3, true, false)
	f, err := life.loadNative()
	if err != nil {
		t.Fatal(err)
	}
	stale := f.Accounts[directoryKey(nativeIssuer, "boss")]
	stale.LegacyMixedUse = true
	f.Accounts[directoryKey(nativeIssuer, "boss")] = stale
	if err := life.saveNative(f); err != nil {
		t.Fatal(err)
	}
	if list, err = accounts.List(); err != nil {
		t.Fatal(err)
	}
	if _, err := life.ValidateNativeSnapshot(root, list); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("snapshot with a demoted legacy flag accepted", err)
	}
	if _, err := life.ReconcileNativeMailbox(root, nativeIssuer, "boss", stale.Owner.Mailbox, "example.test", stale.Limits); err != nil {
		t.Fatal(err)
	}
	if legacyFlag(t, life, "boss") {
		t.Fatal("reconcile kept a demoted legacy flag")
	}
	if _, err := life.ValidateNativeSnapshot(root, list); err != nil {
		t.Fatal(err)
	}
}

func TestNativeOutboundRefusesPromotedSender(t *testing.T) {
	ctx := context.Background()
	sender, u, job := outboundFixture(t)
	life := NewLifecycleStore(sender.ConfigDir)
	nativeRole(t, life, sender.Accounts, "one", "one@example.test", 2, true, true)
	if err := sender.Queue(ctx, u.ID, "11111111-1111-4111-8111-111111111111", job); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("promoted sender queued mail", err)
	}
	nativeRole(t, life, sender.Accounts, "one", "one@example.test", 3, true, false)
	current, err := sender.Accounts.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	job.NativeSendEpoch = current.NativeSendEpoch // role changes revoke earlier send intents
	if err := sender.Queue(ctx, u.ID, "22222222-2222-4222-8222-222222222222", job); err != nil {
		t.Fatal("demoted sender refused", err)
	}
}

// legacyMixedUse grants admission, so it is part of the recovery authority.
func TestNativeRecoveryDigestIncludesLegacyMixedUse(t *testing.T) {
	life, dir, settings, key, challenge := recoveryFixture(t)
	f, err := life.loadNative()
	if err != nil {
		t.Fatal(err)
	}
	k := directoryKey(settings.IssuerURL, "subject")
	a := f.Accounts[k]
	a.LegacyMixedUse = true
	f.Accounts[k] = a
	if err := life.saveNative(f); err != nil {
		t.Fatal(err)
	}
	c, _, _, err := life.nativeRecoveryInputs(dir, settings, key, nil)
	if err != nil || c.AuthorityDigest == challenge.AuthorityDigest {
		t.Fatal("legacyMixedUse missing from the recovery digest", err)
	}
}

// A demotion that cannot read the ledger fails, so the sender retries it; it
// is never recorded over a flag that would return on re-promotion.
func TestNativeLegacyMixedUseDemotionFailsOnUnreadableLedger(t *testing.T) {
	ctx := context.Background()
	config, keyPath, ids := v1Fixture(t)
	if _, err := MigrateNative(ctx, config, keyPath); err != nil {
		t.Fatal(err)
	}
	life := NewLifecycleStore(config)
	accounts, err := users.LoadOrMigrate(ctx, config, filepath.Join(config, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	ledger := life.nativePath()
	saved, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger, []byte("{unreadable"), 0600); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"schemas":[%q],"id":"boss","externalId":"boss","userName":"boss","active":true,"roles":[],"emails":[{"value":"boss@example.test","primary":true}],"meta":{"version":"W/\"3\""}}`, scimUserSchema)
	var demoted DirectoryUser
	if err := json.Unmarshal([]byte(raw), &demoted); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: "boss-3", Type: "user.updated", At: time.Now()}
	apply := func() error {
		_, err := life.ApplyDirectoryUser(nativeIssuer, ev, demoted, EventDigest(ev.Type, []byte(raw)), func() (bool, error) { return true, nil })
		return err
	}
	if err := apply(); err == nil {
		t.Fatal("demotion recorded over an unreadable ledger")
	}
	if d, _, err := life.Directory(nativeIssuer, "boss"); err != nil || d.Revision != 2 {
		t.Fatal("failed demotion advanced the directory", d.Revision, err)
	}
	if err := os.WriteFile(ledger, saved, 0600); err != nil {
		t.Fatal(err)
	}
	if err := apply(); err != nil || legacyFlag(t, life, "boss") {
		t.Fatal("retried demotion did not clear the flag", err)
	}
	if _, err := accounts.SetRole(ids["boss"], users.RoleUser); err != nil {
		t.Fatal(err)
	}
	nativeRole(t, life, accounts, "boss", "boss@example.test", 4, true, true)
	a, _, err := life.NativeAssignment(nativeIssuer, "boss")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := life.AdmitNativeMail(ctx, a.StateRoot, nativeIssuer, ids["boss"], accounts); !errors.Is(err, ErrNativeAdministrator) {
		t.Fatal("re-promotion regained mail", err)
	}
}
