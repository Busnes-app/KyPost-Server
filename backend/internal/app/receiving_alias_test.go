//go:build linux

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func receivingDirectory(t *testing.T, r *receivingRuntime, subject string, revision int, active bool) {
	t.Helper()
	raw := []byte(fmt.Sprintf(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":%q,"externalId":%q,"userName":%q,"active":%t,"emails":[{"value":%q,"primary":true}],"meta":{"version":"W/\"%d\""}}`, subject, subject, subject, active, subject+"@example.test", revision))
	var resource sso.DirectoryUser
	if err := json.Unmarshal(raw, &resource); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: fmt.Sprintf("%s-%d", subject, revision), Type: "user.updated", At: time.Now()}
	if _, err := r.life.ApplyDirectoryUser("https://identity.example.test", ev, resource, sso.EventDigest(ev.Type, raw), func() (bool, error) { return !active, nil }); err != nil {
		t.Fatal(err)
	}
}

func receivingInbox(t *testing.T, r *receivingRuntime, u users.User) []string {
	t.Helper()
	a, _, err := r.life.NativeAssignment(u.NativeMailboxIssuer, u.SSOSub)
	if err != nil {
		t.Fatal(err)
	}
	s, err := mailbox.OpenExisting(filepath.Join(r.stateDir, "users", u.ID, "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.List(context.Background(), "INBOX", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	subjects := []string{}
	for _, row := range rows {
		raw, err := s.Raw(context.Background(), "INBOX", row.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, body, _ := strings.Cut(string(raw), "\r\n\r\n")
		subjects = append(subjects, strings.TrimSpace(body))
	}
	return subjects
}

// receive binds every recipient, accepts and leaves the import to the caller.
func receive(t *testing.T, r *receivingRuntime, id string, recipients ...string) {
	t.Helper()
	ctx := context.Background()
	for _, recipient := range recipients {
		if err := r.bind(ctx, id, "", recipient); err != nil {
			t.Fatal(id, recipient, err)
		}
	}
	if err := r.accept(ctx, id, "", strings.NewReader("From: sender@outside.test\r\n\r\n"+id+"\r\n")); err != nil {
		t.Fatal(id, err)
	}
}

func receivingRoute(t *testing.T, r *receivingRuntime, address string) (mailboxID string, generation int64, active bool) {
	t.Helper()
	path, err := filepath.Abs(filepath.Join(r.stateDir, "receiving", "ingress.db"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow("SELECT mailbox,generation,active FROM routes WHERE address=?", address).Scan(&mailboxID, &generation, &active); err != nil {
		t.Fatal(err)
	}
	return mailboxID, generation, active
}

func TestNativeReceivingAliasLandsOnceInOwningMailbox(t *testing.T) {
	r, created := receivingFixture(t)
	ctx := context.Background()
	if _, err := r.life.AddNativeAlias(ctx, r.stateDir, created[0].ID, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	receive(t, r, "to-primary-and-alias", "one@example.test", "sales@example.test")
	receive(t, r, "to-alias", "sales@example.test")
	for _, id := range []string{"to-primary-and-alias", "to-alias"} {
		if err := r.importDelivery(ctx, id); err != nil {
			t.Fatal(id, err)
		}
	}
	if got := receivingInbox(t, r, created[0]); strings.Join(got, ",") != "to-primary-and-alias,to-alias" && strings.Join(got, ",") != "to-alias,to-primary-and-alias" {
		t.Fatal("alias mail not delivered exactly once to the owner", got)
	}
	if got := receivingInbox(t, r, created[1]); len(got) != 0 {
		t.Fatal("alias mail reached another mailbox", got)
	}
}

// Released then reassigned: mail frozen against the old generation never
// reaches the new mailbox, and reassigning back to the original mailbox gets a
// new generation that older frozen mail does not match either.
func TestNativeReceivingReleaseReassignNeverDeliversOldGeneration(t *testing.T) {
	r, created := receivingFixture(t)
	ctx := context.Background()
	one, two := created[0], created[1]
	if _, err := r.life.AddNativeAlias(ctx, r.stateDir, one.ID, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	receive(t, r, "frozen-at-one", "sales@example.test")
	released, err := r.life.ReleaseNativeAlias(ctx, r.stateDir, "sales@example.test")
	if err != nil || released.State != "reserved" || released.Generation != 2 {
		t.Fatal("release", released, err)
	}
	if owner, generation, active := receivingRoute(t, r, "sales@example.test"); active || generation != 2 || owner != one.ID {
		t.Fatal("release left the route active", owner, generation, active)
	}
	if err := r.bind(ctx, "after-release", "", "sales@example.test"); !errors.Is(err, ingress.ErrRoute) {
		t.Fatal("reserved alias still routes", err)
	}
	if moved, err := r.life.ReassignNativeAddress(ctx, r.stateDir, "sales@example.test", two.ID); err != nil || moved.Generation != 3 || moved.Mailbox != two.ID || moved.State != "active" {
		t.Fatal("reassign", moved, err)
	}
	if err := r.importDelivery(ctx, "frozen-at-one"); !errors.Is(err, ingress.ErrRoute) {
		t.Fatal("old-generation mail was not fenced", err)
	}
	if d, err := r.holding.Get(ctx, receivingGateway, "frozen-at-one"); err != nil || d.State != "quarantined" {
		t.Fatal("old-generation mail not quarantined", d.State, err)
	}
	receive(t, r, "for-two", "sales@example.test")
	if err := r.importDelivery(ctx, "for-two"); err != nil {
		t.Fatal(err)
	}
	receive(t, r, "frozen-at-two", "sales@example.test")
	if _, err := r.life.ReleaseNativeAlias(ctx, r.stateDir, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	if back, err := r.life.ReassignNativeAddress(ctx, r.stateDir, "sales@example.test", one.ID); err != nil || back.Generation != 5 || back.Mailbox != one.ID {
		t.Fatal("reassign back", back, err)
	}
	if err := r.importDelivery(ctx, "frozen-at-two"); !errors.Is(err, ingress.ErrRoute) {
		t.Fatal("mail frozen for the second owner followed the address back", err)
	}
	receive(t, r, "back-at-one", "sales@example.test")
	if err := r.importDelivery(ctx, "back-at-one"); err != nil {
		t.Fatal(err)
	}
	if got := receivingInbox(t, r, one); strings.Join(got, ",") != "back-at-one" {
		t.Fatal("first owner inbox", got)
	}
	if got := receivingInbox(t, r, two); strings.Join(got, ",") != "for-two" {
		t.Fatal("second owner inbox", got)
	}
}

// Same mailbox, newer generation: an address released and reassigned to its
// own mailbox still fences mail frozen before.
func TestNativeReceivingReassignToSameMailboxFencesFrozenMail(t *testing.T) {
	r, created := receivingFixture(t)
	ctx := context.Background()
	if _, err := r.life.AddNativeAlias(ctx, r.stateDir, created[0].ID, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	receive(t, r, "frozen-gen-1", "sales@example.test")
	if _, err := r.life.ReleaseNativeAlias(ctx, r.stateDir, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	if back, err := r.life.ReassignNativeAddress(ctx, r.stateDir, "sales@example.test", created[0].ID); err != nil || back.Generation != 3 {
		t.Fatal(back, err)
	}
	if err := r.importDelivery(ctx, "frozen-gen-1"); !errors.Is(err, ingress.ErrRoute) {
		t.Fatal("older frozen mail matched the re-assigned address", err)
	}
	if got := receivingInbox(t, r, created[0]); len(got) != 0 {
		t.Fatal(got)
	}
}

// Deactivation and reactivation both land inside ApplyDirectory, so mail
// accepted before is fenced even when no worker ran in between.
func TestNativeReceivingDeactivateReactivateFencesAcceptedMail(t *testing.T) {
	r, _ := receivingFixture(t)
	ctx := context.Background()
	receive(t, r, "accepted-before-deactivation", "one@example.test")
	receivingDirectory(t, r, "one", 2, false)
	if _, generation, active := receivingRoute(t, r, "one@example.test"); active || generation != 2 {
		t.Fatal("deactivation left the route active", generation, active)
	}
	receivingDirectory(t, r, "one", 3, true)
	addresses, err := r.life.NativeAddresses()
	if err != nil || addresses["one@example.test"].Generation != 3 || addresses["one@example.test"].State != "active" {
		t.Fatal("reactivation", addresses, err)
	}
	if err := r.importDelivery(ctx, "accepted-before-deactivation"); !errors.Is(err, ingress.ErrRoute) {
		t.Fatal("mail accepted before deactivation was not fenced", err)
	}
	receive(t, r, "after-reactivation", "one@example.test")
	if err := r.importDelivery(ctx, "after-reactivation"); err != nil {
		t.Fatal(err)
	}
}

func receivingMailbox(t *testing.T, r *receivingRuntime, mailboxID string) []string {
	t.Helper()
	a, ok, err := r.life.NativeMailboxAssignment(mailboxID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	s, err := mailbox.OpenExisting(filepath.Join(a.Dir(r.stateDir), "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.List(context.Background(), "INBOX", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	subjects := []string{}
	for _, row := range rows {
		raw, err := s.Raw(context.Background(), "INBOX", row.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, body, _ := strings.Cut(string(raw), "\r\n\r\n")
		subjects = append(subjects, strings.TrimSpace(body))
	}
	return subjects
}

// Mail to an extra mailbox's address lands there, not in the owner's primary;
// a delivery to both mailboxes produces one copy in each.
func TestNativeReceivingExtraMailbox(t *testing.T) {
	r, created := receivingFixture(t)
	ctx := context.Background()
	m, err := r.life.CreateNativeMailbox(ctx, r.stateDir, created[0].ID, "sales@example.test")
	if err != nil {
		t.Fatal(err)
	}
	receive(t, r, "to-extra", "sales@example.test")
	receive(t, r, "to-both", "one@example.test", "sales@example.test")
	for _, id := range []string{"to-extra", "to-both"} {
		if err := r.importDelivery(ctx, id); err != nil {
			t.Fatal(id, err)
		}
	}
	if got := strings.Join(receivingMailbox(t, r, m.ID), ","); got != "to-extra,to-both" && got != "to-both,to-extra" {
		t.Fatal("extra mailbox mail", got)
	}
	if got := receivingInbox(t, r, created[0]); len(got) != 1 || got[0] != "to-both" {
		t.Fatal("primary received extra mailbox mail", got)
	}
	if mailboxID, _, active := receivingRoute(t, r, "sales@example.test"); mailboxID != m.ID || !active {
		t.Fatal("route", mailboxID, active)
	}
	// A disabled mailbox receives nothing new.
	if _, err := r.life.SetNativeMailboxState(ctx, r.stateDir, m.ID, false); err != nil {
		t.Fatal(err)
	}
	// Refused permanently (550 5.1.1), not as a storage failure senders retry.
	if err := r.bind(ctx, "after-disable", "", "sales@example.test"); receivingExit(err) != 3 {
		t.Fatal("disabled mailbox not refused as an unknown recipient:", err)
	}
}

// A creation interrupted before its storage was published leaves an active,
// unprepared mailbox: outbox discovery skips it rather than failing on it.
func TestNativeOutboxSkipsUnpreparedMailbox(t *testing.T) {
	r, created := receivingFixture(t)
	m, err := r.life.CreateNativeMailbox(context.Background(), r.stateDir, created[0].ID, "sales@example.test")
	if err != nil {
		t.Fatal(err)
	}
	all, err := r.accounts.List()
	if err != nil {
		t.Fatal(err)
	}
	mailboxes, err := r.life.NativeMailboxes()
	if err != nil {
		t.Fatal(err)
	}
	for i := range mailboxes {
		if mailboxes[i].ID == m.ID {
			if !mailboxes[i].Prepared {
				t.Fatal("prepared mailbox reported unprepared")
			}
			mailboxes[i].Prepared = false
		}
	}
	ids := outboxMailboxes(all, mailboxes)
	if len(ids) != len(created) || slices.Contains(ids, m.ID) {
		t.Fatal("outbox discovery", ids)
	}
}
