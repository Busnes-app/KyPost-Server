//go:build linux

package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func receivingFixture(t *testing.T) (*receivingRuntime, []users.User) {
	t.Helper()
	ctx := context.Background()
	configDir, stateDir := t.TempDir(), t.TempDir()
	for _, dir := range []string{configDir, stateDir} {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	accounts, err := users.LoadOrMigrate(ctx, configDir, filepath.Join(configDir, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	const issuer = "https://identity.example.test"
	if err := sso.NewStore(configDir).Save(sso.SSOSettings{Enabled: true, IssuerURL: issuer, ClientID: "kypost"}); err != nil {
		t.Fatal(err)
	}
	r := &receivingRuntime{configDir: configDir, stateDir: stateDir, accounts: accounts, life: sso.NewLifecycleStore(configDir), domains: sso.NewNativeDomainStore(configDir)}
	if _, err := r.domains.Configure(ctx, "example.test", issuer); err != nil {
		t.Fatal(err)
	}
	r.domains.SetLookupForTest(func(context.Context, string) ([]string, error) {
		d, err := r.domains.Read()
		return []string{d.RecordValue()}, err
	})
	var created []users.User
	for _, subject := range []string{"one", "two"} {
		raw := []byte(fmt.Sprintf(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":%q,"externalId":%q,"userName":%q,"active":true,"emails":[{"value":%q,"primary":true}],"meta":{"version":"W/\"1\""}}`, subject, subject, subject, subject+"@example.test"))
		var resource sso.DirectoryUser
		if err := json.Unmarshal(raw, &resource); err != nil {
			t.Fatal(err)
		}
		ev := syncauth.Event{ID: "create-" + subject, Type: "user.created", At: time.Now()}
		if _, err := r.life.ApplyDirectoryUser(issuer, ev, resource, sso.EventDigest(ev.Type, raw), func() (bool, error) { return false, nil }); err != nil {
			t.Fatal(err)
		}
		u, err := r.life.AllocateNativeAccount(ctx, stateDir, issuer, subject, r.domains, accounts, mailbox.Limits{MessageBytes: 5 << 20, PayloadBytes: 32 << 20, Records: 10000})
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, u)
	}
	r.holding, err = ingress.Open(filepath.Join(stateDir, "receiving"), receivingLimits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.holding.Close() })
	return r, created
}

func TestNativeReceivingCommitsFrozenRecipientsWithoutIMAP(t *testing.T) {
	r, created := receivingFixture(t)
	ctx := context.Background()
	const id = "receiver-generated-transaction"
	for _, address := range []string{"one@example.test", "two@example.test"} {
		if err := r.bind(ctx, id, "sender@outside.test", address); err != nil {
			t.Fatal(err)
		}
	}
	raw := []byte("From: sender@outside.test\r\nTo: one@example.test\r\nMessage-ID: <forged-owner@outside.test>\r\nSubject: exact bytes\r\n\r\nbody\r\n")
	if err := r.accept(ctx, id, "sender@outside.test", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	// Expired routes and loss of live DNS must not strand accepted mail.
	for _, u := range created {
		a, _, err := r.life.NativeAssignment(u.NativeMailboxIssuer, u.SSOSub)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.holding.SetRoute(ctx, ingress.Route{Address: a.Address, Issuer: a.Owner.Issuer, Subject: a.Owner.Subject, Mailbox: a.Owner.Mailbox, Generation: 1, Active: true, ValidUntil: time.Now().Add(-time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	r.domains.SetLookupForTest(func(context.Context, string) ([]string, error) { return nil, errors.New("DNS unavailable") })
	if err := r.importDelivery(ctx, id); err != nil {
		t.Fatal(err)
	}
	d, err := r.holding.Get(ctx, receivingGateway, id)
	if err != nil || d.State != "archived" || len(d.Raw) != 0 {
		t.Fatalf("holding acknowledgment: %+v %v", d, err)
	}
	for _, u := range created {
		a, _, err := r.life.NativeAssignment(u.NativeMailboxIssuer, u.SSOSub)
		if err != nil {
			t.Fatal(err)
		}
		s, err := mailbox.OpenExisting(filepath.Join(r.stateDir, "users", u.ID, "mailbox"), a.Owner, a.Limits, a.Source)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Raw(ctx, "INBOX", 1)
		_ = s.Close()
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("frozen owner lost exact raw message: %v", err)
		}
	}
}

func TestNativeReceivingRefusesRevokedRecipientBeforeData(t *testing.T) {
	r, created := receivingFixture(t)
	ctx := context.Background()
	if err := r.bind(ctx, "offboarded", "", "one@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.accounts.Deactivate(created[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := r.accept(ctx, "offboarded", "", bytes.NewBufferString("From: test@outside.test\r\n\r\nnot accepted")); err == nil {
		t.Fatal("accepted DATA for revoked frozen recipient")
	}
	d, err := r.holding.Get(ctx, receivingGateway, "offboarded")
	if err != nil || d.State != "staged" || len(d.Raw) != 0 {
		t.Fatalf("revoked DATA changed durable holding state: %+v %v", d, err)
	}
}

// Promotion refuses new and frozen recipients; accepted mail stays held.
func TestNativeReceivingRefusesPromotedRecipient(t *testing.T) {
	r, created := receivingFixture(t)
	ctx := context.Background()
	if err := r.bind(ctx, "before-promotion", "", "one@example.test"); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":"one","externalId":"one","userName":"one","active":true,"roles":["kypost.admin"],"emails":[{"value":"one@example.test","primary":true}],"meta":{"version":"W/\"2\""}}`)
	var resource sso.DirectoryUser
	if err := json.Unmarshal(raw, &resource); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: "promote-one", Type: "user.updated", At: time.Now()}
	if _, err := r.life.ApplyDirectoryUser("https://identity.example.test", ev, resource, sso.EventDigest(ev.Type, raw), func() (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := r.accounts.SetRole(created[0].ID, users.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := r.bind(ctx, "after-promotion", "", "one@example.test"); err == nil {
		t.Fatal("bound mail for a promoted recipient")
	}
	if err := r.accept(ctx, "before-promotion", "", bytes.NewBufferString("From: test@outside.test\r\n\r\nnot accepted")); err == nil {
		t.Fatal("accepted DATA for a promoted recipient")
	}
	d, err := r.holding.Get(ctx, receivingGateway, "before-promotion")
	if err != nil || d.State != "staged" || len(d.Raw) != 0 {
		t.Fatalf("promotion changed durable holding state: %+v %v", d, err)
	}
}

func TestNativeReceivingAuthorityFencesLocalDeactivation(t *testing.T) {
	r, created := receivingFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	other, err := users.OpenExisting(ctx, r.configDir)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	finished := make(chan error, 1)
	go func() {
		finished <- r.withAuthority(ctx, []string{created[0].ID}, []string{"one@example.test"}, nil, func(map[string]sso.NativeAssignment) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case err := <-finished:
		t.Fatalf("authority action failed before entry: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// A second process/store must contend on the disk fence too.
	openingCtx, cancelOpening := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancelOpening()
	opened, err := users.OpenExisting(openingCtx, r.configDir)
	if err == nil || opened != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("existing authority opener bypassed in-flight commit: %v", err)
	}
	deactivated := make(chan error, 1)
	go func() { _, err := other.Deactivate(created[0].ID); deactivated <- err }()
	select {
	case err := <-deactivated:
		t.Fatalf("local deactivation bypassed commit fence: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-finished; err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := <-deactivated; err != nil {
		t.Fatal(err)
	}
	called := false
	err = r.withAuthority(context.Background(), []string{created[0].ID}, []string{"one@example.test"}, nil, func(map[string]sso.NativeAssignment) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("revoked local authority entered receive commit")
	}
}

func TestNativeReceivingExistingSpoolDoesNotRecreateLostMail(t *testing.T) {
	root := t.TempDir()
	limits := receivingLimits
	missing := filepath.Join(root, "missing")
	if s, err := ingress.OpenExisting(missing, limits); err == nil || s != nil {
		t.Fatal("created missing holding directory")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing spool was modified: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "ingress.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := ingress.OpenExisting(root, limits); err == nil || s != nil {
		t.Fatal("initialized an empty replacement holding database")
	}
}

func TestNativeReceivingRetainsAcceptedMailThroughLocalOffboarding(t *testing.T) {
	r, created := receivingFixture(t)
	ctx := context.Background()
	const id = "accepted-before-local-revocation"
	raw := []byte("From: test@outside.test\r\n\r\nretained body\r\n")
	if err := r.bind(ctx, id, "", "one@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := r.accept(ctx, id, "", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.accounts.Deactivate(created[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := r.importDelivery(ctx, id); err == nil {
		t.Fatal("imported accepted mail into an offboarded account")
	}
	d, err := r.holding.Get(ctx, receivingGateway, id)
	if err != nil || d.State != "pending" || !bytes.Equal(d.Raw, raw) {
		t.Fatalf("offboarding discarded accepted mail: state=%s error=%v", d.State, err)
	}
	// Local reactivation at the same signed revision restores the same owner.
	if _, err := r.accounts.Reactivate(created[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := r.importDelivery(ctx, id); err != nil {
		t.Fatal(err)
	}
	d, err = r.holding.Get(ctx, receivingGateway, id)
	if err != nil || d.State != "archived" || len(d.Raw) != 0 {
		t.Fatalf("reactivated owner did not complete delivery: state=%s error=%v", d.State, err)
	}
}

func TestNativeReceivingRestoreHoldPreservesAcceptedMail(t *testing.T) {
	r, _ := receivingFixture(t)
	ctx := context.Background()
	const id = "accepted-before-restore-hold"
	raw := []byte("From: test@outside.test\r\n\r\nrestore obligation\r\n")
	if err := r.bind(ctx, id, "", "one@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := r.accept(ctx, id, "", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.stateDir, sso.NativeRestoreHoldFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.importDelivery(ctx, id); !errors.Is(err, sso.ErrNativeRestoreHold) {
		t.Fatalf("restore hold did not refuse delivery: %v", err)
	}
	d, err := r.holding.Get(ctx, receivingGateway, id)
	if err != nil || d.State != "pending" || !bytes.Equal(d.Raw, raw) {
		t.Fatalf("restore hold lost accepted mail: state=%s error=%v", d.State, err)
	}
}

func TestNativeReceivingStalledPipeRefusesBeforeAuthority(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- (&receivingRuntime{}).accept(ctx, "stalled", "", reader) }()
	select {
	case err := <-finished:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("stalled body must expire before authority access: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stalled stdin ignored receiving deadline")
	}
}

// Routes carry the address generation, so a newer signed revision that
// changes no address state (an ordinary profile edit) leaves accepted mail
// deliverable. Phase 2 routed on the directory revision and quarantined here.
func TestNativeReceivingDirectoryEditKeepsAcceptedMail(t *testing.T) {
	r, created := receivingFixture(t)
	ctx := context.Background()
	const id = "accepted-before-profile-edit"
	raw := []byte("From: test@outside.test\r\n\r\nstill owed\r\n")
	if err := r.bind(ctx, id, "", "one@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := r.accept(ctx, id, "", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	u := created[0]
	directory, known, err := r.life.Directory(u.NativeMailboxIssuer, u.SSOSub)
	if err != nil || !known {
		t.Fatal(err)
	}
	resource := *directory.Resource
	resource.Meta.Version = `W/"2"`
	encoded, err := json.Marshal(resource)
	if err != nil {
		t.Fatal(err)
	}
	event := syncauth.Event{ID: "newer-same-owner", Type: "user.updated", At: time.Now()}
	if _, err := r.life.ApplyDirectoryUser(u.NativeMailboxIssuer, event, resource, sso.EventDigest(event.Type, encoded), func() (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.importDelivery(ctx, id); err != nil {
		t.Fatalf("profile edit fenced accepted mail: %v", err)
	}
	d, err := r.holding.Get(ctx, receivingGateway, id)
	if err != nil || d.State != "archived" {
		t.Fatalf("delivery: state=%s error=%v", d.State, err)
	}
}

func TestNativeReceivingPartialQuotaFailureRetriesWithoutDuplicate(t *testing.T) {
	r, created := receivingFixture(t)
	ctx := context.Background()
	stores := make([]*mailbox.Store, 0, 2)
	for _, u := range created {
		a, _, err := r.life.NativeAssignment(u.NativeMailboxIssuer, u.SSOSub)
		if err != nil {
			t.Fatal(err)
		}
		store, err := mailbox.OpenExisting(filepath.Join(r.stateDir, "users", u.ID, "mailbox"), a.Owner, a.Limits, a.Source)
		if err != nil {
			t.Fatal(err)
		}
		stores = append(stores, store)
		t.Cleanup(func() { _ = store.Close() })
	}
	const id = "two-owner-partial-quota"
	for _, address := range []string{"one@example.test", "two@example.test"} {
		if err := r.bind(ctx, id, "", address); err != nil {
			t.Fatal(err)
		}
	}
	raw := []byte("From: test@outside.test\r\n\r\npartial commit must recover\r\n")
	if err := r.accept(ctx, id, "", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	// Accepted while both had room, the second owner then fills to its real
	// persisted payload quota before import. The first owner sorts first in
	// ingress.Import's deterministic commit order.
	var fillers []int64
	for _, size := range []int{5 << 20, 5 << 20, 5 << 20, 5 << 20, 5 << 20, 5 << 20, 2 << 20} {
		prefix := []byte("From: test@outside.test\r\nSubject: quota filler\r\n\r\n")
		payload := append(prefix, bytes.Repeat([]byte("x"), size-len(prefix))...)
		id, err := stores[1].Append(ctx, "INBOX", bytes.NewReader(payload), false)
		if err != nil {
			t.Fatal(err)
		}
		fillers = append(fillers, id)
	}
	if err := r.importDelivery(ctx, id); !errors.Is(err, mailbox.ErrCapacity) {
		t.Fatalf("second owner's full mailbox must defer acknowledgment: %v", err)
	}
	d, err := r.holding.Get(ctx, receivingGateway, id)
	if err != nil || d.State != "pending" || !bytes.Equal(d.Raw, raw) {
		t.Fatalf("partial quota failure lost holding bytes: %s %v", d.State, err)
	}
	first, err := stores[0].List(ctx, "INBOX", 0, 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("first owner did not commit before second owner failed: count=%d %v", len(first), err)
	}
	for _, filler := range fillers {
		if err := stores[1].Delete(ctx, "INBOX", filler); err != nil {
			t.Fatal(err)
		}
	}
	// Model the claim deadline passing, as the shared ingress crash checks do.
	// There is no production lease-reset or recovery bypass API.
	db, err := sql.Open("sqlite", filepath.Join(r.stateDir, "receiving", "ingress.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("UPDATE deliveries SET lease_until=0 WHERE gateway=? AND id=?", receivingGateway, id)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.importDelivery(ctx, id); err != nil {
		t.Fatal(err)
	}
	d, err = r.holding.Get(ctx, receivingGateway, id)
	if err != nil || d.State != "archived" || len(d.Raw) != 0 {
		t.Fatalf("recovered delivery did not acknowledge: %s %v", d.State, err)
	}
	for index, store := range stores {
		messages, err := store.List(ctx, "INBOX", 0, 10)
		if err != nil || len(messages) != 1 {
			t.Fatalf("owner %d received duplicate or missing message: count=%d %v", index, len(messages), err)
		}
		if index == 0 && messages[0].ID != first[0].ID {
			t.Fatal("retry replaced first owner's committed receipt")
		}
		got, err := store.Raw(ctx, "INBOX", messages[0].ID)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("owner %d lost accepted bytes: %v", index, err)
		}
	}
}
