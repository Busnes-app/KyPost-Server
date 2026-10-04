//go:build linux

package backup

import (
	"bytes"
	"context"
	"database/sql"
	"net/textproto"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

func TestNativeOutboxSealedClaimsSentAndDependencies(t *testing.T) {
	ctx := context.Background()
	s, u := nativeService(t)
	life := sso.NewLifecycleStore(s.dirs.Config)
	assignment, _, err := life.NativeAssignment(u.NativeMailboxIssuer, u.SSOSub)
	if err != nil {
		t.Fatal(err)
	}
	configPath, keyPath := filepath.Join(s.dirs.Config, "native-relay.json"), filepath.Join(s.dirs.Secret, "native-relay.key")
	relay, err := mailmsg.SaveDomainRelay(ctx, configPath, keyPath, mailmsg.DomainRelay{Domain: "example.test", Issuer: u.NativeMailboxIssuer, Host: "smtp.provider.test", Port: 465, Username: "operator-login", Password: "operator-secret"})
	if err != nil {
		t.Fatal(err)
	}
	master, err := cryptutil.LoadKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := mailbox.OpenExisting(filepath.Join(s.dirs.State, "users", u.ID, "mailbox"), assignment.Owner, assignment.Limits, assignment.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw := []byte("From: one@example.test\r\nTo: receiver@outside.test\r\nSubject: private payload\r\n\r\nexact private wire\r\n")
	job := mailbox.OutboundJob{From: "one@example.test", RelayGeneration: relay.Generation, Deliveries: []mailbox.OutboundDelivery{{Recipients: []string{"receiver@outside.test"}, Raw: raw}}, Sent: raw}
	interrupted := "07a7cd86-6d9d-4b91-890d-c6d78d096091"
	accepted := "07a7cd86-6d9d-4b91-890d-c6d78d096092"
	queued := "07a7cd86-6d9d-4b91-890d-c6d78d096093"
	retryable := "07a7cd86-6d9d-4b91-890d-c6d78d096094"
	uncertain := "07a7cd86-6d9d-4b91-890d-c6d78d096095"
	failed := "07a7cd86-6d9d-4b91-890d-c6d78d096096"
	for _, id := range []string{interrupted, accepted, queued, retryable, uncertain, failed} {
		if err = store.QueueOutbound(ctx, master, id, job); err != nil {
			t.Fatal(err)
		}
		if id == queued {
			continue
		}
		_, claim, err := store.ClaimOutbound(ctx, master, id, 0, relay.Generation)
		if err != nil {
			t.Fatal(err)
		}
		outcomes := map[string]error{
			retryable: &textproto.Error{Code: 451, Msg: "temporary rejection"},
			uncertain: mailmsg.ErrSMTPAcceptanceUncertain,
			failed:    &textproto.Error{Code: 550, Msg: "permanent rejection"},
		}
		if outcome, ok := outcomes[id]; ok {
			if err = store.CompleteOutbound(ctx, id, 0, claim, outcome); err != nil {
				t.Fatal(err)
			}
		}
		if id == accepted {
			if err = store.CompleteOutbound(ctx, id, 0, claim, nil); err != nil {
				t.Fatal(err)
			}
			if _, err = store.FileOutboundSent(ctx, master, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	recoveryKey := pinTestKey(t, s)
	result, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := os.ReadFile(result.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	// Collection must not mutate live work. The queued snapshot may be older
	// than a subsequent accepted submission, whose evidence a restore loses.
	_, liveStatuses, _, err := store.ReadOutbound(ctx, master, queued)
	if err != nil || liveStatuses[0].State != "queued" {
		t.Fatal("collection mutated live work", liveStatuses, err)
	}
	_, claim, err := store.ClaimOutbound(ctx, master, queued, 0, relay.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteOutbound(ctx, queued, 0, claim, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = store.FileOutboundSent(ctx, master, queued); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	manifest, _, err := capsule.Open(sealed, recoveryKey, restored)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range drillChecks(restored, manifest) {
		if !check.Passed {
			t.Fatal("drill", check.Name)
		}
	}
	restoredPath := filepath.Join(restored, "state/users", u.ID, "mailbox/mailbox.db")
	has, err := mailbox.ValidateOutboundSnapshot(ctx, restoredPath, master, relay)
	if err != nil || !has {
		t.Fatal("outbox did not survive sealed snapshot", has, err)
	}
	native, err := QuarantineNativeRestore(restored)
	if err != nil || !native || sso.RequireNativeRestoreReleased(filepath.Join(restored, "state")) == nil {
		t.Fatal("restored outbox not held", native, err)
	}
	// Repeating quarantine must neither reauthorize mail nor rewrite frozen intent.
	if _, err = QuarantineNativeRestore(restored); err != nil {
		t.Fatal(err)
	}
	restoredStore, err := mailbox.OpenExisting(filepath.Dir(restoredPath), assignment.Owner, assignment.Limits, assignment.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredStore.Close()
	for id, want := range map[string]string{interrupted: "submitting", accepted: "accepted", queued: "quarantined", retryable: "quarantined", uncertain: "uncertain", failed: "failed"} {
		got, statuses, sentID, err := restoredStore.ReadOutbound(ctx, master, id)
		if err != nil || len(statuses) != 1 || statuses[0].State != want || !bytes.Equal(got.Sent, raw) || !bytes.Equal(got.Deliveries[0].Raw, raw) || (sentID != 0) != (id == accepted) {
			t.Fatalf("restored %s: states=%+v sent=%d err=%v", id, statuses, sentID, err)
		}
		if id == queued || id == retryable {
			if statuses[0].NextAttempt != 0 {
				t.Fatal("restored retry timer survived quarantine")
			}
		}
		if _, _, err := restoredStore.ClaimOutbound(ctx, master, id, 0, relay.Generation); err == nil {
			t.Fatal("restored job authorized resubmission", id)
		}
	}
	pending, err := restoredStore.PendingOutbound(ctx, 10)
	if err != nil || len(pending) != 0 {
		t.Fatal("restored work scheduled automatically", pending, err)
	}
	// Even a recipe that predates outbox support must not claim queue verification.
	recipe := manifest.VerificationRecipe.(map[string]any)
	delete(recipe, "outbox")
	passed := true
	for _, check := range drillChecks(restored, manifest) {
		passed = passed && check.Passed
	}
	if passed {
		t.Fatal("old recipe attested outbox recovery")
	}
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Collect(); err == nil {
		t.Fatal("sealed outbox without master key")
	}
	if err = os.WriteFile(keyPath, keyBytes, 0600); err != nil {
		t.Fatal(err)
	}
	relayBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Collect(); err == nil {
		t.Fatal("sealed outbox without relay authority")
	}
	if err = os.WriteFile(configPath, relayBytes, 0600); err != nil {
		t.Fatal(err)
	}
	wrong := append([]byte(nil), master...)
	wrong[0] ^= 1
	if _, err = mailbox.ValidateOutboundSnapshot(ctx, restoredPath, wrong, relay); err == nil {
		t.Fatal("wrong queue key accepted")
	}
}

func TestNativeOutboxRestoreQuarantineRefusesMissingOrPartialDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	if err := quarantineRestoredOutbox(path); err == nil {
		t.Fatal("missing database accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing database recreated", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE outbox(id TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := quarantineRestoredOutbox(path); err == nil {
		t.Fatal("partial queue schema accepted")
	}
}

func TestNativeOutboxRestoreQuarantineRelativePathAndOlderDatabase(t *testing.T) {
	t.Chdir(t.TempDir())
	path := "mailbox.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("CREATE TABLE messages(id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if err = quarantineRestoredOutbox(path); err != nil {
		t.Fatal("older database refused", err)
	}
	if _, err = db.Exec("CREATE TABLE outbox(id TEXT); CREATE TABLE outbox_deliveries(state TEXT,next_attempt INTEGER); INSERT INTO outbox_deliveries VALUES('queued',42)"); err != nil {
		t.Fatal(err)
	}
	if err = quarantineRestoredOutbox(path); err != nil {
		t.Fatal("relative restore path refused", err)
	}
	var state string
	var next int
	if err = db.QueryRow("SELECT state,next_attempt FROM outbox_deliveries").Scan(&state, &next); err != nil || state != "quarantined" || next != 0 {
		t.Fatal("relative restore work not quarantined", state, next, err)
	}
}
