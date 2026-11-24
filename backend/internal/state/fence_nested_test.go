package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

// fencePID is the backend holding name's fence, 0 for none.
func fencePID(t *testing.T, s *Store, name string) int {
	t.Helper()
	var pid int
	err := s.db.QueryRowContext(context.Background(), `SELECT coalesce(max(pid), 0) FROM pg_locks WHERE locktype = 'advisory' AND objsubid = 1
 AND granted AND ((classid::bigint << 32) | objid::bigint) = `+fenceKey, fenceName(name)).Scan(&pid)
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// A set taken under a held context joins its session, unlocks only itself on release, and
// may not rank before what is held; the outer loss or release ends the nested context too.
func TestNestedFencesJoinTheHeldSession(t *testing.T) {
	dbbackend.RequirePostgres(t)
	a, b := twoReplicas(t)
	owned, release, err := a.HoldFences(context.Background(), []Fence{{Name: vol.FenceNativeDomains}})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	inner, releaseInner, err := a.HoldFences(owned, []Fence{{Name: vol.FenceSSOLifecycle}})
	if err != nil {
		t.Fatal(err)
	}
	if pid := fencePID(t, a, vol.FenceNativeDomains); pid == 0 || pid != fencePID(t, a, vol.FenceSSOLifecycle) {
		t.Fatal("nested fence took a second session")
	}
	if _, _, err := a.HoldFences(inner, []Fence{{Name: vol.FenceSettings + "u1"}}); !errors.Is(err, errFence) {
		t.Fatal("a fence ranked before a held one was admitted", err)
	}
	won := make(chan error, 1)
	go func() { won <- b.WithFences(context.Background(), []Fence{{Name: vol.FenceSSOLifecycle}}, nop) }()
	eventually(t, "replica b to queue", func() bool { return fenceLocks(t, b, vol.FenceSSOLifecycle, false) == 1 })
	releaseInner()
	releaseInner()
	if err := <-won; err != nil {
		t.Fatal(err)
	}
	if fenceLocks(t, a, vol.FenceNativeDomains, true) != 1 || owned.Err() != nil {
		t.Fatal("a nested release dropped the outer fence")
	}
	again, releaseAgain, err := a.HoldFences(owned, []Fence{{Name: vol.FenceQuota + "m1"}})
	if err != nil {
		t.Fatal(err)
	}
	release()
	if again.Err() == nil {
		t.Fatal("outer release left the nested context live")
	}
	releaseAgain()
	if fenceLocks(t, b, vol.FenceNativeDomains, true)+fenceLocks(t, b, vol.FenceQuota+"m1", true) != 0 {
		t.Fatal("fences outlived the outer release")
	}
}

// A nested set waiting on the session blocks the probe's ping; the probe must skip, not
// read the wait as a lost session. A real loss still ends the nested context.
func TestNestedWaitIsNotALostFence(t *testing.T) {
	dbbackend.RequirePostgres(t)
	oldInterval, oldTimeout := lockProbeInterval, lockProbeTimeout
	lockProbeInterval, lockProbeTimeout = 20*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { lockProbeInterval, lockProbeTimeout = oldInterval, oldTimeout })
	a, b := twoReplicas(t)
	inside, leave, done := hold(b, []Fence{{Name: vol.FenceSSOLifecycle}})
	<-inside
	owned, release, err := a.HoldFences(context.Background(), []Fence{{Name: vol.FenceNativeDomains}})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	nested := make(chan error, 1)
	var inner context.Context
	go func() {
		var err error
		inner, _, err = a.HoldFences(owned, []Fence{{Name: vol.FenceSSOLifecycle}})
		nested <- err
	}()
	eventually(t, "nested set to queue", func() bool { return fenceLocks(t, b, vol.FenceSSOLifecycle, false) == 1 })
	time.Sleep(25 * lockProbeInterval)
	close(leave)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-nested; err != nil || owned.Err() != nil {
		t.Fatal("a nested wait ended ownership", err, context.Cause(owned))
	}
	var n int
	if err := b.db.QueryRowContext(context.Background(), `SELECT count(pg_terminate_backend($1))`, fencePID(t, a, vol.FenceNativeDomains)).Scan(&n); err != nil || n != 1 {
		t.Fatal("terminate", n, err)
	}
	select {
	case <-inner.Done():
		if !errors.Is(context.Cause(inner), ErrOwnershipLost) {
			t.Fatal("cause", context.Cause(inner))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a lost session left the nested holder running")
	}
}

// Two hosts mount their own roots over one table. A table lock is the database fence on
// both, never a local file; an unmounted path keeps its flock. The settings fence orders
// UpdateUserSettings across hosts, and a fenced save refuses settings changed since its load.
func TestTableLocksFenceAcrossHosts(t *testing.T) {
	dbbackend.RequirePostgres(t)
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	db, err := volumes.Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	table, err := vol.NewTable(db, "config", vol.TableOptions{MasterKey: bytes.Repeat([]byte{6}, 32), Org: func(context.Context, string) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	hostA, hostB := t.TempDir(), t.TempDir()
	for _, root := range []string{hostA, hostB} {
		release, err := vol.MountFenced(root, table, VolFence(dir))
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	pathA, pathB := filepath.Join(hostA, "people", "u1", "config.yaml"), filepath.Join(hostB, "people", "u1", "config.yaml")
	release, err := config.LockUserSettings(pathA)
	if err != nil {
		t.Fatal(err)
	}
	updated := make(chan error, 1)
	go func() {
		updated <- config.UpdateUserSettings(pathB, func(s *config.UserSettings) error { s.Notifications.Mode = "b"; return nil })
	}()
	eventually(t, "host b to queue", func() bool { return fenceLocks(t, st, vol.FenceSettings+"u1", false) == 1 })
	select {
	case err := <-updated:
		t.Fatal("host b changed settings while host a held the fence", err)
	case <-time.After(100 * time.Millisecond):
	}
	settings, save, err := config.LoadUserSettingsForSave(pathA)
	if err != nil {
		t.Fatal(err)
	}
	settings.Labels.Allowlist = []string{"a"}
	if err := save(settings); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	got, err := config.LoadUserSettings(pathA)
	if err != nil || got.Notifications.Mode != "b" || len(got.Labels.Allowlist) != 1 {
		t.Fatal("a fenced write was lost", got, err)
	}
	stale, save, err := config.LoadUserSettingsForSave(pathA)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateUserSettings(pathB, func(s *config.UserSettings) error { s.Notifications.Mode = "c"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := save(stale); !errors.Is(err, vol.ErrStale) {
		t.Fatal("a save overwrote settings changed since its load", err)
	}
	for _, p := range []string{pathA, pathB} {
		if _, err := os.Lstat(p + ".lock"); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("table lock created a local file", err)
		}
	}
	plain := filepath.Join(t.TempDir(), "u2", "config.yaml")
	ctx, unlock, err := vol.Lock(context.Background(), plain, vol.FenceSettings+"u2", vol.Exclusive)
	if err != nil || ctx.Err() != nil {
		t.Fatal(err)
	}
	unlock()
	if _, err := os.Lstat(plain + ".lock"); err != nil || fenceLocks(t, st, vol.FenceSettings+"u2", true) != 0 {
		t.Fatal("an unmounted path did not keep its flock", err)
	}
}
