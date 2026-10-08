//go:build linux

package sso

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func releaseStatus(t *testing.T, life *LifecycleStore, root string, settings SSOSettings, key []byte, accounts *users.Store, now time.Time) NativeRestoreReleaseStatus {
	t.Helper()
	all, err := accounts.List()
	if err != nil {
		t.Fatal(err)
	}
	return life.NativeRestoreReleaseStatus(root, t.TempDir(), settings, key, all, now)
}

func precondition(t *testing.T, st NativeRestoreReleaseStatus, id string) NativeRestoreReleaseCheck {
	t.Helper()
	i := slices.IndexFunc(st.Preconditions, func(c NativeRestoreReleaseCheck) bool { return c.ID == id })
	if i < 0 {
		t.Fatalf("no %s in %+v", id, st)
	}
	return st.Preconditions[i]
}

// treeDigest records every file's mode, mtime and content.
func treeDigest(t *testing.T, roots ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			// Every SQLite reader of a WAL database updates the -shm index;
			// its content, and every durable file, must stay unchanged.
			entry := info.Mode().String()
			if !strings.HasSuffix(path, "-shm") {
				entry += info.ModTime().String()
			}
			if info.Mode().IsRegular() {
				raw, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				sum := sha256.Sum256(raw)
				entry += hex.EncodeToString(sum[:])
			}
			out[path] = entry
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestNativeRestoreReleaseStatusNotHeld(t *testing.T) {
	st := NewLifecycleStore(t.TempDir()).NativeRestoreReleaseStatus(t.TempDir(), t.TempDir(), SSOSettings{}, nil, nil, time.Now())
	if st.Held || len(st.Preconditions) != 0 || st.ReleaseEnabled || st.Cloudflare != "" || len(st.NextSteps) != 1 {
		t.Fatalf("%+v", st)
	}
}

func TestNativeRestoreReleaseStatusRepairPreconditions(t *testing.T) {
	life, root, settings, key, accounts, u := publishedRecoveryFixture(t, false)
	st := releaseStatus(t, life, root, settings, key, accounts, time.Now())
	if p1 := precondition(t, st, "P1"); p1.OK || !reflect.DeepEqual(p1.Reasons, []string{"recovery evidence is recorded but repair has not run"}) {
		t.Fatalf("P1 before repair %+v", p1)
	}
	p3 := precondition(t, st, "P3")
	if p3.OK || !strings.Contains(strings.Join(p3.Reasons, "\n"), "restore again with this version to qualify for release") {
		t.Fatalf("P3 without marker %+v", p3)
	}
	if err := applyPublishedRecoveryBatch(t, life, root, settings, key, accounts, false); err != nil {
		t.Fatal(err)
	}
	if p1 := precondition(t, releaseStatus(t, life, root, settings, key, accounts, time.Now()), "P1"); p1.OK || !reflect.DeepEqual(p1.Reasons, []string{"repair did not complete; credential cleanup must succeed"}) {
		t.Fatalf("P1 incomplete repair %+v", p1)
	}
	if err := completePublishedRecoveryBatch(t, life, root, settings, key, accounts); err != nil {
		t.Fatal(err)
	}

	before := treeDigest(t, filepath.Dir(life.path), root)
	st = releaseStatus(t, life, root, settings, key, accounts, time.Now())
	if after := treeDigest(t, filepath.Dir(life.path), root); !reflect.DeepEqual(before, after) {
		for k := range after {
			if before[k] != after[k] {
				t.Errorf("changed %s: %q -> %q", k, before[k], after[k])
			}
		}
		for k := range before {
			if _, ok := after[k]; !ok {
				t.Errorf("removed %s", k)
			}
		}
		t.Fatal("status changed files")
	}
	for _, id := range []string{"P1", "P2", "P9"} {
		if c := precondition(t, st, id); !c.OK || len(c.Reasons) != 0 {
			t.Fatalf("%s after completed repair %+v", id, c)
		}
	}
	for _, id := range []string{"P4", "P5", "P6", "P8"} {
		if c := precondition(t, st, id); c.OK || !c.CheckedAtRelease {
			t.Fatalf("%s must be decided at release %+v", id, c)
		}
	}
	if p8 := precondition(t, st, "P8"); !strings.Contains(strings.Join(p8.Reasons, "\n"), "fresh domain proof is required at release") {
		t.Fatalf("P8 %+v", p8)
	}
	if !st.Held || st.Epoch != "12345678-1234-4123-8123-123456789abc" || len(st.Preconditions) != 9 || st.ReleaseEnabled {
		t.Fatalf("%+v", st)
	}

	f, err := life.load()
	if err != nil {
		t.Fatal(err)
	}
	if p1 := precondition(t, releaseStatus(t, life, root, settings, key, accounts, f.RecoveryRepair.ExpiresAt), "P1"); p1.OK || len(p1.Reasons) != 1 || !strings.HasPrefix(p1.Reasons[0], "repair evidence window ended at ") {
		t.Fatalf("P1 after expiry %+v", p1)
	}
	if p2 := precondition(t, releaseStatus(t, life, root, settings, []byte(strings.Repeat("x", 32)), accounts, time.Now()), "P2"); p2.OK {
		t.Fatalf("P2 with another pairing key %+v", p2)
	}
	// Digest drift: a local reactivation after the repair.
	if _, err := accounts.Reactivate(u.ID); err != nil {
		t.Fatal(err)
	}
	st = releaseStatus(t, life, root, settings, key, accounts, time.Now())
	if p2 := precondition(t, st, "P2"); p2.OK || !reflect.DeepEqual(p2.Reasons, []string{"authority changed since the repair"}) {
		t.Fatalf("P2 after drift %+v", p2)
	}
	if p1 := precondition(t, st, "P1"); !p1.OK {
		t.Fatalf("P1 after drift %+v", p1)
	}
}

func TestNativeRestoreReleaseStatusBarrierMismatch(t *testing.T) {
	life, root, settings, key, accounts, u := publishedRecoveryFixture(t, false)
	if err := applyPublishedRecoveryBatch(t, life, root, settings, key, accounts, false); err != nil {
		t.Fatal(err)
	}
	if err := completePublishedRecoveryBatch(t, life, root, settings, key, accounts); err != nil {
		t.Fatal(err)
	}
	f, err := life.load()
	if err != nil {
		t.Fatal(err)
	}
	k := directoryKey(settings.IssuerURL, u.SSOSub)
	barrier := f.RecoveryRepairBarriers[k]
	barrier.Nonce = strings.Repeat("0", 64)
	f.RecoveryRepairBarriers[k] = barrier
	if err = fsutil.PersistJSONFile(life.path, f); err != nil {
		t.Fatal(err)
	}
	if p2 := precondition(t, releaseStatus(t, life, root, settings, key, accounts, time.Now()), "P2"); p2.OK || !reflect.DeepEqual(p2.Reasons, []string{"repair barrier for account " + u.ID + " does not match"}) {
		t.Fatalf("P2 barrier %+v", p2)
	}
}

func TestNativeRestoreReleaseStatusTokenFence(t *testing.T) {
	life, _, settings, _, _, u := publishedRecoveryFixture(t, false)
	f, err := life.load()
	if err != nil {
		t.Fatal(err)
	}
	challenge := &f.RecoveryReceipt.Challenge
	k := directoryKey(settings.IssuerURL, u.SSOSub)
	setFence := func(at time.Time) {
		d := f.Directory[k]
		d.RevokedBefore = at.Unix()
		f.Directory[k] = d
	}
	created := challenge.CreatedAt.Add(-time.Minute).Truncate(time.Second)
	q := NativeRestoreQualification{CreatedAt: created}
	setFence(created.Add(31 * time.Second))
	if r := life.nativeRestoreTokenFenceReasons(f, q, challenge); len(r) != 0 {
		t.Fatal("fenced restore refused", r)
	}
	setFence(created)
	if r := life.nativeRestoreTokenFenceReasons(f, q, challenge); len(r) != 0 {
		t.Fatal("fence equal to createdAt refused", r)
	}
	for name, c := range map[string]struct {
		q         NativeRestoreQualification
		challenge *NativeRecoveryChallenge
		fence     time.Time
		reason    string
	}{
		"no-marker":         {NativeRestoreQualification{}, challenge, created, "needs the restore qualification marker (P3)"},
		"no-challenge":      {q, nil, created, "no recovery challenge is recorded"},
		"challenge-earlier": {NativeRestoreQualification{CreatedAt: challenge.CreatedAt.Add(time.Second)}, challenge, challenge.CreatedAt.Add(time.Minute), "the recovery challenge predates the restore qualification; request a new challenge"},
		"fence-earlier":     {q, challenge, created.Add(-time.Second), "mailbox " + u.ID + " token fence predates the restore qualification"},
	} {
		t.Run(name, func(t *testing.T) {
			setFence(c.fence)
			if r := life.nativeRestoreTokenFenceReasons(f, c.q, c.challenge); !reflect.DeepEqual(r, []string{c.reason}) {
				t.Fatalf("reasons %q", r)
			}
		})
	}
}

func TestNativeRestoreReleaseStatusZeroSubjectsAndFlag(t *testing.T) {
	config, root := t.TempDir(), t.TempDir()
	if err := fsutil.PersistJSONFile(filepath.Join(root, NativeRestoreHoldFile), map[string]any{"version": 1, "epoch": "12345678-1234-4123-8123-123456789abc"}); err != nil {
		t.Fatal(err)
	}
	settings := SSOSettings{Enabled: true, IssuerURL: nativeIssuer, ClientID: "kypost"}
	key := []byte(strings.Repeat("k", 32))
	for value, enabled := range map[string]bool{"": false, "false": false, "true": true, "yes": false} {
		t.Setenv("KYPOST_NATIVE_RESTORE_RELEASE", value)
		st := NewLifecycleStore(config).NativeRestoreReleaseStatus(root, t.TempDir(), settings, key, nil, time.Now())
		if st.ReleaseEnabled != enabled || (value == "yes") != slices.Contains(st.NextSteps, "KYPOST_NATIVE_RESTORE_RELEASE must be true or false.") {
			t.Fatalf("flag %q: %+v", value, st)
		}
		if p9 := precondition(t, st, "P9"); p9.OK || !reflect.DeepEqual(p9.Reasons, []string{"zero-subject hold: its release path is not available yet"}) {
			t.Fatalf("P9 %+v", p9)
		}
		if p8 := precondition(t, st, "P8"); p8.OK || p8.CheckedAtRelease || !reflect.DeepEqual(p8.Reasons, []string{noNativeDomain}) {
			t.Fatalf("P8 without a domain %+v", p8)
		}
	}
}
