package backup

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/ky-primitives/recoverykey"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// replicaOf is a second process on s's volumes and database.
func replicaOf(t *testing.T, s *Service) *Service {
	t.Helper()
	return openService(t, s.dirs, s.cfg)
}

func newKey(t *testing.T) recoverykey.PrivateKey {
	t.Helper()
	k, err := recoverykey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pin(s *Service, k recoverykey.PrivateKey, threshold, total int) error {
	return s.PinKey(base64.StdEncoding.EncodeToString(k.Public().Bytes()), threshold, total)
}

// AC19: a pin on A lets B, with its recovery.pub gone, seal to the same key after rewriting
// the file from the record; a tampered record, a database-only key swap and a stale file each
// refuse the run and show on status, and write no file.
func TestPinnedKeyReachesEveryReplica(t *testing.T) {
	dbbackend.RequirePostgres(t)
	a := validService(t)
	t.Setenv("TUNING_FILE", filepath.Join(a.dirs.Config, "TUNING.md"))
	b := replicaOf(t, a)
	key := newKey(t)
	if err := pin(a, key, 2, 3); err != nil {
		t.Fatal(err)
	}
	pub := recoveryclient.RecoveryKeyPath(a.dirs.State)
	if err := os.Remove(pub); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncRecoveryKey(); err != nil {
		t.Fatalf("B could not rewrite recovery.pub from the record: %v", err)
	}
	res, err := b.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.RecoveryKeyID != key.Public().ID() {
		t.Fatalf("B sealed to %s, want %s", res.Manifest.RecoveryKeyID, key.Public().ID())
	}

	sealed, err := a.store.Setting(anchorSetting)
	if err != nil {
		t.Fatal(err)
	}
	refuses := func(name string) {
		t.Helper()
		if err := b.SyncRecoveryKey(); !errors.Is(err, ErrAnchor) {
			t.Fatalf("%s: sync %v", name, err)
		}
		if _, err := b.Run(context.Background()); !errors.Is(err, ErrAnchor) {
			t.Fatalf("%s: run %v", name, err)
		}
		if st, err := b.Status(); err != nil || st.KeyProblem == "" {
			t.Fatalf("%s: status %+v %v", name, st.KeyProblem, err)
		}
	}
	raw, _ := base64.StdEncoding.DecodeString(sealed)
	raw[len(raw)-1] ^= 1
	must(t, a.store.SetSetting(anchorSetting, base64.StdEncoding.EncodeToString(raw)))
	refuses("a flipped byte in the record")
	must(t, a.store.SetSetting(anchorSetting, sealed))

	// A database writer without totp-secret.key moves the pin to its own key.
	other := newKey(t)
	must(t, a.store.SetSetting("kyrecovery_key_id", other.Public().ID()))
	must(t, os.Remove(pub))
	refuses("a key swapped in the database")
	if _, err := os.Stat(pub); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused sync wrote recovery.pub")
	}
	must(t, a.store.SetSetting("kyrecovery_key_id", key.Public().ID()))

	// A stale recovery.pub naming another key.
	must(t, os.WriteFile(pub, other.Public().Bytes(), 0o600))
	refuses("a stale recovery.pub")
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// AC19: two replicas pinning two keys at once: one wins, the other is refused and writes no
// file; the same key with another topology is refused and the stored one is kept.
func TestConcurrentPinsAndWriteOnceTopology(t *testing.T) {
	a := validService(t)
	b := replicaOf(t, a)
	k1, k2 := newKey(t), newKey(t)
	var errs [2]error
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, s := range []*Service{a, b} {
		wg.Go(func() {
			<-start
			k := []recoverykey.PrivateKey{k1, k2}[i]
			errs[i] = pin(s, k, 2, 3)
		})
	}
	close(start)
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("want one pin: %v / %v", errs[0], errs[1])
	}
	winner, loser := k1, errs[1]
	if errs[0] != nil {
		winner, loser = k2, errs[0]
	}
	if !errors.Is(loser, ErrKeyAlreadyPinned) && !errors.Is(loser, recoveryclient.ErrInProgress) {
		t.Fatalf("loser: %v", loser)
	}
	b2, err := os.ReadFile(recoveryclient.RecoveryKeyPath(a.dirs.State))
	if err != nil || string(b2) != string(winner.Public().Bytes()) {
		t.Fatal("recovery.pub does not hold the winner's key")
	}
	if err := pin(b, winner, 3, 5); !errors.Is(err, ErrTopologyPinned) {
		t.Fatalf("same key, other topology: %v", err)
	}
	if id, _ := a.PinnedKeyID(); id != winner.Public().ID() {
		t.Fatal("pin moved")
	}
	if th, _ := a.store.Setting("kyrecovery_threshold"); th != "2" {
		t.Fatalf("stored topology changed to %s", th)
	}
	if err := pin(b, winner, 2, 3); err != nil {
		t.Fatalf("same key, same topology: %v", err)
	}
}

