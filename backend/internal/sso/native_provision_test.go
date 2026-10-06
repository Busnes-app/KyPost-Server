//go:build linux

package sso

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

const nativeIssuer = "https://identity.example.test"

var nativeLimits = mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100}

func nativeDesired(t *testing.T, s *LifecycleStore, sub, address string, revision int, active bool) {
	t.Helper()
	raw := fmt.Sprintf(`{"schemas":[%q],"id":%q,"externalId":%q,"userName":%q,"active":%t,"emails":[{"value":%q,"primary":true}],"meta":{"version":%q}}`, scimUserSchema, sub, sub, sub, active, address, fmt.Sprintf(`W/"%d"`, revision))
	var u DirectoryUser
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: fmt.Sprintf("%s-%d", sub, revision), Type: "user.updated", At: time.Now()}
	if _, err := s.ApplyDirectoryUser(nativeIssuer, ev, u, EventDigest(ev.Type, []byte(raw)), func() (bool, error) { return !active, nil }); err != nil {
		t.Fatal(err)
	}
}

func TestNativeProvisioningReconcilesAndRetainsOwnership(t *testing.T) {
	config, root := t.TempDir(), t.TempDir()
	s := NewLifecycleStore(config)
	nativeDesired(t, s, "one", "One@EXAMPLE.TEST", 1, true)
	first, err := s.ReconcileNativeMailbox(root, nativeIssuer, "one", "local-one", "example.test", nativeLimits)
	if err != nil || first.Status != "applied" || first.Source == "" || first.Address != "one@example.test" {
		t.Fatalf("initial %+v %v", first, err)
	}
	owner := first.Owner
	store, err := mailbox.Open(filepath.Join(root, "users", owner.Mailbox, "mailbox"), owner, nativeLimits)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: sender@example.test\r\nSubject: retained\r\n\r\nexact mail")
	id, err := store.Append(context.Background(), "INBOX", bytes.NewReader(raw), false)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	for _, active := range []bool{false, true} {
		revision := 2
		if active {
			revision = 3
		}
		nativeDesired(t, s, "one", "one@example.test", revision, active)
		s = NewLifecycleStore(config)
		next, err := s.ReconcileNativeMailbox(root, nativeIssuer, "one", "local-one", "example.test", nativeLimits)
		if err != nil || next.Source != first.Source || next.DesiredActive != active || next.Status != "applied" || next.Revision != int64(revision) {
			t.Fatalf("lifecycle %+v %v", next, err)
		}
	}
	nativeDesired(t, s, "two", "ONE@example.test", 1, true)
	if _, err = s.ReconcileNativeMailbox(root, nativeIssuer, "two", "local-two", "example.test", nativeLimits); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("address reassigned", err)
	}
	nativeDesired(t, s, "three", "three@example.test", 1, true)
	if _, err = s.ReconcileNativeMailbox(root, nativeIssuer, "three", "local-one", "example.test", nativeLimits); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("local account reassigned", err)
	}
	nativeDesired(t, s, "one", "renamed@example.test", 4, true)
	if _, err = s.ReconcileNativeMailbox(root, nativeIssuer, "one", "local-one", "example.test", nativeLimits); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("address changed implicitly", err)
	}
	failed, found, err := s.NativeAssignment(nativeIssuer, "one")
	if err != nil || !found || failed.Status != "failed" || failed.Revision != 4 || failed.Address != first.Address || failed.Source != first.Source {
		t.Fatal("current failed status missing", failed, err)
	}
	nativeDesired(t, s, "one", "one@example.test", 5, true)
	if repaired, err := s.ReconcileNativeMailbox(root, nativeIssuer, "one", "local-one", "example.test", nativeLimits); err != nil || repaired.Source != first.Source || repaired.Status != "applied" {
		t.Fatal("failed desired-state repair", repaired, err)
	}
	for _, input := range []struct {
		id, root string
		limits   mailbox.Limits
	}{{"other-local", root, nativeLimits}, {"local-one", t.TempDir(), nativeLimits}, {"local-one", root, mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 8 << 20, Records: 100}}} {
		if _, err := s.ReconcileNativeMailbox(input.root, nativeIssuer, "one", input.id, "example.test", input.limits); !errors.Is(err, ErrNativeProvisioning) {
			t.Fatal("immutable assignment changed", err)
		}
	}
	store, err = mailbox.Open(filepath.Join(root, "users", owner.Mailbox, "mailbox"), owner, nativeLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.Raw(context.Background(), "INBOX", id)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("lifecycle altered mail", err)
	}
}

