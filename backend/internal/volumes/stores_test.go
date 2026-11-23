package volumes_test

import (
	"context"
	"fmt"
	"github.com/Busnes-app/kypost-server/backend/internal/kycalendar"
	"github.com/Busnes-app/kypost-server/backend/internal/mailcache"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpdiscovery"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/contacts"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/groups"
	"github.com/Busnes-app/kypost-server/backend/internal/rules"
	"github.com/Busnes-app/kypost-server/backend/internal/sendas"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

func TestDocumentStoresShareChangesAcrossPrivateRoots(t *testing.T) {
	dbbackend.RequirePostgres(t)
	for _, mode := range []string{"multi", "migrated-single"} {
		t.Run(mode, func(t *testing.T) {
			db, err := volumes.Open(t.TempDir(), true)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			options := vol.TableOptions{MasterKey: make([]byte, 32), Org: func(_ context.Context, owner string) (string, error) {
				if mode == "migrated-single" {
					return "", nil
				}
				return "org-" + owner, nil
			}}
			roots := []string{filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "state")}
			dirs := make([]string, 2)
			for i, root := range roots {
				table, err := vol.NewTable(db, "state", options)
				if err != nil {
					t.Fatal(err)
				}
				release, err := vol.Mount(root, table)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				dirs[i] = filepath.Join(root, "users", "member-a")
			}
			a, err := contacts.New(dirs[0])
			if err != nil {
				t.Fatal(err)
			}
			b, err := contacts.New(dirs[1])
			if err != nil {
				t.Fatal(err)
			}
			c, err := a.Upsert(contacts.Contact{FormattedName: "Shared Contact"})
			if err != nil {
				t.Fatal(err)
			}
			if got, ok, err := b.Get(c.UID); err != nil || !ok || got.FormattedName != c.FormattedName {
				t.Fatal("contact not shared", ok, err)
			}
			if _, err := b.Delete(c.UID); err != nil {
				t.Fatal(err)
			}
			if got, ok, err := a.Get(c.UID); err != nil || !ok || !got.Deleted {
				t.Fatal("contact tombstone not shared", ok, err)
			}
			ga, err := groups.New(dirs[0])
			if err != nil {
				t.Fatal(err)
			}
			gb, err := groups.New(dirs[1])
			if err != nil {
				t.Fatal(err)
			}
			g, err := ga.Upsert(groups.Group{Name: "Shared Group"})
			if err != nil {
				t.Fatal(err)
			}
			if _, ok, err := gb.Get(g.ID); err != nil || !ok {
				t.Fatal("group not shared", err)
			}
			ra, err := rules.New(dirs[0])
			if err != nil {
				t.Fatal(err)
			}
			rb, err := rules.New(dirs[1])
			if err != nil {
				t.Fatal(err)
			}
			r, err := ra.Upsert(rules.Rule{Name: "Shared Rule", Match: rules.MatchGroup{Op: "allof"}, Actions: []rules.Action{{Type: "read"}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, ok, err := rb.Get(r.ID); err != nil || !ok {
				t.Fatal("rule not shared", err)
			}
			sa, err := sendas.New(dirs[0])
			if err != nil {
				t.Fatal(err)
			}
			sb, err := sendas.New(dirs[1])
			if err != nil {
				t.Fatal(err)
			}
			alias, err := sa.Create("member-a", "alias@example.test", "Alias")
			if err != nil {
				t.Fatal(err)
			}
			if _, ok, err := sb.Get(alias.ID); err != nil || !ok {
				t.Fatal("send-as record not shared", err)
			}
			var wg sync.WaitGroup
			for i := range 4 {
				wg.Go(func() {
					store := a
					if i%2 != 0 {
						store = b
					}
					_, err := store.Upsert(contacts.Contact{FormattedName: fmt.Sprintf("Concurrent %d", i)})
					if err != nil {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			list, err := a.List()
			if err != nil || len(list) != 4 {
				t.Fatal("lost contact mutation", len(list), err)
			}
			other, err := contacts.New(filepath.Join(roots[0], "users", "member-b"))
			if err != nil {
				t.Fatal(err)
			}
			if list, err := other.List(); err != nil || len(list) != 0 {
				t.Fatal("other user saw contacts", len(list), err)
			}
			if _, err := db.Exec("DELETE FROM documents WHERE path='users/member-a/groups.json'"); err != nil {
				t.Fatal(err)
			}
			if list, err := ga.List(); err != nil || len(list) != 0 {
				t.Fatal("served a removed cached group", len(list), err)
			}
			for _, root := range roots {
				if _, err := os.Stat(root); !os.IsNotExist(err) {
					t.Fatal("table store wrote to private root", root, err)
				}
			}
		})
	}
}

func TestSmallDocumentAdaptersOnBothVolumes(t *testing.T) {
	for _, kind := range []string{"files", "tables"} {
		t.Run(kind, func(t *testing.T) {
			dirs := []string{filepath.Join(t.TempDir(), "users", "member"), ""}
			dirs[1] = dirs[0]
			if kind == "tables" {
				dbbackend.RequirePostgres(t)
				db, err := volumes.Open(t.TempDir(), true)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				for i := range 2 {
					root := filepath.Join(t.TempDir(), "state")
					table, err := vol.NewTable(db, "state", vol.TableOptions{MasterKey: make([]byte, 32), Org: func(context.Context, string) (string, error) { return "", nil }})
					if err != nil {
						t.Fatal(err)
					}
					release, err := vol.Mount(root, table)
					if err != nil {
						t.Fatal(err)
					}
					defer release()
					dirs[i] = filepath.Join(root, "users", "member")
				}
			}
			for _, dir := range dirs {
				if err := vol.OpenDocument(filepath.Join(dir, "pgp-discovery.json")).Mkdir(0700); err != nil {
					t.Fatal(err)
				}
			}
			start := make(chan struct{})
			result := make(chan error, 2)
			go func() {
				<-start
				_, err := pgpdiscovery.Update(dirs[0], func(s pgpdiscovery.Settings) pgpdiscovery.Settings { s.StoreDiscoveredKeys = false; return s })
				result <- err
			}()
			go func() {
				<-start
				_, err := pgpdiscovery.Update(dirs[1], func(s pgpdiscovery.Settings) pgpdiscovery.Settings { s.AdvertiseAutocrypt = false; return s })
				result <- err
			}()
			close(start)
			for range 2 {
				if err := <-result; err != nil {
					t.Fatal(err)
				}
			}
			if s, err := pgpdiscovery.Load(dirs[0]); err != nil || s.StoreDiscoveredKeys || s.AdvertiseAutocrypt {
				t.Fatal("lost a PGP preference update", s, err)
			}
			if err := pgpdiscovery.AddSuppression(dirs[0], "refused@example.test", pgpdiscovery.ReasonExplicit); err != nil {
				t.Fatal(err)
			}
			if list, err := pgpdiscovery.LoadSuppressions(dirs[1]); err != nil || len(list) != 1 {
				t.Fatal("suppression not shared", list, err)
			}
			if removed, err := pgpdiscovery.RemoveSuppression(dirs[1], "refused@example.test"); err != nil || !removed {
				t.Fatal(removed, err)
			}
			if list, err := pgpdiscovery.LoadSuppressions(dirs[0]); err != nil || len(list) != 0 {
				t.Fatal("suppression removal not shared", list, err)
			}
			if err := kycalendar.OpenInvitations(dirs[0]).Record("uid-1", "organizer@example.test"); err != nil {
				t.Fatal(err)
			}
			if has, err := kycalendar.OpenInvitations(dirs[1]).Has("uid-1", "organizer@example.test"); err != nil || !has {
				t.Fatal("invitation not shared", has, err)
			}
			ref, err := kycalendar.OpenFlags(dirs[0]).Put(kycalendar.Flag{Folder: "INBOX", Message: "1"})
			if err != nil {
				t.Fatal(err)
			}
			if flag, ok, err := kycalendar.OpenFlags(dirs[1]).Get(ref); err != nil || !ok || flag.Cleared {
				t.Fatal("flag not shared", ok, err)
			}
			if _, err := kycalendar.OpenFlags(dirs[1]).Clear("", "INBOX", "1"); err != nil {
				t.Fatal(err)
			}
			if flag, ok, err := kycalendar.OpenFlags(dirs[0]).Get(ref); err != nil || !ok || !flag.Cleared {
				t.Fatal("flag clear not shared", ok, err)
			}
			a, err := mailcache.New(dirs[0])
			if err != nil {
				t.Fatal(err)
			}
			b, err := mailcache.New(dirs[1])
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Upsert("INBOX", []mailcache.Entry{{UID: 1, Body: "private warm body"}}); err != nil {
				t.Fatal(err)
			}
			if list, _, err := b.Snapshot("INBOX", 10); err != nil || len(list) != 1 || list[0].Body != "private warm body" {
				t.Fatal("warm cache not shared", err)
			}
			if err := b.OmitBodies(); err != nil {
				t.Fatal(err)
			}
			if err := a.Upsert("INBOX", []mailcache.Entry{{UID: 1, Body: "must stay omitted"}}); err != nil {
				t.Fatal(err)
			}
			if list, _, err := a.Snapshot("INBOX", 10); err != nil || len(list) != 1 || list[0].Body != "" {
				t.Fatal("privacy policy lost across writers", err)
			}
		})
	}
}