// AC19: the pin's rows commit but the sealed record does not: reported, file kept.
func TestPinWithoutRecordIsReported(t *testing.T) {
	dbbackend.RequirePostgres(t)
	a := validService(t)
	totp := filepath.Join(a.dirs.Secret, "totp-secret.key")
	saved, err := os.ReadFile(totp)
	must(t, err)
	must(t, os.Remove(totp))
	if err := pin(a, newKey(t), 2, 3); !errors.Is(err, ErrPinNotRecorded) {
		t.Fatalf("pin without a record: %v", err)
	}
	if _, err := os.Stat(recoveryclient.RecoveryKeyPath(a.dirs.State)); err != nil {
		t.Fatal("the pinned key's file was not kept")
	}
	must(t, os.WriteFile(totp, saved, 0o600))
	if st, err := a.Status(); err != nil || st.KeyProblem == "" {
		t.Fatalf("status of a pin without its record: %q %v", st.KeyProblem, err)
	}
}

// AC20: every operation reads the pairing in one statement. The hook re-pairs between every
// database read; with per-key reads InspectPairing would pair one URL with another token.
func TestOneOperationNeverMixesTwoPairings(t *testing.T) {
	a := validService(t)
	pinTestKey(t, a)
	sealer, err := a.loadSealer()
	must(t, err)
	pairs := [][2]string{{"https://a.example", "token-a"}, {"https://b.example", "token-b"}}
	setPair := func(i int) {
		must(t, a.store.BackupSettingsTransaction(func(st recoveryclient.Settings) error {
			return recoveryclient.StorePairing(st, sealer, pairs[i][0], pairs[i][1])
		}))
	}
	setPair(0)
	var flips atomic.Int32
	settingsRead = func() error {
		setPair(int(flips.Add(1)) % 2)
		return nil
	}
	t.Cleanup(func() { settingsRead = func() error { return nil } })
	for range 4 {
		p, err := a.InspectPairing()
		must(t, err)
		if got := [2]string{p.URL, p.Token}; got != pairs[0] && got != pairs[1] {
			t.Fatalf("one read paired %s with %s", p.URL, p.Token)
		}
		st, err := a.Status()
		must(t, err)
		if !st.Paired {
			t.Fatal("status lost the pairing")
		}
	}
	if flips.Load() == 0 {
		t.Fatal("the hook never ran")
	}
}

// AC20: a settings read that fails is unknown, never "not paired": status and runs fail,
// and the scheduler audits the failure.
func TestUnreadableSettingsAreNotUnpaired(t *testing.T) {
	a := validService(t)
	pinTestKey(t, a)
	settingsRead = func() error { return errors.New("the database is down") }
	t.Cleanup(func() { settingsRead = func() error { return nil } })
	if _, err := a.Status(); !errors.Is(err, ErrSettingsUnreadable) {
		t.Fatalf("status: %v", err)
	}
	if _, err := a.Run(context.Background()); !errors.Is(err, ErrSettingsUnreadable) {
		t.Fatalf("run: %v", err)
	}
	if _, _, err := a.Export(context.Background()); !errors.Is(err, ErrSettingsUnreadable) {
		t.Fatalf("export: %v", err)
	}
	if ok, err := a.Configured(); ok || !errors.Is(err, ErrSettingsUnreadable) {
		t.Fatalf("configured: %v %v", ok, err)
	}
}

