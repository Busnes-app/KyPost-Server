//go:build linux

package sso

import (
	"bytes"
	"context"
	"errors"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeAllocationPreparedBeforePublication(t *testing.T) {
	config, root := t.TempDir(), t.TempDir()
	life := NewLifecycleStore(config)
	domains := provenNativeDomain(t, config)
	accounts, err := users.LoadOrMigrate(context.Background(), config, filepath.Join(config, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	nativeDesired(t, life, "one", "one@example.test", 1, true)
	u, err := life.AllocateNativeAccount(context.Background(), root, nativeIssuer, "one", domains, accounts, nativeLimits, "")
	if err != nil || u.NativeMailboxSource == "" {
		t.Fatal(u, err)
	}
	a, ok, err := life.NativeAssignment(nativeIssuer, "one")
	if err != nil || !ok || a.Owner.Mailbox != u.ID || a.Source != u.NativeMailboxSource {
		t.Fatal(a, err)
	}
	if source, err := mailbox.ValidatePreparedAccount(root, a.Owner, a.Address, nativeLimits); err != nil || source != a.Source {
		t.Fatal(source, err)
	}
	store, err := mailbox.Open(filepath.Join(root, "users", u.ID, "mailbox"), a.Owner, nativeLimits)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: test@example.test\r\nSubject: native\r\n\r\nretained")
	id, err := store.Append(context.Background(), "INBOX", bytes.NewReader(raw), false)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	retry, err := life.AllocateNativeAccount(context.Background(), root, nativeIssuer, "one", domains, accounts, nativeLimits, "")
	if err != nil || retry.ID != u.ID || retry.NativeMailboxSource != u.NativeMailboxSource {
		t.Fatal("retry changed owner", retry, err)
	}
	store, err = mailbox.Open(filepath.Join(root, "users", u.ID, "mailbox"), a.Owner, nativeLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.Raw(context.Background(), "INBOX", id)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("mail changed", err)
	}
	nativeDesired(t, life, "one", "one@example.test", 2, false)
	if _, err = life.AllocateNativeAccount(context.Background(), root, nativeIssuer, "one", domains, accounts, nativeLimits, ""); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("inactive allocated", err)
	}
}

func TestNativeAllocationRefusesLegacyAndCancelledLocks(t *testing.T) {
	config, root := t.TempDir(), t.TempDir()
	life := NewLifecycleStore(config)
	domains := provenNativeDomain(t, config)
	accounts, err := users.LoadOrMigrate(context.Background(), config, filepath.Join(config, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	nativeDesired(t, life, "legacy", "legacy@example.test", 1, true)
	if _, err = accounts.CreateSSOUser("legacy", users.RoleUser, "legacy", "legacy", "legacy@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err = life.AllocateNativeAccount(context.Background(), root, nativeIssuer, "legacy", domains, accounts, nativeLimits, ""); !errors.Is(err, users.ErrSSOSubTaken) {
		t.Fatal("legacy adopted", err)
	}
	if _, found, err := life.NativeAssignment(nativeIssuer, "legacy"); err != nil || found {
		t.Fatal("reserved legacy", err)
	}
	nativeDesired(t, life, "new", "new@example.test", 1, true)
	release, err := fsutil.LockFile(life.path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	_, err = life.AllocateNativeAccount(ctx, root, nativeIssuer, "new", domains, accounts, nativeLimits, "")
	cancel()
	release()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("lock wait not cancelled", err)
	}
	if _, err = life.AllocateNativeAccount(context.Background(), root, nativeIssuer, "new", domains, accounts, nativeLimits, ""); err != nil {
		t.Fatal("cancel abandoned lock", err)
	}
}

func TestNativeAllocationRepairsAcknowledgedUnpublishedReservation(t *testing.T) {
	config, root := t.TempDir(), t.TempDir()
	life := NewLifecycleStore(config)
	domains := provenNativeDomain(t, config)
	accounts, err := users.LoadOrMigrate(context.Background(), config, filepath.Join(config, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	nativeDesired(t, life, "collision", "collision@example.test", 1, true)
	if _, err = accounts.CreateSSOUser("collision", users.RoleUser, "different", "", ""); err != nil {
		t.Fatal(err)
	}
	a, err := life.ReconcileNativeMailbox(root, nativeIssuer, "collision", "reserved-local-id", "example.test", nativeLimits)
	if err != nil {
		t.Fatal(err)
	}
	u, err := life.AllocateNativeAccount(context.Background(), root, nativeIssuer, "collision", domains, accounts, nativeLimits, "")
	if err != nil || u.ID != a.Owner.Mailbox || u.NativeMailboxSource != a.Source || u.Username != "native-reserved-local-id" {
		t.Fatal("lost users publication changed reservation", u, err)
	}
	if _, err = life.AllocateNativeAccount(context.Background(), root, "https://other.test", "collision", domains, accounts, nativeLimits, ""); !errors.Is(err, ErrNativeDomain) {
		t.Fatal("wrong issuer allocated", err)
	}
}

func TestNativeAllocationCannotOutliveProofDuringContention(t *testing.T) {
	config, root := t.TempDir(), t.TempDir()
	life := NewLifecycleStore(config)
	domains := provenNativeDomain(t, config)
	accounts, err := users.LoadOrMigrate(context.Background(), config, filepath.Join(config, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	nativeDesired(t, life, "expiry", "expiry@example.test", 1, true)
	proof, err := domains.Read()
	if err != nil {
		t.Fatal(err)
	}
	proof.ExpiresAt = time.Now().Add(2 * time.Second).Unix()
	if err = domains.persist(proof); err != nil {
		t.Fatal(err)
	}
	release, err := fsutil.LockFile(life.path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err = life.AllocateNativeAccount(ctx, root, nativeIssuer, "expiry", domains, accounts, nativeLimits, "")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatal("allocation outlived proof", err, time.Since(start))
	}
	if _, err = accounts.GetBySSOSub("expiry"); !errors.Is(err, users.ErrNotFound) {
		t.Fatal("expired proof published", err)
	}
}

// Multi mode (tenancy design "Identity binding", step 2): a crash after the mailbox is
// reserved but before users.json is written leaves the reserved ID, so the retry
// publishes that ID with the org, never a second one; a retry naming another org is
// refused and changes nothing.
func TestNativeAllocationKeepsItsIDAndOrgAcrossACrash(t *testing.T) {
	const org, other = "1a2b3c4d-0000-4000-8000-00000000000a", "1a2b3c4d-0000-4000-8000-00000000000b"
	config, root := t.TempDir(), t.TempDir()
	life := NewLifecycleStore(config)
	domains := provenNativeDomain(t, config)
	accounts, err := users.LoadOrMigrate(context.Background(), config, filepath.Join(config, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	hostedDomainsFixture(t, config)
	hostedDesired(t, life, "member", "member@example.test", "member@example.test", 1)
	reserved, err := life.ReconcileNativeMailbox(root, nativeIssuer, "member", "reserved-id", "example.test", nativeLimits)
	if err != nil {
		t.Fatal(err)
	}
	u, err := life.AllocateNativeAccount(context.Background(), root, nativeIssuer, "member", domains, accounts, nativeLimits, org)
	if err != nil || u.ID != reserved.Owner.Mailbox || u.OrgID != org {
		t.Fatal("crash retry", u, err)
	}
	if _, err := life.AllocateNativeAccount(context.Background(), root, nativeIssuer, "member", domains, accounts, nativeLimits, other); !errors.Is(err, users.ErrNativeAccountConflict) {
		t.Fatal("another org was accepted", err)
	}
	if again, err := accounts.GetBySSOSub("member"); err != nil || again.ID != u.ID || again.OrgID != org {
		t.Fatal("account changed", again, err)
	}
}
