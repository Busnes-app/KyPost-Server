//go:build linux

package sso

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

// MH7: end-to-end table-mode native account allocation commits the accounts row before
// writing native-mailbox.json into the documents table (so vol.Table locks accounts
// FOR KEY SHARE) and leaves both config and state directories empty on the host's disk.
func TestTableAllocateNativeAccountWritesNothingToHostDisk(t *testing.T) {
	dbbackend.RequirePostgres(t)
	configDir, stateDir := t.TempDir(), t.TempDir()
	t.Setenv("CONFIG_DIR", configDir)
	t.Setenv("STATE_DIR", stateDir)
	dbbackend.FreshDatabaseForTest(t)

	st, err := state.New(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	docDB, err := volumes.Open(stateDir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer docDB.Close()
	unmountAccounts, err := users.MountAccounts(configDir, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unmountAccounts()

	accounts, err := users.OpenExisting(context.Background(), configDir)
	if err != nil {
		t.Fatal(err)
	}
	master := bytes.Repeat([]byte{9}, 32)
	orgLookup := func(_ context.Context, owner string) (string, error) {
		u, err := accounts.Get(owner)
		if err != nil {
			return "", err
		}
		return u.OrgID, nil
	}
	fence := state.VolFence(stateDir)
	for _, m := range []struct {
		dir  string
		root string
	}{
		{configDir, "config"},
		{stateDir, "state"},
	} {
		tbl, err := vol.NewTable(docDB, m.root, vol.TableOptions{MasterKey: master, Org: orgLookup})
		if err != nil {
			t.Fatal(err)
		}
		release, err := vol.MountFenced(m.dir, tbl, fence)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}

	life := NewLifecycleStore(configDir)
	domains := provenNativeDomain(t, configDir)
	nativeDesired(t, life, "one", "one@example.test", 1, true)

	u, err := life.AllocateNativeAccount(context.Background(), stateDir, nativeIssuer, "one", domains, accounts, nativeLimits, "")
	if err != nil || u.NativeMailboxSource == "" {
		t.Fatalf("AllocateNativeAccount in table mode: user=%+v err=%v", u, err)
	}
	a, ok, err := life.NativeAssignment(nativeIssuer, "one")
	if err != nil || !ok || a.Status != "applied" || a.Owner.Mailbox != u.ID || a.Source != u.NativeMailboxSource {
		t.Fatalf("unexpected assignment: ok=%v a=%+v err=%v", ok, a, err)
	}
	if got, err := mailbox.ValidatePreparedAccount(stateDir, a.Owner, a.Address, nativeLimits); err != nil || got != a.Source {
		t.Fatalf("ValidatePreparedAccount: got=%q want=%q err=%v", got, a.Source, err)
	}
	retry, err := life.AllocateNativeAccount(context.Background(), stateDir, nativeIssuer, "one", domains, accounts, nativeLimits, "")
	if err != nil || retry.ID != u.ID || retry.NativeMailboxSource != u.NativeMailboxSource {
		t.Fatalf("retry AllocateNativeAccount: got=%+v want=%+v err=%v", retry, u, err)
	}
	for name, dir := range map[string]string{"config": configDir, "state": stateDir} {
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Fatalf("table-mode AllocateNativeAccount touched host %s disk: entries=%v err=%v", name, entries, err)
		}
	}
}