func TestNativeProvisioningRefusesUnverifiedAmbiguousAndPartialState(t *testing.T) {
	for _, mode := range []string{"unknown", "legacy-resource", "inactive", "no-primary", "two-primary", "outside-domain", "display-name", "quoted", "unicode", "invalid-domain", "traversal", "invalid-limits", "legacy-state", "missing-applied", "corrupt-registry"} {
		t.Run(mode, func(t *testing.T) {
			config, root := t.TempDir(), t.TempDir()
			s := NewLifecycleStore(config)
			address := "one@example.test"
			active := true
			switch mode {
			case "outside-domain":
				address = "one@other.test"
			case "display-name":
				address = "One <one@example.test>"
			case "quoted":
				address = `"one two"@example.test`
			case "unicode":
				address = "ö@example.test"
			case "inactive":
				active = false
			}
			if mode != "unknown" {
				nativeDesired(t, s, "one", address, 1, active)
			}
			if mode == "legacy-resource" {
				_, err := s.ApplyDirectory(nativeIssuer, syncauth.Event{ID: "legacy", Type: "user.updated", At: time.Now()}, "one", 2, "digest", true, func() (bool, error) { return false, nil })
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "no-primary" || mode == "two-primary" {
				f, err := s.load()
				if err != nil {
					t.Fatal(err)
				}
				d := f.Directory[directoryKey(nativeIssuer, "one")]
				if mode == "no-primary" {
					d.Resource.Emails[0].Primary = false
				} else {
					d.Resource.Emails = append(d.Resource.Emails, d.Resource.Emails[0])
				}
				f.Directory[directoryKey(nativeIssuer, "one")] = d
				if err := fsutil.PersistJSONFile(s.path, f); err != nil {
					t.Fatal(err)
				}
			}
			id, domain, limits := "local-one", "example.test", nativeLimits
			switch mode {
			case "invalid-domain":
				domain = "EXAMPLE.TEST"
			case "traversal":
				id = "../escape"
			case "invalid-limits":
				limits.Records = 0
			}
			target := filepath.Join(root, "users", id)
			if mode == "legacy-state" {
				st, err := state.New(target)
				if err != nil {
					t.Fatal(err)
				}
				_ = st.Close()
			}
			if mode == "missing-applied" {
				if _, err := s.ReconcileNativeMailbox(root, nativeIssuer, "one", id, domain, limits); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(target); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "corrupt-registry" {
				if err := os.WriteFile(s.nativePath(), []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			a, err := s.ReconcileNativeMailbox(root, nativeIssuer, "one", id, domain, limits)
			if err == nil {
				t.Fatalf("invalid input/state admitted: %+v", a)
			}
			if mode == "legacy-state" || mode == "missing-applied" {
				persisted, found, e := s.NativeAssignment(nativeIssuer, "one")
				if e != nil || !found || persisted.Status != "failed" {
					t.Fatalf("failure not retained %+v %v", persisted, e)
				}
			}
			if mode == "missing-applied" {
				if _, e := os.Stat(target); !errors.Is(e, os.ErrNotExist) {
					t.Fatal("missing applied storage recreated", e)
				}
			}
		})
	}
}

func TestNativeProvisioningConcurrentReservationsAndRevocation(t *testing.T) {
	config, root := t.TempDir(), t.TempDir()
	s := NewLifecycleStore(config)
	for _, sub := range []string{"one", "two"} {
		nativeDesired(t, s, sub, "same@example.test", 1, true)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, sub := range []string{"one", "two"} {
		wg.Go(func() {
			_, e := NewLifecycleStore(config).ReconcileNativeMailbox(root, nativeIssuer, sub, "local-"+sub, "example.test", nativeLimits)
			results <- e
		})
	}
	wg.Wait()
	close(results)
	wins := 0
	for e := range results {
		if e == nil {
			wins++
		} else if !errors.Is(e, ErrNativeProvisioning) {
			t.Fatal(e)
		}
	}
	if wins != 1 {
		t.Fatal("reservation winners", wins)
	}
	// Hold a real preparation between durable reservation and publication. A
	// directory update must wait; it cannot land halfway through the commit.
	nativeDesired(t, s, "three", "three@example.test", 1, true)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, e := s.reconcileNativeMailbox(root, nativeIssuer, "three", "local-three", "example.test", nativeLimits, func(root string, o mailbox.Owner, a string, l mailbox.Limits) (string, error) {
			close(entered)
			<-release
			return mailbox.PrepareAccount(root, o, a, l)
		})
		done <- e
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatal("preparation stopped before fence check", err)
	}
	revoked := make(chan struct{})
	go func() { nativeDesired(t, s, "three", "three@example.test", 2, false); close(revoked) }()
	select {
	case <-revoked:
		t.Fatal("deactivation crossed preparation fence")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	<-revoked
	a, e := s.ReconcileNativeMailbox(root, nativeIssuer, "three", "local-three", "example.test", nativeLimits)
	if e != nil || a.DesiredActive || a.Revision != 2 {
		t.Fatal("deactivation not reconciled", a, e)
	}
}

func TestNativeProvisioningCrashHelper(t *testing.T) {
	config := os.Getenv("KYPOST_NATIVE_PROVISION_CONFIG")
	if config == "" {
		return
	}
	root, mode := os.Getenv("KYPOST_NATIVE_PROVISION_ROOT"), os.Getenv("KYPOST_NATIVE_PROVISION_MODE")
	s := NewLifecycleStore(config)
	_, err := s.reconcileNativeMailbox(root, nativeIssuer, "one", "local-one", "example.test", nativeLimits, func(root string, o mailbox.Owner, address string, l mailbox.Limits) (string, error) {
		if mode == "after" {
			if _, e := mailbox.PrepareAccount(root, o, address, l); e != nil {
				return "", e
			}
		}
		p, e := os.FindProcess(os.Getpid())
		if e != nil {
			return "", e
		}
		return "", p.Kill()
	})
	t.Fatalf("child not killed: %v", err)
}

func TestNativeProvisioningKilledReservationAndLostAcknowledgement(t *testing.T) {
	for _, mode := range []string{"before", "after"} {
		t.Run(mode, func(t *testing.T) {
			config, root := t.TempDir(), t.TempDir()
			s := NewLifecycleStore(config)
			nativeDesired(t, s, "one", "one@example.test", 1, true)
			cmd := exec.Command(os.Args[0], "-test.run=^TestNativeProvisioningCrashHelper$")
			cmd.Env = append(os.Environ(), "KYPOST_NATIVE_PROVISION_CONFIG="+config, "KYPOST_NATIVE_PROVISION_ROOT="+root, "KYPOST_NATIVE_PROVISION_MODE="+mode)
			output, err := cmd.CombinedOutput()
			var killed *exec.ExitError
			if !errors.As(err, &killed) {
				t.Fatalf("not killed: %v %s", err, output)
			}
			status, ok := killed.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("failed before boundary: %v %s", err, output)
			}
			s = NewLifecycleStore(config)
			pending, found, err := s.NativeAssignment(nativeIssuer, "one")
			if err != nil || !found || pending.Status != "pending" || pending.Source != "" {
				t.Fatal("reservation not durable", pending, err)
			}
			nativeDesired(t, s, "two", "one@example.test", 1, true)
			if _, err = s.ReconcileNativeMailbox(root, nativeIssuer, "two", "local-two", "example.test", nativeLimits); !errors.Is(err, ErrNativeProvisioning) {
				t.Fatal("pending address not reserved", err)
			}
			var source string
			if mode == "after" {
				source, err = mailbox.ValidatePreparedAccount(root, pending.Owner, pending.Address, nativeLimits)
				if err != nil {
					t.Fatal(err)
				}
			}
			applied, err := s.ReconcileNativeMailbox(root, nativeIssuer, "one", "local-one", "example.test", nativeLimits)
			if err != nil || applied.Status != "applied" || applied.Source == "" || source != "" && applied.Source != source {
				t.Fatal("replay changed owner/source", applied, err)
			}
		})
	}
}

func TestNativeProvisioningRefusesMissingLedgerAndRestoredDirectory(t *testing.T) {
	config, root := t.TempDir(), t.TempDir()
	s := NewLifecycleStore(config)
	nativeDesired(t, s, "one", "one@example.test", 1, true)
	lifecycleBefore, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReconcileNativeMailbox(root, nativeIssuer, "one", "local-one", "example.test", nativeLimits); err != nil {
		t.Fatal(err)
	}
	ledger, err := os.ReadFile(s.nativePath())
	if err != nil {
		t.Fatal(err)
	}
	nativeDesired(t, s, "two", "one@example.test", 1, true)
	if err = os.Remove(s.nativePath()); err != nil {
		t.Fatal(err)
	}
	// An active everyday revision may clear legacyMixedUse, so a lost ledger
	// fails it for retry; it must not be recorded past the flag.
	raw := fmt.Sprintf(`{"schemas":[%q],"id":"two","externalId":"two","userName":"two","active":true,"emails":[{"value":"one@example.test","primary":true}],"meta":{"version":"W/\"2\""}}`, scimUserSchema)
	var update DirectoryUser
	if err = json.Unmarshal([]byte(raw), &update); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: "two-2", Type: "user.updated", At: time.Now()}
	if _, err = s.ApplyDirectoryUser(nativeIssuer, ev, update, EventDigest(ev.Type, []byte(raw)), func() (bool, error) { return false, nil }); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("directory revision recorded over a lost ledger", err)
	}
	if _, err = s.ReconcileNativeMailbox(root, nativeIssuer, "two", "local-two", "example.test", nativeLimits); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("lost ledger freed reservation", err)
	}
	if _, _, err = s.NativeAssignment(nativeIssuer, "one"); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("missing ledger reported empty", err)
	}
	if err = os.WriteFile(s.nativePath(), ledger, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.path, lifecycleBefore, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReconcileNativeMailbox(root, nativeIssuer, "one", "local-one", "example.test", nativeLimits); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("restored unfenced directory adopted ledger", err)
	}
}
