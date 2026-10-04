//go:build linux

package backup

import (
	"context"
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
	for _, id := range []string{interrupted, accepted} {
		if err = store.QueueOutbound(ctx, master, id, job); err != nil {
			t.Fatal(err)
		}
		_, claim, err := store.ClaimOutbound(ctx, master, id, 0, relay.Generation)
		if err != nil {
			t.Fatal(err)
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
