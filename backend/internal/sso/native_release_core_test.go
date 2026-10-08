//go:build linux

package sso

import (
	"context"
	"crypto/sha256"
	"database/sql"
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
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
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
	// Linked administrators: one KyIdentity demoted, one still administrator;
	// and a linked user KyIdentity promoted (never promoted locally).
	for name, role := range map[string]users.Role{"demoted": users.RoleAdmin, "kept": users.RoleAdmin, "user": users.RoleUser} {
		u, err := accounts.Create(ctx, name+"-account", "linked-password-123456", role)
		if err != nil {
			t.Fatal(err)
		}
		if err = accounts.LinkSSO(u.ID, name+"-sub", name, ""); err != nil {
			t.Fatal(err)
		}
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
	fx.freshRepair(t, completeRepair)
	return fx
}

// releaseEvidence is KyIdentity's answer for the fixture's subjects.
func releaseEvidence(subjects []string) []nativeRecoverySubject {
	var out []nativeRecoverySubject
	for _, sub := range subjects {
		revision := max(map[string]int64{"gone": 2}[sub], 1)
		profile := fmt.Sprintf(`{"id":%q,"externalId":%q,"active":false,"roles":[]}`, sub, sub)
		switch sub {
		case "two", "demoted-sub":
			profile = fmt.Sprintf(`{"id":%q,"externalId":%q,"userName":%q,"active":true,"roles":[]}`, sub, sub, sub)
		case "kept-sub", "user-sub":
			profile = fmt.Sprintf(`{"id":%q,"externalId":%q,"userName":%q,"active":true,"roles":["kypost.admin"]}`, sub, sub, sub)
		}
		out = append(out, nativeRecoverySubject{ID: sub, Revision: &revision, Profile: json.RawMessage(profile)})
	}
	return out
}

const releaseSubjects = 7 // one, two, gone, legacy-sub, demoted-sub, kept-sub, user-sub

// freshRepair is challenge -> signed evidence -> repair (-> completion) against
// the current state: the documented recovery after an interrupted release.
func (fx releaseFix) freshRepair(t *testing.T, complete bool) {
	t.Helper()
	ctx := context.Background()
	fingerprint := sha256.Sum256(fx.key)
	release, err := fx.life.LockDirectory()
	if err != nil {
		t.Fatal(err)
	}
	err = fx.accounts.WithCurrentUsers(ctx, func(all []users.User) error {
		c, err := fx.life.BeginNativeRecoveryHeld(ctx, fx.root, fx.settings, fx.key, all, "paired-system", hex.EncodeToString(fingerprint[:]))
		if err != nil {
			return err
		}
		body, headers := recoveryPayload(t, c, fx.key, func(e *nativeRecoveryEvidence) { e.Subjects = releaseEvidence(c.Subjects) })
		return fx.life.AcceptNativeRecoveryHeld(ctx, fx.root, fx.settings, fx.key, all, body, headers)
	})
	release()
	if err != nil {
		t.Fatal(err)
	}
	if err = applyPublishedRecoveryBatch(t, fx.life, fx.root, fx.settings, fx.key, fx.accounts, false); err != nil {
		t.Fatal(err)
	}
	if complete {
		if err = completePublishedRecoveryBatch(t, fx.life, fx.root, fx.settings, fx.key, fx.accounts); err != nil {
			t.Fatal(err)
		}
	}
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
	err = fx.accounts.DeactivateForRestoreRelease(ctx, func(all []users.User) ([]string, []string, func(), error) {
		p, err := fx.life.PlanNativeRestoreReleaseHeld(fx.root, fx.settings, fx.key, all, "operator", now)
		if err != nil {
			return nil, nil, nil, err
		}
		plan = p
		if err = p.RecordIntent(ctx); err == nil && hit != nil {
			err = hit("intent")
		}
		return p.Record.Deactivated, p.Record.Demoted, nil, err
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
	demoted, _ := fx.accounts.GetByUsername("demoted-account")
	if !reflect.DeepEqual(plan.Record.Deactivated, []string{fx.leg.ID}) || !reflect.DeepEqual(plan.Record.Demoted, []string{demoted.ID}) {
		t.Fatalf("account changes %+v", plan.Record)
	}
	// (a) roles: demote, never promote.
	for name, role := range map[string]users.Role{"demoted-account": users.RoleUser, "kept-account": users.RoleAdmin, "user-account": users.RoleUser} {
		if u, _ := fx.accounts.GetByUsername(name); !u.Active || u.Role != role {
			t.Fatalf("%s: active=%v role=%s", name, u.Active, u.Role)
		}
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
	want := map[string]NativeReleaseFloor{"one": {1, false}, "two": {1, true}, "gone": {2, false}, "legacy-sub": {1, false}, "demoted-sub": {1, true}, "kept-sub": {1, true}, "user-sub": {1, true}}
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
	if !st.Released || st.Held || !reflect.DeepEqual(st.ResyncSubjects, []string{"demoted-sub", "kept-sub", "two", "user-sub"}) || !slices.ContainsFunc(st.NextSteps, func(s string) bool { return strings.HasPrefix(s, "Run a KyIdentity resync") }) {
		t.Fatalf("status %+v", st)
	}
	if err = fx.life.CompleteNativeRestoreRelease(fx.root); err != nil {
		t.Fatal(err)
	}
	if record, _, _ = fx.life.NativeRestoreReleased(fx.root); record.CompletedAt == nil {
		t.Fatal("completion not recorded")
	}
	for _, sub := range st.ResyncSubjects {
		nativeDesired(t, fx.life, sub, sub+"@example.test", 2, true)
	}
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
		}, "P2: " + uncomputedAuthority},
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

// Each crash point, then the documented recovery: before the rename the intent
// refuses a retry; a new challenge, fresh evidence and repair release cleanly.
func TestNativeRestoreReleaseCrashPointsAndRecovery(t *testing.T) {
	for _, point := range []string{"intent", "ledger", "lifecycle", "rename"} {
		t.Run(point, func(t *testing.T) {
			fx := releaseCoreFixture(t, true)
			_, err := fx.runRelease(t, time.Now().UTC(), crashAt(point))
			if err == nil {
				t.Fatal("crash not reported")
			}
			_, released, rerr := fx.life.NativeRestoreReleased(fx.root)
			if point == "rename" {
				if !errors.Is(err, ErrNativeReleaseUnconfirmed) || !released || rerr != nil || RequireNativeRestoreReleased(fx.root) != nil {
					t.Fatal("a crash after the rename must read as released", err, released, rerr)
				}
				return
			}
			if errors.Is(err, ErrNativeReleaseUnconfirmed) || released || RequireNativeRestoreReleased(fx.root) == nil {
				t.Fatal("crash before rename released the hold")
			}
			_, err = fx.runRelease(t, time.Now().UTC(), nil)
			var refused *NativeRestoreRefusedError
			if !errors.As(err, &refused) || RequireNativeRestoreReleased(fx.root) == nil {
				t.Fatal("retry without fresh evidence", err)
			}
			if u, _ := fx.accounts.Get(fx.leg.ID); u.Active != (point == "intent") {
				t.Fatal("account changes must precede the ledger and lifecycle writes", point, u.Active)
			}
			if f, _ := fx.life.load(); (point == "lifecycle") != (len(f.ReleaseFloors) == releaseSubjects) {
				t.Fatal("floors and the lifecycle write are one", point, len(f.ReleaseFloors))
			}
			fx.freshRepair(t, true)
			if f, _ := fx.life.load(); f.RestoreRelease != nil {
				t.Fatal("new challenge kept the intent")
			}
			if _, err = fx.runRelease(t, time.Now().UTC(), nil); err != nil {
				t.Fatal("recovery release", err)
			}
			fx.assertReleased(t)
		})
	}
}

// assertReleased is the consistent end state of every successful release.
func (fx releaseFix) assertReleased(t *testing.T) {
	t.Helper()
	if _, released, err := fx.life.NativeRestoreReleased(fx.root); !released || err != nil || RequireNativeRestoreReleased(fx.root) != nil {
		t.Fatal("not released", err)
	}
	f, err := fx.life.load()
	if err != nil || len(f.ReleaseFloors) != releaseSubjects {
		t.Fatal("floors", len(f.ReleaseFloors), err)
	}
	for _, sub := range []string{"one", "gone", "legacy-sub"} {
		if d := f.Directory[directoryKey(nativeIssuer, sub)]; d.Active {
			t.Fatalf("%s row active", sub)
		}
	}
	if u, _ := fx.accounts.Get(fx.leg.ID); u.Active {
		t.Fatal("legacy administrator active")
	}
	if u, _ := fx.accounts.GetByUsername("demoted-account"); u.Role != users.RoleUser {
		t.Fatal("demotion lost")
	}
	all, _ := fx.accounts.List()
	if _, err = fx.life.ValidateNativeSnapshot(fx.root, all); err != nil {
		t.Fatal("inconsistent", err)
	}
}

// A real older receipt and repair replayed over a newer repair is refused.
func TestNativeRestoreReleaseRefusesOlderReceipt(t *testing.T) {
	fx := releaseCoreFixture(t, true)
	old, err := fx.life.load()
	if err != nil {
		t.Fatal(err)
	}
	fx.freshRepair(t, true)
	f, err := fx.life.load()
	if err != nil {
		t.Fatal(err)
	}
	f.RecoveryReceipt, f.RecoveryRepair = old.RecoveryReceipt, old.RecoveryRepair
	if err = fsutil.PersistJSONFile(fx.life.path, f); err != nil {
		t.Fatal(err)
	}
	_, err = fx.runRelease(t, time.Now().UTC(), nil)
	var refused *NativeRestoreRefusedError
	if !errors.As(err, &refused) || !slices.ContainsFunc(refused.Reasons, func(r string) bool { return strings.HasPrefix(r, "P2: repair barrier for account") }) || RequireNativeRestoreReleased(fx.root) == nil {
		t.Fatal("older receipt accepted", err)
	}
}

// A release record restored from a post-release backup belongs to an older
// epoch; it neither refuses nor survives the new hold's release.
func TestNativeRestoreReleaseIgnoresOtherEpochRecord(t *testing.T) {
	fx := releaseCoreFixture(t, true)
	f, err := fx.life.load()
	if err != nil {
		t.Fatal(err)
	}
	f.RestoreRelease = &NativeRestoreReleaseRecord{Epoch: "12345678-1234-4123-8123-123456789abd", Actor: "earlier"}
	if err = fsutil.PersistJSONFile(fx.life.path, f); err != nil {
		t.Fatal(err)
	}
	plan, err := fx.runRelease(t, time.Now().UTC(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if record, _, _ := fx.life.NativeRestoreReleased(fx.root); record.Epoch != releaseEpoch || record.Actor != plan.Record.Actor {
		t.Fatalf("record %+v", record)
	}
	fx.assertReleased(t)
}

// With a receiving store present, the expired subject's route is written inactive.
func TestNativeRestoreReleaseDeactivatesRoutes(t *testing.T) {
	ctx := context.Background()
	fx := releaseCoreFixture(t, true)
	ledger, err := fx.life.loadNative()
	if err != nil {
		t.Fatal(err)
	}
	holding, err := ingress.Open(filepath.Join(fx.root, "receiving"), ingress.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100})
	if err != nil {
		t.Fatal(err)
	}
	x := ledger.stored.Addresses["one@example.test"]
	if err = holding.SetRoute(ctx, ingress.Route{Address: "one@example.test", Issuer: nativeIssuer, Subject: "one", Mailbox: fx.one.ID, Generation: x.Generation, Active: true, ValidUntil: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err = holding.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = fx.runRelease(t, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(fx.root, "receiving", "ingress.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var active bool
	var generation int64
	if err = db.QueryRow("SELECT active,generation FROM routes WHERE address='one@example.test'").Scan(&active, &generation); err != nil || active || generation <= x.Generation {
		t.Fatal("route still active", active, generation, err)
	}
	fx.assertReleased(t)
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
			case releaseErr == nil && !held && len(f.ReleaseFloors) == releaseSubjects:
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
