package users

import (
	"context"
	"database/sql"
	"errors"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

func accountStores(t *testing.T) (*Store, *Store, *sql.DB) {
	t.Helper()
	dbbackend.RequirePostgres(t)
	dir := t.TempDir()
	cache, err := tenancy.Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	for _, org := range []string{"org-staff", "org-customer"} {
		if _, err = cache.Exec(`INSERT INTO orgs(id,status,revision,enabled,quotas_json,polled_at) VALUES(?,'active',1,1,'{}',0)`, org); err != nil {
			t.Fatal(err)
		}
	}
	var stores [2]*Store
	var first *sql.DB
	for i := range stores {
		db, err := volumes.Open(dir, i == 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		root := filepath.Join(t.TempDir(), "config")
		release, err := MountAccounts(root, dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
		stores[i], err = OpenExisting(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = db
		}
	}
	return stores[0], stores[1], first
}

func TestTableAccountsUnboundAndIssuerPairs(t *testing.T) {
	a, b, db := accountStores(t)
	for _, name := range []string{"local-one", "local-two"} {
		if _, err := a.CreateSSOUser(name, RoleUser, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	var unbound int
	if err := db.QueryRow(`SELECT count(*) FROM accounts WHERE issuer IS NULL AND subject IS NULL`).Scan(&unbound); err != nil || unbound != 2 {
		t.Fatal(unbound, err)
	}
	for _, issuer := range []string{"https://one.example", "https://two.example"} {
		name := "bound-one"
		if issuer == "https://two.example" {
			name = "bound-two"
		}
		u, err := a.CreateSSOUserIssuer(issuer, name, RoleUser, "same-subject", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		got, err := b.GetBySSOSubIssuer(issuer, "same-subject")
		if err != nil || got.ID != u.ID {
			t.Fatal(got, err)
		}
	}
	if _, err := b.GetBySSOSub("same-subject"); !errors.Is(err, ErrNativeAccountConflict) {
		t.Fatal("ambiguous issuer", err)
	}
	if _, err := b.GetBySSOSubIssuer("https://foreign.example", "same-subject"); !errors.Is(err, ErrNativeAccountConflict) {
		t.Fatal(err)
	}
	if _, err := b.CreateSSOUserIssuer("https://one.example", "third", RoleUser, "same-subject", "", "", ""); !errors.Is(err, ErrSSOSubTaken) {
		t.Fatal(err)
	}
	if _, err := b.CreateSSOUserIssuer("https://one.example", "LOCAL-ONE", RoleUser, "new-subject", "", "", ""); !errors.Is(err, ErrUsernameTaken) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(a.path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("table store created account files", err)
	}
}

func TestTableAccountsConcurrentDemotionAndUniqueness(t *testing.T) {
	a, b, _ := accountStores(t)
	first, err := a.CreateSSOUser("first-admin", RoleAdmin, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.CreateSSOUser("second-admin", RoleAdmin, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	result := make(chan error, 2)
	var wg sync.WaitGroup
	for i, s := range []*Store{a, b} {
		wg.Go(func() {
			<-start
			id := first.ID
			if i == 1 {
				id = second.ID
			}
			_, e := s.SetRole(id, RoleUser)
			result <- e
		})
	}
	close(start)
	wg.Wait()
	close(result)
	won, refused := 0, 0
	for e := range result {
		if e == nil {
			won++
		} else if errors.Is(e, ErrLastActiveAdmin) {
			refused++
		} else {
			t.Fatal(e)
		}
	}
	if won != 1 || refused != 1 {
		t.Fatal(won, refused)
	}
	start = make(chan struct{})
	result = make(chan error, 2)
	for i, s := range []*Store{a, b} {
		wg.Go(func() {
			<-start
			name := "unique-a"
			if i == 1 {
				name = "unique-b"
			}
			_, e := s.CreateSSOUserIssuer("https://issuer.example", name, RoleUser, "race-subject", "", "", "")
			result <- e
		})
	}
	close(start)
	wg.Wait()
	close(result)
	won, refused = 0, 0
	for e := range result {
		if e == nil {
			won++
		} else if errors.Is(e, ErrSSOSubTaken) {
			refused++
		} else {
			t.Fatal(e)
		}
	}
	if won != 1 || refused != 1 {
		t.Fatal(won, refused)
	}
}

func TestTableAccountSnapshotDoesNotBlockWriterAndReadsFresh(t *testing.T) {
	a, b, db := accountStores(t)
	u, err := a.CreateSSOUser("snapshot", RoleUser, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var initial int64
	if err = db.QueryRow(`SELECT revision FROM accounts WHERE id=?`, u.ID).Scan(&initial); err != nil {
		t.Fatal(err)
	}
	err = a.WithCurrentUsers(context.Background(), func(all []User) error {
		done := make(chan error, 1)
		go func() { _, err := b.Deactivate(u.ID); done <- err }()
		select {
		case err := <-done:
			if err != nil {
				return err
			}
		case <-time.After(3 * time.Second):
			t.Fatal("snapshot blocked writer")
		}
		if len(all) != 1 || !all[0].Active {
			t.Fatal("snapshot changed", all)
		}
		all[0].Username = "mutated snapshot"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Get(u.ID)
	if err != nil || got.Active || got.Username != "snapshot" {
		t.Fatal(got, err)
	}
	var revision int64
	if err = db.QueryRow(`SELECT revision FROM accounts WHERE id=?`, u.ID).Scan(&revision); err != nil || revision <= initial {
		t.Fatal(revision, err)
	}
}

func TestTableAccountRoutingCorruptionFailsClosed(t *testing.T) {
	a, b, db := accountStores(t)
	u, err := a.CreateSSOUser("original", RoleUser, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE accounts SET username='corrupt' WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Get(u.ID); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatal("corrupt record became an account or missing", err)
	}
	if _, err := LoadOrMigrate(context.Background(), filepath.Dir(a.path), ""); err == nil {
		t.Fatal("startup accepted corrupt account routing")
	}

}

func TestTableAccountRestoreCommitsBeforeFinalizerAndKeepsFences(t *testing.T) {
	a, b, _ := accountStores(t)
	if _, err := a.CreateSSOUser("recovery-admin", RoleAdmin, "", "", ""); err != nil {
		t.Fatal(err)
	}
	target, err := a.CreateSSOUser("release-target", RoleUser, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	held := false
	called := 0
	failed := errors.New("external finalizer failed")
	err = a.DeactivateForRestoreRelease(context.Background(), func(current []User) ([]string, []string, func(), error) {
		held = true
		return []string{target.ID}, nil, func() { held = false }, nil
	}, func() error {
		called++
		if !held {
			t.Fatal("commit fence released early")
		}
		got, err := b.Get(target.ID)
		if err != nil || got.Active {
			t.Fatal("finalizer ran before account commit", got, err)
		}
		return failed
	})
	if !errors.Is(err, failed) || called != 1 || held {
		t.Fatal(err, called, held)
	}
	got, err := b.Get(target.ID)
	if err != nil || got.Active {
		t.Fatal("failed finalizer rolled back deactivation", got, err)
	}
}

func TestTableAccountSerializationRetryBoundPreservesAccount(t *testing.T) {
	a, _, db := accountStores(t)
	u, err := a.CreateSSOUser("contended", RoleUser, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE SEQUENCE retry_attempts`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE FUNCTION refuse_account_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM nextval('retry_attempts'); RAISE EXCEPTION 'forced serialization conflict' USING ERRCODE='40001'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TRIGGER refuse_update BEFORE UPDATE ON accounts FOR EACH ROW EXECUTE FUNCTION refuse_account_update()`); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Deactivate(u.ID); !errors.Is(err, ErrKDFBusy) {
		t.Fatal("retry bound did not answer busy", err)
	}
	var attempts int
	if err = db.QueryRow(`SELECT last_value FROM retry_attempts`).Scan(&attempts); err != nil || attempts != 5 {
		t.Fatal(attempts, err)
	}
	got, err := a.Get(u.ID)
	if err != nil || !got.Active {
		t.Fatal("failed attempts changed the account", got, err)
	}
}

func TestTableAccountRecoveryIntentPreservesCredentialsAndCommitFence(t *testing.T) {
	a, b, db := accountStores(t)
	if _, err := a.CreateSSOUser("recovery-owner", RoleAdmin, "", "", ""); err != nil {
		t.Fatal(err)
	}
	before := recoveryNativeOwner(t, a, "native-repair", "native-subject")
	table, err := vol.NewTable(db, "state", vol.TableOptions{MasterKey: make([]byte, 32), Org: func(context.Context, string) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	release, err := vol.Mount(root, table)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	held := false
	err = a.RepairNativeAccounts(context.Background(), before.NativeMailboxIssuer, []NativeAccountRepair{{before.ID, before.SSOSub, before.NativeMailboxSource, false, RoleUser}}, func(current, repaired []User) (func(), error) {
		persisted, err := b.Get(before.ID)
		if err != nil || !persisted.Active {
			return nil, errors.New("repair preceded its intent")
		}
		if err = vol.OpenDocument(filepath.Join(root, "native-recovery-repair.json")).Replace([]byte(`{"intent":"recorded"}`)); err != nil {
			return nil, err
		}
		held = true
		return func() {
			persisted, err := b.Get(before.ID)
			if err != nil || persisted.Active {
				t.Error("commit fence released before account commit", err)
			}
			held = false
		}, nil
	})
	if err != nil || held {
		t.Fatal(err, held)
	}
	after, err := b.Get(before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Active || after.Role != RoleUser || after.NativeSendEpoch != before.NativeSendEpoch+1 {
		t.Fatal("wrong repaired authority", after)
	}
	before.Active, before.Role, before.UpdatedAt, before.DeactivatedAt, before.NativeSendEpoch = after.Active, after.Role, after.UpdatedAt, after.DeactivatedAt, after.NativeSendEpoch
	if !reflect.DeepEqual(before, after) {
		t.Fatal("repair changed retained credentials or key material")
	}
}

func TestTableAccountOrgAndStaffInvariants(t *testing.T) {
	a, b, _ := accountStores(t)
	a.StaffOrg = "org-staff"
	u, err := a.CreateSSOUserIssuer("https://issuer.example", "customer", RoleUser, "customer-subject", "", "", "org-customer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetRole(u.ID, RoleAdmin); !errors.Is(err, ErrCustomerAdmin) {
		t.Fatal("customer gained admin", err)
	}
	a.mu.Lock()
	err = a.withWriteLock(context.Background(), func() error {
		f, err := a.readFileUnlocked()
		if err != nil {
			return err
		}
		f.Users[0].OrgID = "other-org"
		return a.writeFileUnlocked(f)
	})
	a.mu.Unlock()
	if !errors.Is(err, ErrOrgIDWriteOnce) {
		t.Fatal("org binding changed", err)
	}
	got, err := b.Get(u.ID)
	if err != nil || got.OrgID != u.OrgID || got.Role != RoleUser {
		t.Fatal(got, err)
	}
}

func TestTableAccountCannotPublishAfterDeletedOrg(t *testing.T) {
	a, _, db := accountStores(t)
	if _, err := db.Exec(`UPDATE ` + a.orgTable + ` SET status='deleted' WHERE id='org-customer'`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateSSOUserIssuer("https://issuer.example", "late", RoleUser, "late", "", "", "org-customer"); !errors.Is(err, ErrNativeAccountConflict) {
		t.Fatal("post-delete account published", err)
	}
	all, err := a.List()
	if err != nil || len(all) != 0 {
		t.Fatal(all, err)
	}
}
