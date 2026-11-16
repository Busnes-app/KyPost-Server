//go:build linux

package sso

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestHRAddressReservationBeforeIdentity(t *testing.T) {
	config, root := t.TempDir(), t.TempDir()
	ctx := context.Background()
	life := NewLifecycleStore(config)
	domains := provenNativeDomain(t, config)
	reserve := func(subject string) string {
		t.Helper()
		address, err := life.ReserveHRAddress(ctx, root, nativeIssuer, subject, "Alice.Adams", "example.test", domains, nativeLimits)
		if err != nil {
			t.Fatal(err)
		}
		return address
	}
	first := reserve("hr-one")
	if first != "alice.adams@example.test" {
		t.Fatal(first)
	}
	if other := reserve("hr-two"); other != "alice.adams.2@example.test" {
		t.Fatal(other)
	}
	if address, err := life.ReserveHRAddress(ctx, root+string(filepath.Separator), nativeIssuer, "hr-two", "Alice.Adams", "example.test", domains, nativeLimits); err != nil || address != "alice.adams.2@example.test" {
		t.Fatal("noncanonical configured root changed reservation", address, err)
	}
	life = NewLifecycleStore(config)
	if retry := reserve("hr-one"); retry != first {
		t.Fatal("retry changed address", retry)
	}
	if _, found, err := life.Directory(nativeIssuer, "hr-one"); err != nil || found {
		t.Fatal("reservation created directory grant", err)
	}
	a, found, err := life.NativeAssignment(nativeIssuer, "hr-one")
	if err != nil || !found || a.DesiredActive || a.Source != "" || a.Status != "pending" {
		t.Fatal(a, err)
	}
	if _, err := os.Stat(filepath.Join(root, "users", a.Owner.Mailbox)); !os.IsNotExist(err) {
		t.Fatal("reservation created mailbox storage", err)
	}
	x, err := life.NativeAddress(first)
	if err != nil || x.State != "disabled" {
		t.Fatal("reservation routes mail", x, err)
	}
	if err := life.authority().View(ctx, func(r authorityReader) error { return r.Check() }); err != nil {
		t.Fatal("reservation is not valid backup authority", err)
	}
	if _, err := life.ValidateNativeSnapshot(root, nil); err != nil {
		t.Fatal("address-only reservation broke backups", err)
	}
	accounts, err := users.LoadOrMigrate(ctx, config, filepath.Join(config, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	nativeDesired(t, life, "hr-one", first, 1, true)
	u, err := life.AllocateNativeAccount(ctx, root, nativeIssuer, "hr-one", domains, accounts, nativeLimits)
	if err != nil || u.ID != a.Owner.Mailbox || u.NativeMailboxSource == "" {
		t.Fatal("activation did not consume reservation", u, err)
	}
	if retry := reserve("hr-one"); retry != first {
		t.Fatal("retry after activation changed address")
	}
	if _, err := life.ReserveHRAddress(ctx, root, nativeIssuer, "hr-one", "alice", "other.test", domains, nativeLimits); err == nil {
		t.Fatal("changed domain accepted")
	}
	if _, err := life.ReserveHRAddress(ctx, root, nativeIssuer, "../escape", "alice", "example.test", domains, nativeLimits); err == nil {
		t.Fatal("path traversal accepted")
	}
	nativeDesired(t, life, "foreign", "foreign@example.test", 1, true)
	if _, err := life.ReserveHRAddress(ctx, root, nativeIssuer, "foreign", "foreign", "example.test", domains, nativeLimits); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("existing subject adopted", err)
	}
	// Restoring an unrelated live account must remain possible while a hire
	// has no identity/provisioning revision yet. Its reservation stays sealed.
	if err := fsutil.PersistJSONFile(filepath.Join(root, NativeRestoreHoldFile), map[string]any{"version": 1, "epoch": "12345678-1234-4123-8123-123456789abc"}); err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("k", 32))
	fingerprint := sha256.Sum256(key)
	settings := SSOSettings{Enabled: true, IssuerURL: nativeIssuer, ClientID: "kypost"}
	accountList, err := accounts.List()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := life.ValidateNativeSnapshot(root, accountList); err != nil {
		t.Fatal("reservation broke restored snapshot", err)
	}
	release, err := life.LockDirectory()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	challenge, err := life.BeginNativeRecoveryHeld(ctx, root, settings, key, accountList, "paired-system", hex.EncodeToString(fingerprint[:]))
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range challenge.Subjects {
		if sub == "hr-two" {
			t.Fatal("uncreated identity requires impossible recovery evidence")
		}
	}
	body, headers := recoveryPayload(t, challenge, key, func(e *nativeRecoveryEvidence) {
		e.Subjects = nil
		for _, sub := range challenge.Subjects {
			revision := int64(2)
			profile, _ := json.Marshal(map[string]any{"id": sub, "externalId": sub, "active": false, "roles": []string{}})
			e.Subjects = append(e.Subjects, nativeRecoverySubject{ID: sub, Revision: &revision, Profile: profile})
		}
	})
	if err := life.AcceptNativeRecoveryHeld(ctx, root, settings, key, accountList, body, headers); err != nil {
		t.Fatal("pending hire blocks recovery evidence", err)
	}
	if _, found, err := life.Directory(nativeIssuer, "hr-two"); err != nil || found {
		t.Fatal("recovery invented identity", err)
	}
	if !errors.Is(RequireNativeRestoreReleased(root), ErrNativeRestoreHold) {
		t.Fatal("evidence bypassed restore hold")
	}
}