// AC15: three replicas on one due tick make one capsule; a run while another holds the
// operation is refused busy.
func TestConcurrentDueTicksMakeOneCapsule(t *testing.T) {
	a := validService(t)
	t.Setenv("TUNING_FILE", filepath.Join(a.dirs.Config, "TUNING.md"))
	pinTestKey(t, a)
	must(t, a.SetIntervalSeconds(int64(recoveryclient.MinInterval/time.Second)))
	must(t, a.store.SetSetting("backup_last_attempt", time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339)))
	services := []*Service{a, replicaOf(t, a), replicaOf(t, a)}
	var made atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, s := range services {
		wg.Go(func() {
			<-start
			_, err := s.RunIfDue(context.Background(), nil)
			switch {
			case err == nil:
				made.Add(1)
			case errors.Is(err, ErrNotDue), errors.Is(err, recoveryclient.ErrInProgress):
			default:
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	copies, err := recoveryclient.ListLocalCopies(a.cfg.Dir, AppName)
	must(t, err)
	if made.Load() != 1 || len(copies) != 1 {
		t.Fatalf("one due tick on three replicas made %d runs and %d capsules", made.Load(), len(copies))
	}
	release, err := a.lock()
	must(t, err)
	defer release()
	if _, err := services[1].Run(context.Background()); !errors.Is(err, recoveryclient.ErrInProgress) {
		t.Fatalf("Back up now during another run: %v", err)
	}
}

// AC15: losing the backup-run lock cancels the run.
func TestLostRunLockCancelsTheRun(t *testing.T) {
	dbbackend.RequirePostgres(t)
	a := validService(t)
	inside := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.withRunLock(context.Background(), func(ctx context.Context) error {
			close(inside)
			<-ctx.Done()
			return context.Cause(ctx)
		})
	}()
	<-inside
	b := replicaOf(t, a)
	if err := b.withRunLock(context.Background(), func(context.Context) error { return nil }); !errors.Is(err, recoveryclient.ErrInProgress) {
		t.Fatalf("second run while the first holds backup-run: %v", err)
	}
	db, err := dbbackend.OpenFile(filepath.Join(a.dirs.State, "state.db"), "")
	must(t, err)
	defer db.Close()
	var n int
	must(t, db.QueryRow(`SELECT count(pg_terminate_backend(pid)) FROM pg_locks WHERE locktype='advisory' AND granted AND objsubid=2
 AND classid=3 AND objid=hashtext(current_schema() || ':' || 'backup-run')::oid`).Scan(&n))
	select {
	case err := <-done:
		if !errors.Is(err, state.ErrOwnershipLost) {
			t.Fatalf("run ended with %v, want lost ownership", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run kept going after its lock was lost")
	}
}

// Two fixtures on one database never refuse each other's backup-run: its key is scoped to
// the fixture's schema. Back-to-back runs within each are state's TestReleasedLockIsFreeAtOnce.
func TestTwoFixturesRunAtOnceOnOneDatabase(t *testing.T) {
	dbbackend.RequirePostgres(t)
	a, b := validService(t), validService(t)
	inside := make(chan struct{})
	leave := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.withRunLock(context.Background(), func(context.Context) error {
			close(inside)
			<-leave
			return nil
		})
	}()
	<-inside
	err := b.withRunLock(context.Background(), func(context.Context) error { return nil })
	close(leave)
	if err != nil {
		t.Fatalf("second fixture refused while the first runs: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
