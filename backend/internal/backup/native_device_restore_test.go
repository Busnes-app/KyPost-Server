//go:build linux

package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func seedRestoreDevice(t *testing.T, st *state.Store, userID string) string {
	t.Helper()
	if err := st.UpsertNativeDevice(state.NativeDevice{DeviceID: "old-device", UserID: userID, Platform: "android", PushToken: "old-push-token", MFAApprover: true, SecretHash: users.HashDeviceSecret("old-device-secret")}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertNotificationSubscription(state.NotificationSubscription{Endpoint: "https://push.example.test/old-browser", Auth: "old-auth", P256DH: "old-public-key"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCheckpoint("retained-checkpoint"); err != nil {
		t.Fatal(err)
	}
	subscriber, err := st.GetOrCreateSubscriberID()
	if err != nil {
		t.Fatal(err)
	}
	return subscriber
}

func TestNativeRestoreRevokesDevicesAndPairingPreservesMailAndLegacy(t *testing.T) {
	ctx := context.Background()
	s, u := nativeService(t)
	assignment, _, err := sso.NewLifecycleStore(s.dirs.Config).NativeAssignment(u.NativeMailboxIssuer, u.SSOSub)
	if err != nil {
		t.Fatal(err)
	}
	account := filepath.Join(s.dirs.State, "users", u.ID)
	st, err := state.OpenNative(account, u.NativeMailboxSource)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	oldSubscriber := seedRestoreDevice(t, st, u.ID)
	contactBytes := []byte(`{"contacts":[{"uid":"retained-contact","fn":"Retained contact","rev":1,"createdAt":"2026-10-03T00:00:00Z","updatedAt":"2026-10-03T00:00:00Z"}],"seq":1}`)
	if err := os.WriteFile(filepath.Join(account, "contacts.json"), contactBytes, 0600); err != nil {
		t.Fatal(err)
	}
	legacy := users.User{ID: "legacy", Username: "legacy", Role: users.RoleUser, Active: true}
	hash, err := users.HashPassword(ctx, "historical-carddav-password")
	if err != nil {
		t.Fatal(err)
	}
	davBytes, err := json.Marshal(map[string]string{"hash": hash, "createdAt": "2026-10-03T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{u.ID, legacy.ID} {
		dir := filepath.Join(s.dirs.Config, "users", id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "carddav-auth.json"), davBytes, 0600); err != nil {
			t.Fatal(err)
		}
	}
	legacyStore, err := state.New(filepath.Join(s.dirs.State, "users", legacy.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer legacyStore.Close()
	legacySubscriber := seedRestoreDevice(t, legacyStore, legacy.ID)
	userBytes, err := json.Marshal(map[string]any{"users": []users.User{u, legacy}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dirs.Config, "users.json"), userBytes, 0600); err != nil {
		t.Fatal(err)
	}
	mail, err := mailbox.OpenExisting(filepath.Join(account, "mailbox"), assignment.Owner, assignment.Limits, assignment.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer mail.Close()
	raw := []byte("From: sender@outside.test\r\nTo: one@example.test\r\nSubject: retained ciphertext\r\nContent-Type: application/pgp-encrypted\r\n\r\nopaque original bytes\x00\r\n")
	receipt := mailbox.Receipt{Gateway: "maddy", Delivery: "retained-delivery", Sender: "sender@outside.test", Recipients: []mailbox.Recipient{{Address: "one@example.test", Generation: assignment.Revision}}}
	id, err := mail.Import(ctx, receipt, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	key := pinTestKey(t, s)
	result, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := os.ReadFile(result.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "recovered")
	if _, _, err := capsule.Open(sealed, key, restored); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(restored, "config/users", u.ID, "carddav-auth.json")); err != nil || !bytes.Equal(b, davBytes) {
		t.Fatal("sealed fixture lacks historical CardDAV credential", err)
	}
	liveGeneration := mail.MessageReferenceGeneration()
	priorGeneration := liveGeneration
	// Two passes prove interruption/retry convergence; neither restores trust.
	for range 2 {
		if native, err := QuarantineNativeRestore(restored); !native || err != nil {
			t.Fatal(native, err)
		}
		rotated, err := mailbox.OpenExisting(filepath.Join(restored, "state/users", u.ID, "mailbox"), assignment.Owner, assignment.Limits, assignment.Source)
		if err != nil {
			t.Fatal(err)
		}
		gotGeneration := rotated.MessageReferenceGeneration()
		if err := rotated.Close(); err != nil {
			t.Fatal(err)
		}
		if gotGeneration == "" || gotGeneration == priorGeneration {
			t.Fatal("restore did not rotate native reference generation")
		}
		priorGeneration = gotGeneration
	}
	if !errors.Is(sso.RequireNativeRestoreReleased(filepath.Join(restored, "state")), sso.ErrNativeRestoreHold) {
		t.Fatal("credential revocation released native hold")
	}
	if _, err := os.Lstat(filepath.Join(restored, "config/users", u.ID, "carddav-auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("native restored CardDAV credential retained", err)
	}
	for _, path := range []string{
		filepath.Join(restored, "config/users", legacy.ID, "carddav-auth.json"),
		filepath.Join(s.dirs.Config, "users", u.ID, "carddav-auth.json"),
	} {
		if b, err := os.ReadFile(path); err != nil || !bytes.Equal(b, davBytes) {
			t.Fatal("legacy or original CardDAV credential changed", path, err)
		}
	}
	restoredAccount := filepath.Join(restored, "state/users", u.ID)
	if b, err := os.ReadFile(filepath.Join(restoredAccount, "contacts.json")); err != nil || !bytes.Equal(b, contactBytes) {
		t.Fatal("credential revocation changed contacts", err)
	}
	restoredState, err := state.OpenNative(restoredAccount, u.NativeMailboxSource)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredState.Close()
	if _, ok := restoredState.GetNativeDevice("old-device"); ok {
		t.Fatal("restored credential/MFA device still resolves")
	}
	checkpoint, err := restoredState.Checkpoint()
	if err != nil || checkpoint != "retained-checkpoint" {
		t.Fatal("restored checkpoint changed", checkpoint, err)
	}
	subs, err := restoredState.ListNotificationSubscriptionsStrict()
	if err != nil || len(subs) != 0 || restoredState.SubscriberID() == oldSubscriber || restoredState.SubscriberID() == "" {
		t.Fatal("restored targets/token authority or checkpoint", subs, err)
	}
	restoredLegacy, err := state.New(filepath.Join(restored, "state/users", legacy.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer restoredLegacy.Close()
	if _, ok := restoredLegacy.GetNativeDevice("old-device"); !ok || restoredLegacy.SubscriberID() != legacySubscriber {
		t.Fatal("legacy restore credentials changed")
	}
	restoredMail, err := mailbox.OpenExisting(filepath.Join(restoredAccount, "mailbox"), assignment.Owner, assignment.Limits, assignment.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredMail.Close()
	got, err := restoredMail.Raw(ctx, "INBOX", id)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("credential revocation changed retained mail", err)
	}
	if repeated, err := restoredMail.Import(ctx, receipt, bytes.NewReader(raw)); err != nil || repeated != id {
		t.Fatal("credential revocation changed receiving receipt", repeated, err)
	}
	gotUsers, err := os.ReadFile(filepath.Join(restored, "config/users.json"))
	if err != nil || !bytes.Equal(gotUsers, userBytes) {
		t.Fatal("account/key material document changed", err)
	}
	liveAgain, err := mailbox.OpenExisting(filepath.Join(account, "mailbox"), assignment.Owner, assignment.Limits, assignment.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer liveAgain.Close()
	if liveAgain.MessageReferenceGeneration() != liveGeneration {
		t.Fatal("restore mutated live reference generation")
	}
	if _, ok := st.GetNativeDevice("old-device"); !ok || st.SubscriberID() != oldSubscriber {
		t.Fatal("backup collection revoked live credentials")
	}
}

func TestNativeRestoreDeviceRevocationSourceFenceAndAtomicFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	source := "native:" + strings.Repeat("1", 64)
	st, err := state.NewNative("account", source)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	before := seedRestoreDevice(t, st, "one")
	path := filepath.Join("account", "state.db")
	if err := revokeRestoredDeviceCredentials(path, "native:"+strings.Repeat("2", 64)); !errors.Is(err, state.ErrMailSource) {
		t.Fatal("foreign source admitted", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TRIGGER fail_rotation BEFORE UPDATE ON meta WHEN NEW.key='subscriber_id' BEGIN SELECT RAISE(ABORT,'injected storage refusal'); END"); err != nil {
		t.Fatal(err)
	}
	if err := revokeRestoredDeviceCredentials(path, source); err == nil {
		t.Fatal("failed rotation reported success")
	}
	subs, err := st.ListNotificationSubscriptionsStrict()
	if _, ok := st.GetNativeDevice("old-device"); !ok || st.SubscriberID() != before || err != nil || len(subs) != 1 {
		t.Fatal("failed revocation partially committed", err)
	}
	if _, err := db.Exec("DROP TRIGGER fail_rotation"); err != nil {
		t.Fatal(err)
	}
	if err := revokeRestoredDeviceCredentials(path, source); err != nil {
		t.Fatal("relative path revocation failed", err)
	}
	if _, ok := st.GetNativeDevice("old-device"); ok || st.SubscriberID() == before {
		t.Fatal("successful revocation retained authority")
	}
	missing := filepath.Join("account", "missing.db")
	if err := revokeRestoredDeviceCredentials(missing, source); err == nil {
		t.Fatal("missing account state adopted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("missing account state recreated", err)
	}
}
