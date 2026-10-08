//go:build linux

package sso

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

const releaseEpoch = "12345678-1234-4123-8123-123456789abc"

type releaseFix struct {
	life          *LifecycleStore
	root          string
	settings      SSOSettings
	key           []byte
	accounts      *users.Store
	one, two, leg users.User
}

// releaseCoreFixture is a qualified, fenced, repaired hold. Evidence: "one"
// (native) expired at its own revision, "two" (native) unchanged and active,
// "gone" (directory-only administrator row) deleted at a newer revision and
// "legacy-sub" (non-native SSO-linked administrator, no row) deleted.
func releaseCoreFixture(t *testing.T, completeRepair bool) releaseFix {
	t.Helper()
	ctx := context.Background()
	config, root := t.TempDir(), t.TempDir()
	life := NewLifecycleStore(config)
	domains := provenNativeDomain(t, config)
	accounts, err := users.LoadOrMigrate(ctx, config, filepath.Join(config, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	fx := releaseFix{life: life, root: root, settings: SSOSettings{Enabled: true, IssuerURL: nativeIssuer, ClientID: "kypost"}, key: []byte(strings.Repeat("k", 32)), accounts: accounts}
	for _, sub := range []string{"one", "two"} {
		nativeDesired(t, life, sub, sub+"@example.test", 1, true)
		u, err := life.AllocateNativeAccount(ctx, root, nativeIssuer, sub, domains, accounts, nativeLimits)
		if err != nil {
			t.Fatal(err)
		}
		if err = mailbox.RotateRestoredMessageReferences(filepath.Join(root, "users", u.ID, "mailbox", "mailbox.db"), u.NativeMailboxSource); err != nil {
			t.Fatal(err)
		}
		if sub == "one" {
			fx.one = u
		} else {
			fx.two = u
		}
	}
	nativeDesired(t, life, "gone", "gone@example.test", 1, true)
	if fx.leg, err = accounts.Create(ctx, "legacy-admin", "legacy-password-123456", users.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err = accounts.LinkSSO(fx.leg.ID, "legacy-sub", "legacy", ""); err != nil {
		t.Fatal(err)
	}
	if err = fsutil.PersistJSONFile(filepath.Join(root, NativeRestoreHoldFile), map[string]any{"version": 1, "epoch": releaseEpoch}); err != nil {
		t.Fatal(err)
	}
	all, _ := accounts.List()
	if err = life.RecordNativeRestoreQualification(root, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err = life.FenceRestoredNativeTokens(root, all); err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(fx.key)
	release, err := life.LockDirectory()
	if err != nil {
		t.Fatal(err)
	}
	err = accounts.WithCurrentUsers(ctx, func(all []users.User) error {
		c, err := life.BeginNativeRecoveryHeld(ctx, root, fx.settings, fx.key, all, "paired-system", hex.EncodeToString(fingerprint[:]))
		if err != nil {
			return err
		}
		body, headers := recoveryPayload(t, c, fx.key, func(e *nativeRecoveryEvidence) {
			e.Subjects = nil
			for _, sub := range c.Subjects {
				revision := map[string]int64{"gone": 2}[sub]
				revision = max(revision, 1)
				profile := fmt.Sprintf(`{"id":%q,"externalId":%q,"active":false,"roles":[]}`, sub, sub)
				if sub == "two" {
					profile = `{"id":"two","externalId":"two","userName":"two","active":true,"roles":[]}`
				}
				e.Subjects = append(e.Subjects, nativeRecoverySubject{ID: sub, Revision: &revision, Profile: json.RawMessage(profile)})
			}
		})
		return life.AcceptNativeRecoveryHeld(ctx, root, fx.settings, fx.key, all, body, headers)
	})
	release()
	if err != nil {
		t.Fatal(err)
	}
	if err = applyPublishedRecoveryBatch(t, life, root, fx.settings, fx.key, accounts, false); err != nil {
		t.Fatal(err)
	}
	if completeRepair {
		if err = completePublishedRecoveryBatch(t, life, root, fx.settings, fx.key, accounts); err != nil {
			t.Fatal(err)
		}
	}
	return fx
}

// runRelease is the API sequence without HTTP: fences, plan, intent, accounts, commit.
func (fx releaseFix) runRelease(t *testing.T, now time.Time, hit func(string) error) (*NativeRestoreReleasePlan, error) {
	t.Helper()
	ctx := context.Background()
	release, err := fx.life.LockDirectory()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var plan *NativeRestoreReleasePlan
	err = fx.accounts.DeactivateForRestoreRelease(ctx, func(all []users.User) ([]string, func(), error) {
		p, err := fx.life.PlanNativeRestoreReleaseHeld(fx.root, fx.settings, fx.key, all, "operator", now)
		if err != nil {
			return nil, nil, err
		}
		plan = p
		if err = p.RecordIntent(ctx); err == nil && hit != nil {
			err = hit("intent")
		}
		return p.Deactivate, nil, err
	}, func() error { return plan.Commit(ctx, hit) })
	return plan, err
}

func crashAt(point string) func(string) error {
	return func(p string) error {
		if p == point {
			return errors.New("crash at " + p)
		}
		return nil
	}
}

func TestNativeRestoreReleaseAppliesEvidence(t *testing.T) {
	fx := releaseCoreFixture(t, true)
	before := time.Now().Unix()
	plan, err := fx.runRelease(t, time.Now().UTC(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Deactivate, []string{fx.leg.ID}) {
		t.Fatalf("deactivate %v", plan.Deactivate)
	}
	if RequireNativeRestoreReleased(fx.root) != nil {
		t.Fatal("hold remains")
	}
	if epoch, err := readRestoreEpoch(filepath.Join(fx.root, NativeRestoreReleasedFile)); err != nil || epoch != releaseEpoch {
		t.Fatal("released marker", epoch, err)
	}
	record, released, err := fx.life.NativeRestoreReleased(fx.root)
	if err != nil || !released || record.Epoch != releaseEpoch || record.Actor != "operator" || record.CompletedAt != nil {
		t.Fatalf("record %+v %v %v", record, released, err)
	}
	// (a) the deleted non-native administrator is deactivated; nothing activated.
	if u, _ := fx.accounts.Get(fx.leg.ID); u.Active || u.DeactivatedAt == "" {
		t.Fatal("legacy administrator still active")
	}
	if u, _ := fx.accounts.Get(fx.one.ID); u.Active {
		t.Fatal("repaired native account activated")
	}
	f, err := fx.life.load()
	if err != nil {
		t.Fatal(err)
	}
	row := func(sub string) DirectoryState { return f.Directory[directoryKey(nativeIssuer, sub)] }
	// (b) evidence-inactive rows are inactive at the evidence revision.
	for sub, rev := range map[string]int64{"one": 1, "gone": 2, "legacy-sub": 1} {
		d := row(sub)
		if d.Active || d.Revision != rev || d.Resource == nil || *d.Resource.Active || d.EventID != nativeReleaseEventID {
			t.Fatalf("%s row %+v", sub, d)
		}
	}
	if d := row("two"); !d.Active || d.Revision != 1 || d.EventID == nativeReleaseEventID {
		t.Fatalf("active row rewritten %+v", d)
	}
	// Every evidence subject's ID tokens issued during the hold are refused.
	for _, sub := range []string{"one", "two", "gone", "legacy-sub"} {
		if row(sub).RevokedBefore < before+31 {
			t.Fatalf("%s token fence %d", sub, row(sub).RevokedBefore)
		}
	}
	want := map[string]NativeReleaseFloor{"one": {1, false}, "two": {1, true}, "gone": {2, false}, "legacy-sub": {1, false}}
	for sub, floor := range want {
		if f.ReleaseFloors[directoryKey(nativeIssuer, sub)] != floor {
			t.Fatalf("%s floor %+v", sub, f.ReleaseFloors[directoryKey(nativeIssuer, sub)])
		}
	}
	// (b) the expired subject's address is disabled; the active one's is not.
	ledger, err := fx.life.loadNative()
	if err != nil {
		t.Fatal(err)
	}
	if x := ledger.stored.Addresses["one@example.test"]; x.State != "disabled" {
		t.Fatalf("one address %+v", x)
	}
	if x := ledger.stored.Addresses["two@example.test"]; x.State != "active" {
		t.Fatalf("two address %+v", x)
	}
	all, _ := fx.accounts.List()
	if _, err = fx.life.ValidateNativeSnapshot(fx.root, all); err != nil {
		t.Fatal("released state is inconsistent", err)
	}
	// Floors: the deleted administrator row cannot provision from stale state.
	if fx.life.CheckNativeReleaseFloor(nativeIssuer, "gone", DirectoryState{Revision: 2, Active: true}) == nil {
		t.Fatal("deleted administrator not floored")
	}
	// The directory's own deletion at the evidence revision is the same state;
	// an active event at that revision contradicts signed evidence.
	gone, digest := releaseResourceFor(t, "gone", 2, false)
	if status, err := fx.life.ApplyDirectoryUser(nativeIssuer, syncauth.Event{ID: "gone-delete", Type: "user.deleted", At: time.Now()}, gone, digest, func() (bool, error) { return true, nil }); err != nil || status != DirectoryAlreadyApplied {
		t.Fatal("queued deletion refused", status, err)
	}
	one, digest := releaseResourceFor(t, "one", 1, true)
	if _, err := fx.life.ApplyDirectoryUser(nativeIssuer, syncauth.Event{ID: "one-stale", Type: "user.updated", At: time.Now()}, one, digest, func() (bool, error) { return false, nil }); !errors.Is(err, ErrDirectoryConflict) {
		t.Fatal("stale reactivation accepted", err)
	}
	// (c) status: released, resync required for the active floored subject.
	st := fx.life.NativeRestoreReleaseStatus(fx.root, t.TempDir(), fx.settings, fx.key, all, time.Now())
	if !st.Released || st.Held || !reflect.DeepEqual(st.ResyncSubjects, []string{"two"}) || !slices.ContainsFunc(st.NextSteps, func(s string) bool { return strings.HasPrefix(s, "Run a KyIdentity resync") }) {
		t.Fatalf("status %+v", st)
	}
	if err = fx.life.CompleteNativeRestoreRelease(fx.root); err != nil {
		t.Fatal(err)
	}
	if record, _, _ = fx.life.NativeRestoreReleased(fx.root); record.CompletedAt == nil {
		t.Fatal("completion not recorded")
	}
	nativeDesired(t, fx.life, "two", "two@example.test", 2, true)
	if st = fx.life.NativeRestoreReleaseStatus(fx.root, t.TempDir(), fx.settings, fx.key, all, time.Now()); len(st.ResyncSubjects) != 0 || slices.ContainsFunc(st.NextSteps, func(s string) bool { return strings.HasPrefix(s, "Run a KyIdentity resync") }) {
		t.Fatalf("resync step after a newer revision %+v", st)
	}
}

func releaseResourceFor(t *testing.T, sub string, revision int64, active bool) (DirectoryUser, string) {
	t.Helper()
	u := DirectoryUser{Schemas: []string{scimUserSchema}, ID: sub, ExternalID: sub, UserName: sub, Active: &active}
	u.Meta.Version = fmt.Sprintf(`W/"%d"`, revision)
	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	return u, EventDigest("user.updated", raw)
}

type releaseSnapshot struct{ lifecycle, ledger, hold []byte }

func (fx releaseFix) snapshot(t *testing.T) (releaseSnapshot, []users.User) {
	t.Helper()
	l, _ := os.ReadFile(fx.life.path)
	n, _ := os.ReadFile(fx.life.nativePath())
	h, _ := os.ReadFile(filepath.Join(fx.root, NativeRestoreHoldFile))
	all, _ := fx.accounts.List()
	return releaseSnapshot{l, n, h}, all
}

func TestNativeRestoreReleaseRefusesWithoutMutation(t *testing.T) {
	persist := func(t *testing.T, fx releaseFix, edit func(*lifecycleFile)) {
		f, err := fx.life.load()
		if err != nil {
			t.Fatal(err)
		}
		edit(&f)
		if err = fsutil.PersistJSONFile(fx.life.path, f); err != nil {
			t.Fatal(err)
		}
	}
	for name, c := range map[string]struct {
		partial bool
		damage  func(*testing.T, *releaseFix) time.Time
		reason  string
	}{
		"P1-partial-repair": {true, nil, "P1: repair did not complete"},
		"P1-expired-evidence": {false, func(t *testing.T, fx *releaseFix) time.Time {
			f, _ := fx.life.load()
			return f.RecoveryRepair.ExpiresAt
		}, "P1: repair evidence window ended"},
		"P1-replayed-nonce": {false, func(t *testing.T, fx *releaseFix) time.Time {
			persist(t, *fx, func(f *lifecycleFile) { f.RecoveryRepair.Nonce = strings.Repeat("0", 64) })
			return time.Now()
		}, "P1: repair journal does not match"},
		"P1-wrong-system": {false, func(t *testing.T, fx *releaseFix) time.Time {
			persist(t, *fx, func(f *lifecycleFile) { f.RecoveryReceipt.Challenge.SystemID = "other-system" })
			return time.Now()
		}, "P1: repair journal does not match"},
		"P2-drift": {false, func(t *testing.T, fx *releaseFix) time.Time {
			if _, err := fx.accounts.Reactivate(fx.one.ID); err != nil {
				t.Fatal(err)
			}
			return time.Now()
		}, "P2: authority changed since the repair"},
		"P2-wrong-issuer": {false, func(t *testing.T, fx *releaseFix) time.Time {
			fx.settings.IssuerURL = "https://other.example"
			return time.Now()
		}, "P2: "},
		"P2-previous-epoch": {false, func(t *testing.T, fx *releaseFix) time.Time {
			if err := fsutil.PersistJSONFile(filepath.Join(fx.root, NativeRestoreHoldFile), map[string]any{"version": 1, "epoch": "12345678-1234-4123-8123-123456789abd"}); err != nil {
				t.Fatal(err)
			}
			return time.Now()
		}, "P2: repair belongs to another issuer, restore epoch or pairing key"},
		"P3-missing-marker": {false, func(t *testing.T, fx *releaseFix) time.Time {
			if err := os.Remove(filepath.Join(fx.root, NativeRestoreQualificationFile)); err != nil {
				t.Fatal(err)
			}
			return time.Now()
		}, "P3: no restore qualification marker"},
		"P7-marker-after-challenge": {false, func(t *testing.T, fx *releaseFix) time.Time {
			q, err := readNativeRestoreQualification(filepath.Join(fx.root, NativeRestoreQualificationFile))
			if err != nil {
				t.Fatal(err)
			}
			q.CreatedAt = time.Now().Add(time.Hour).UTC()
			if err = fsutil.PersistJSONFile(filepath.Join(fx.root, NativeRestoreQualificationFile), q); err != nil {
				t.Fatal(err)
			}
			return time.Now()
		}, "P7: the recovery challenge predates the restore qualification"},
		"P7-token-fence": {false, func(t *testing.T, fx *releaseFix) time.Time {
			// Lowering a fence also drifts authority; P7 still names it.
			persist(t, *fx, func(f *lifecycleFile) {
				k := directoryKey(nativeIssuer, "two")
				d := f.Directory[k]
				d.RevokedBefore = 1
				f.Directory[k] = d
			})
			return time.Now()
		}, "P7: mailbox " + "two"},
		"interrupted-intent": {false, func(t *testing.T, fx *releaseFix) time.Time {
			persist(t, *fx, func(f *lifecycleFile) { f.RestoreRelease = &NativeRestoreReleaseRecord{Epoch: releaseEpoch} })
			return time.Now()
		}, "an earlier release attempt for this hold was interrupted"},
	} {
		t.Run(name, func(t *testing.T) {
			fx := releaseCoreFixture(t, !c.partial)
			now := time.Now()
			if c.damage != nil {
				now = c.damage(t, &fx)
			}
			if name == "P7-token-fence" {
				c.reason = "P7: mailbox " + fx.two.ID + " token fence predates the restore qualification"
			}
			before, accounts := fx.snapshot(t)
			_, err := fx.runRelease(t, now.UTC(), nil)
			var refused *NativeRestoreRefusedError
			if !errors.As(err, &refused) || !slices.ContainsFunc(refused.Reasons, func(r string) bool { return strings.HasPrefix(r, c.reason) }) {
				t.Fatalf("want %q, got %v", c.reason, err)
			}
			after, accountsAfter := fx.snapshot(t)
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(accounts, accountsAfter) || RequireNativeRestoreReleased(fx.root) == nil {
				t.Fatal("refusal mutated state or released the hold")
			}
		})
	}
}

func TestNativeRestoreReleaseZeroSubjects(t *testing.T) {
	config, root := t.TempDir(), t.TempDir()
	if err := fsutil.PersistJSONFile(filepath.Join(root, NativeRestoreHoldFile), map[string]any{"version": 1, "epoch": releaseEpoch}); err != nil {
		t.Fatal(err)
	}
	_, err := NewLifecycleStore(config).PlanNativeRestoreReleaseHeld(root, SSOSettings{Enabled: true, IssuerURL: nativeIssuer, ClientID: "kypost"}, []byte(strings.Repeat("k", 32)), nil, "operator", time.Now())
	var refused *NativeRestoreRefusedError
	if !errors.As(err, &refused) || !strings.HasPrefix(refused.Reasons[0], "P9:") {
		t.Fatal(err)
	}
}

func TestNativeRestoreReleaseCrashPoints(t *testing.T) {
	for _, point := range []string{"intent", "lifecycle", "rename"} {
		t.Run(point, func(t *testing.T) {
			fx := releaseCoreFixture(t, true)
			if _, err := fx.runRelease(t, time.Now().UTC(), crashAt(point)); err == nil {
				t.Fatal("crash not reported")
			}
			_, released, err := fx.life.NativeRestoreReleased(fx.root)
			if point == "rename" {
				if !released || err != nil || RequireNativeRestoreReleased(fx.root) != nil {
					t.Fatal("rename crash must read as released", released, err)
				}
				return
			}
			if released || RequireNativeRestoreReleased(fx.root) == nil {
				t.Fatal("crash before rename released the hold")
			}
			// The intent refuses a retry; only fresh evidence clears it.
			_, err = fx.runRelease(t, time.Now().UTC(), nil)
			var refused *NativeRestoreRefusedError
			if !errors.As(err, &refused) || RequireNativeRestoreReleased(fx.root) == nil {
				t.Fatal("retry without fresh evidence", err)
			}
			if u, _ := fx.accounts.Get(fx.leg.ID); u.Active != (point == "intent") {
				t.Fatal("deactivation must precede the lifecycle write", point, u.Active)
			}
			if point == "lifecycle" {
				f, _ := fx.life.load()
				if len(f.ReleaseFloors) != 4 {
					t.Fatal("floors not durable before the rename")
				}
			}
			// A new challenge clears the intent; floors only go up.
			fingerprint := sha256.Sum256(fx.key)
			release, err := fx.life.LockDirectory()
			if err != nil {
				t.Fatal(err)
			}
			err = fx.accounts.WithCurrentUsers(context.Background(), func(all []users.User) error {
				_, err := fx.life.BeginNativeRecoveryHeld(context.Background(), fx.root, fx.settings, fx.key, all, "paired-system", hex.EncodeToString(fingerprint[:]))
				return err
			})
			release()
			if f, _ := fx.life.load(); err != nil || f.RestoreRelease != nil || point == "lifecycle" && len(f.ReleaseFloors) != 4 {
				t.Fatal("new challenge kept the intent or dropped floors", err)
			}
		})
	}
}

// A directory event racing release lands wholly before (release refused: the
// newer revision drops the receipt) or after (released, floors in place).
func TestNativeRestoreReleaseRacesDirectoryEvent(t *testing.T) {
	for i := range 4 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			fx := releaseCoreFixture(t, true)
			var wg sync.WaitGroup
			var releaseErr error
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, releaseErr = fx.runRelease(t, time.Now().UTC(), nil)
			}()
			u, digest := releaseResourceFor(t, "two", 5, true)
			if _, err := fx.life.ApplyDirectoryUser(nativeIssuer, syncauth.Event{ID: "two-5", Type: "user.updated", At: time.Now()}, u, digest, func() (bool, error) { return false, nil }); err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			f, err := fx.life.load()
			if err != nil {
				t.Fatal(err)
			}
			held := RequireNativeRestoreReleased(fx.root) != nil
			switch {
			case releaseErr == nil && !held && len(f.ReleaseFloors) == 4:
			case releaseErr != nil && held && len(f.ReleaseFloors) == 0 && f.RestoreRelease == nil:
			default:
				t.Fatalf("inconsistent: err=%v held=%v floors=%d", releaseErr, held, len(f.ReleaseFloors))
			}
			all, _ := fx.accounts.List()
			if _, err = fx.life.ValidateNativeSnapshot(fx.root, all); err != nil {
				t.Fatal(err)
			}
			if f.Directory[directoryKey(nativeIssuer, "two")].Revision != 5 {
				t.Fatal("directory event lost")
			}
		})
	}
}

// A new restore epoch landing after the lifecycle write must not be renamed away.
func TestNativeRestoreReleaseRechecksEpochBeforeRename(t *testing.T) {
	fx := releaseCoreFixture(t, true)
	hold := filepath.Join(fx.root, NativeRestoreHoldFile)
	_, err := fx.runRelease(t, time.Now().UTC(), func(point string) error {
		if point != "lifecycle" {
			return nil
		}
		return fsutil.PersistJSONFile(hold, map[string]any{"version": 1, "epoch": "12345678-1234-4123-8123-123456789abd"})
	})
	if !errors.Is(err, ErrNativeRecovery) || RequireNativeRestoreReleased(fx.root) == nil {
		t.Fatal("renamed another epoch's hold", err)
	}
}
