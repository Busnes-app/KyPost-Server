//go:build linux

package sso

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func aliasJob(job mailbox.OutboundJob) mailbox.OutboundJob {
	job.From = "sales@example.test"
	job.Deliveries = []mailbox.OutboundDelivery{{Recipients: []string{"recipient@example.test"}, Raw: []byte("From: sales@example.test\r\nTo: recipient@example.test\r\nSubject: from an alias\r\n\r\nmessage\r\n")}}
	return job
}

// A job queued from an alias records the alias generation. Once it is
// retryable, releasing the alias (or releasing and reassigning it) ends it
// stale on the next worker run, before any network attempt.
func TestNativeOutboundAliasFromIsFencedByGeneration(t *testing.T) {
	for _, mode := range []string{"released", "reassigned", "reassigned-back"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			sender, u, job := outboundFixture(t)
			life := NewLifecycleStore(sender.ConfigDir)
			if _, err := life.AddNativeAlias(ctx, sender.StateRoot, u.ID, "sales@example.test"); err != nil {
				t.Fatal(err)
			}
			id, err := fsutil.NewUUIDv4()
			if err != nil {
				t.Fatal(err)
			}
			if err = sender.Queue(ctx, u.ID, id, aliasJob(job)); err != nil {
				t.Fatal("alias From refused", err)
			}
			box, key, err := sender.openStorage(u.ID)
			if err != nil {
				t.Fatal(err)
			}
			stored, _, _, err := box.ReadOutbound(ctx, key, id)
			_ = box.Close()
			if err != nil || stored.From != "sales@example.test" || stored.FromGeneration != 1 {
				t.Fatal("From generation not recorded", stored.From, stored.FromGeneration, err)
			}
			// As if a first attempt was definitely refused and is now due.
			db, err := sql.Open("sqlite", filepath.Join(sender.StateRoot, "users", u.ID, "mailbox", "mailbox.db"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.Exec("UPDATE outbox_deliveries SET state='retryable',attempts=1,claim=?,next_attempt=0 WHERE job=?", id, id)
			_ = db.Close()
			if err != nil {
				t.Fatal(err)
			}
			if _, err = life.ReleaseNativeAlias(ctx, sender.StateRoot, "sales@example.test"); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "reassigned":
				nativeDesired(t, life, "two", "two@example.test", 1, true)
				two, err := life.AllocateNativeAccount(ctx, sender.StateRoot, nativeIssuer, "two", sender.Domains, sender.Accounts, nativeLimits)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = life.ReassignNativeAddress(ctx, sender.StateRoot, "sales@example.test", two.ID); err != nil {
					t.Fatal(err)
				}
			case "reassigned-back":
				if _, err = life.ReassignNativeAddress(ctx, sender.StateRoot, "sales@example.test", u.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err = sender.Recover(ctx, u.ID, id); !errors.Is(err, ErrNativeOutboundStale) {
				t.Fatal("stale alias job not refused", err)
			}
			statuses, _, err := sender.Status(ctx, u.ID, id)
			if err != nil || len(statuses) != 1 || statuses[0].State != "quarantined" || statuses[0].Attempts != 1 {
				t.Fatal("stale alias job was not quarantined untouched", statuses, err)
			}
		})
	}
}

func TestNativeOutboundQueueRefusesUnownedFrom(t *testing.T) {
	ctx := context.Background()
	sender, u, job := outboundFixture(t)
	id, err := fsutil.NewUUIDv4()
	if err != nil {
		t.Fatal(err)
	}
	if err = sender.Queue(ctx, u.ID, id, aliasJob(job)); !errors.Is(err, ErrNativeOutboundStale) {
		t.Fatal("From outside the ledger queued", err)
	}
	if _, err = NewLifecycleStore(sender.ConfigDir).AddNativeAlias(ctx, sender.StateRoot, u.ID, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	if err = sender.Queue(ctx, u.ID, id, aliasJob(job)); err != nil {
		t.Fatal("owned active alias refused", err)
	}
}

// Jobs queued before generations carry 0: only the primary passes, still
// fenced on the directory revision; an alias needs its exact generation.
func TestNativeOutboundLegacyJobsKeepPrimaryOnly(t *testing.T) {
	u := users.User{ID: "local-one"}
	a := NativeAssignment{Owner: mailbox.Owner{Mailbox: "local-one"}, Address: "one@example.test"}
	d := DirectoryState{Revision: 3}
	relay := mailmsg.DomainRelay{Generation: "relay"}
	for _, c := range []struct {
		name       string
		from       NativeAddress
		job        mailbox.OutboundJob
		admissible bool
	}{
		{"legacy-primary", NativeAddress{Address: "one@example.test", Mailbox: "local-one", State: "active", Generation: 5}, mailbox.OutboundJob{From: "one@example.test"}, true},
		{"legacy-alias", NativeAddress{Address: "sales@example.test", Mailbox: "local-one", State: "active", Generation: 1}, mailbox.OutboundJob{From: "sales@example.test"}, false},
		{"current-alias", NativeAddress{Address: "sales@example.test", Mailbox: "local-one", State: "active", Generation: 2}, mailbox.OutboundJob{From: "sales@example.test", FromGeneration: 2}, true},
		{"older-alias-generation", NativeAddress{Address: "sales@example.test", Mailbox: "local-one", State: "active", Generation: 3}, mailbox.OutboundJob{From: "sales@example.test", FromGeneration: 2}, false},
		{"disabled", NativeAddress{Address: "sales@example.test", Mailbox: "local-one", State: "disabled", Generation: 2}, mailbox.OutboundJob{From: "sales@example.test", FromGeneration: 2}, false},
		{"other-mailbox", NativeAddress{Address: "sales@example.test", Mailbox: "local-two", State: "active", Generation: 2}, mailbox.OutboundJob{From: "sales@example.test", FromGeneration: 2}, false},
		{"stale-directory", NativeAddress{Address: "one@example.test", Mailbox: "local-one", State: "active", Generation: 5}, mailbox.OutboundJob{From: "one@example.test", DirectoryRevision: 2}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			job := c.job
			job.RelayGeneration = relay.Generation
			if !strings.HasPrefix(c.name, "stale") {
				job.DirectoryRevision = d.Revision
			}
			called := false
			err := (NativeOutbound{}).withJobAuthority(context.Background(), u, a, c.from, d, relay, job, nil, func(context.Context) error { called = true; return nil })
			if called != c.admissible || (err == nil) != c.admissible || !c.admissible && !errors.Is(err, ErrNativeOutboundStale) {
				t.Fatal(c.name, called, err)
			}
		})
	}
}
