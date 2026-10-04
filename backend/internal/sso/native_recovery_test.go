package sso

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func recoveryFixture(t *testing.T) (*LifecycleStore, string, SSOSettings, []byte, NativeRecoveryChallenge) {
	t.Helper()
	dir := t.TempDir()
	life := NewLifecycleStore(dir)
	settings := SSOSettings{Enabled: true, IssuerURL: "https://idp.example", ClientID: "kypost"}
	key := []byte(strings.Repeat("k", 32))
	if err := fsutil.PersistJSONFile(filepath.Join(dir, NativeRestoreHoldFile), map[string]any{"version": 1, "epoch": "12345678-1234-4123-8123-123456789abc"}); err != nil {
		t.Fatal(err)
	}
	_, err := life.ApplyDirectory(settings.IssuerURL, syncauth.Event{ID: "old", Type: "user.updated", At: time.Now()}, "subject", 1, "old-digest", true, func() (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	// Retained preparation has never published a local account.
	if err := life.saveNative(nativeAssignments{Version: 1, Accounts: map[string]NativeAssignment{
		directoryKey(settings.IssuerURL, "subject"): {Owner: mailbox.Owner{Issuer: settings.IssuerURL, Subject: "subject", Mailbox: "reserved-mailbox"}, Revision: 1, Digest: "old-digest"},
	}}); err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(key)
	release, err := life.LockDirectory()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	c, err := life.BeginNativeRecoveryHeld(context.Background(), dir, settings, key, nil, "paired-system", hex.EncodeToString(fingerprint[:]))
	if err != nil {
		t.Fatal(err)
	}
	return life, dir, settings, key, c
}
func recoveryPayload(t *testing.T, c NativeRecoveryChallenge, key []byte, mutate func(*nativeRecoveryEvidence)) ([]byte, syncauth.Headers) {
	t.Helper()
	now := time.Now().UTC()
	revision := int64(2)
	e := nativeRecoveryEvidence{Version: 1, Issuer: c.Issuer, SystemID: c.SystemID, Nonce: c.Nonce, IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute), Subjects: []nativeRecoverySubject{{ID: "subject", Revision: &revision, Profile: json.RawMessage(`{"id":"subject","externalId":"subject","active":false,"roles":[]}`)}}}
	if mutate != nil {
		mutate(&e)
	}
	body, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	headers, err := syncauth.Sign(key, e.IssuedAt, "recovery.evidence", c.Nonce, body)
	if err != nil {
		t.Fatal(err)
	}
	return body, headers
}
func TestNativeRecoveryAtomicReceiptAndDirectoryBarriers(t *testing.T) {
	life, root, settings, key, c := recoveryFixture(t)
	body, headers := recoveryPayload(t, c, key, nil)
	release, err := life.LockDirectory()
	if err != nil {
		t.Fatal(err)
	}
	err = life.AcceptNativeRecoveryHeld(context.Background(), root, settings, key, nil, body, headers)
	release()
	if err != nil {
		t.Fatal(err)
	}
	f, err := life.load()
	if err != nil {
		t.Fatal(err)
	}
	if f.RecoveryChallenge != nil || f.RecoveryReceipt == nil || !bytes.Equal(f.RecoveryReceipt.Body, body) || f.RecoveryFloors[directoryKey(settings.IssuerURL, "subject")] != 2 || !f.Directory[directoryKey(settings.IssuerURL, "subject")].Active {
		t.Fatal("missing receipt/floor or ordinary state mutated")
	}
	if !errors.Is(RequireNativeRestoreReleased(root), ErrNativeRestoreHold) {
		t.Fatal("hold released")
	}
	if err = life.AcceptNativeRecoveryHeld(context.Background(), root, settings, key, nil, body, headers); err == nil {
		t.Fatal("evidence replay admitted")
	}
	for _, id := range []string{"old", "other"} {
		called := false
		_, err = life.ApplyDirectory(settings.IssuerURL, syncauth.Event{ID: id, At: time.Now()}, "subject", 1, "old-digest", true, func() (bool, error) { called = true; return false, nil })
		if !errors.Is(err, ErrDirectoryConflict) || called {
			t.Fatal("floor after replay shortcut", id, err)
		}
	}
	before, _ := os.ReadFile(life.path)
	_, err = life.ApplyDirectory(settings.IssuerURL, syncauth.Event{ID: "new", At: time.Now()}, "subject", 3, "new-digest", false, func() (bool, error) { return false, errors.New("injected account failure") })
	if err == nil {
		t.Fatal("callback failure ignored")
	}
	after, _ := os.ReadFile(life.path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed callback mutated receipt")
	}
	_, err = life.ApplyDirectory(settings.IssuerURL, syncauth.Event{ID: "new", At: time.Now()}, "subject", 3, "new-digest", false, func() (bool, error) { return true, nil })
	if err != nil {
		t.Fatal(err)
	}
	f, err = life.load()
	if err != nil || f.RecoveryReceipt != nil || f.RecoveryFloors[directoryKey(settings.IssuerURL, "subject")] != 2 {
		t.Fatal("new evidence failed to invalidate receipt/preserve floor", err)
	}
}
func TestNativeRecoveryRejectsUnqualifiedEvidenceWithoutMutation(t *testing.T) {
	cases := map[string]func(*nativeRecoveryEvidence){
		"issuer": func(e *nativeRecoveryEvidence) { e.Issuer = "https://foreign.example" }, "system": func(e *nativeRecoveryEvidence) { e.SystemID = "foreign" }, "nonce": func(e *nativeRecoveryEvidence) { e.Nonce = strings.Repeat("b", 64) }, "version": func(e *nativeRecoveryEvidence) { e.Version = 2 },
		"unknown revision": func(e *nativeRecoveryEvidence) { e.Subjects[0].Revision = nil }, "lower revision": func(e *nativeRecoveryEvidence) { r := int64(0); e.Subjects[0].Revision = &r }, "missing subject": func(e *nativeRecoveryEvidence) { e.Subjects = nil }, "foreign subject": func(e *nativeRecoveryEvidence) { e.Subjects[0].ID = "foreign" }, "duplicate": func(e *nativeRecoveryEvidence) { e.Subjects = append(e.Subjects, e.Subjects[0]) },
		"profile identity": func(e *nativeRecoveryEvidence) {
			e.Subjects[0].Profile = json.RawMessage(`{"id":"foreign","externalId":"foreign","active":false,"roles":[]}`)
		}, "expired": func(e *nativeRecoveryEvidence) {
			e.IssuedAt = time.Now().Add(-10 * time.Minute)
			e.ExpiresAt = e.IssuedAt.Add(5 * time.Minute)
		}, "long lifetime": func(e *nativeRecoveryEvidence) { e.ExpiresAt = e.IssuedAt.Add(6 * time.Minute) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			life, root, settings, key, c := recoveryFixture(t)
			body, h := recoveryPayload(t, c, key, mutate)
			before, _ := os.ReadFile(life.path)
			release, err := life.LockDirectory()
			if err != nil {
				t.Fatal(err)
			}
			err = life.AcceptNativeRecoveryHeld(context.Background(), root, settings, key, nil, body, h)
			release()
			if err == nil {
				t.Fatal("unqualified evidence accepted")
			}
			after, _ := os.ReadFile(life.path)
			if !bytes.Equal(before, after) {
				t.Fatal("refusal mutated lifecycle")
			}
		})
	}
}
func TestNativeRecoveryRetainsFloorSubjectsAndRejectsEpochChange(t *testing.T) {
	life, root, settings, key, c := recoveryFixture(t)
	f, err := life.load()
	if err != nil {
		t.Fatal(err)
	}
	f.RecoveryFloors = map[string]int64{directoryKey(settings.IssuerURL, "offboarded"): 9}
	if err = fsutil.PersistJSONFile(life.path, f); err != nil {
		t.Fatal(err)
	}
	current, revisions, _, err := life.nativeRecoveryInputs(root, settings, key, nil)
	if err != nil || strings.Join(current.Subjects, ",") != "offboarded,subject" || revisions["offboarded"] != 9 {
		t.Fatal("floor-only subject omitted", err)
	}
	body, h := recoveryPayload(t, c, key, nil)
	if err = fsutil.PersistJSONFile(filepath.Join(root, NativeRestoreHoldFile), map[string]any{"version": 1, "epoch": "12345678-1234-4123-8123-123456789abd"}); err != nil {
		t.Fatal(err)
	}
	if err = life.AcceptNativeRecoveryHeld(context.Background(), root, settings, key, nil, body, h); err == nil {
		t.Fatal("wrong epoch admitted")
	}
}

func TestNativeRecoveryRefusesPartialNativeAccountMarkers(t *testing.T) {
	life, root, settings, key, _ := recoveryFixture(t)
	for _, u := range []users.User{{ID: "orphan", NativeMailboxSource: "native:orphan"}, {ID: "orphan", NativeMailboxIssuer: settings.IssuerURL}, {ID: "orphan", NativeMailboxIssuer: settings.IssuerURL, NativeMailboxSource: "native:orphan"}, {ID: "orphan", NativeMailboxSource: "native:orphan", SSOSub: "subject"}} {
		if _, _, _, err := life.nativeRecoveryInputs(root, settings, key, []users.User{u}); err == nil {
			t.Fatal("partial native markers omitted")
		}
	}
}

func TestNativeRecoveryRefusesUnusableHoldAndCancelledCommit(t *testing.T) {
	life, root, settings, key, c := recoveryFixture(t)
	body, h := recoveryPayload(t, c, key, nil)
	before, _ := os.ReadFile(life.path)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := life.AcceptNativeRecoveryHeld(ctx, root, settings, key, nil, body, h); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled commit admitted", err)
	}
	after, _ := os.ReadFile(life.path)
	if !bytes.Equal(before, after) {
		t.Fatal("cancelled commit mutated lifecycle")
	}
	for _, hold := range []string{`{"version":1}`, `{"version":1,"epoch":""}`, `{"version":2,"epoch":"12345678-1234-4123-8123-123456789abc"}`, `{"version":1,"epoch":"historical"}`} {
		if err := os.WriteFile(filepath.Join(root, NativeRestoreHoldFile), []byte(hold), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := life.nativeRecoveryInputs(root, settings, key, nil); err == nil {
			t.Fatal("unusable epoch accepted", hold)
		}
	}
}

func TestNativeRecoveryPublishedFloorSafety(t *testing.T) {
	for _, byID := range []bool{false, true} {
		for _, dominated := range []bool{false, true} {
			t.Run(fmt.Sprintf("mailboxID=%t/dominated=%t", byID, dominated), func(t *testing.T) {
				life, root, settings, key, _ := recoveryFixture(t)
				f, err := life.load()
				if err != nil {
					t.Fatal(err)
				}
				k := directoryKey(settings.IssuerURL, "subject")
				f.RecoveryFloors = map[string]int64{k: 2}
				if dominated {
					d := f.Directory[k]
					d.Revision = 3
					f.Directory[k] = d
				}
				if err = fsutil.PersistJSONFile(life.path, f); err != nil {
					t.Fatal(err)
				}
				u := users.User{ID: "published", SSOSub: "subject"}
				if byID {
					u = users.User{ID: "reserved-mailbox"}
				}
				before, err := os.ReadFile(life.path)
				if err != nil {
					t.Fatal(err)
				}
				_, _, eligible, err := life.nativeRecoveryInputs(root, settings, key, []users.User{u})
				if dominated {
					if err != nil || eligible["subject"] {
						t.Fatal("dominated floor not harmless", err)
					}
				} else if !errors.Is(err, ErrNativeRecovery) {
					t.Fatal("unsafe published floor admitted", err)
				}
				after, err := os.ReadFile(life.path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Fatal("qualification changed barriers")
				}
			})
		}
	}
}

func TestNativeRecoveryPublicationAboveFloorAndRetry(t *testing.T) {
	life, root, settings, key, c := recoveryFixture(t)
	body, headers := recoveryPayload(t, c, key, nil)
	if err := life.AcceptNativeRecoveryHeld(context.Background(), root, settings, key, nil, body, headers); err != nil {
		t.Fatal(err)
	}
	accounts := []users.User{}
	ev := syncauth.Event{ID: "publication", Type: "user.updated", At: time.Now()}
	_, err := life.ApplyDirectory(settings.IssuerURL, ev, "subject", 3, "published", true, func() (bool, error) {
		// Model account publication followed by failed directory persistence.
		accounts = []users.User{{ID: "reserved-mailbox", SSOSub: "subject"}}
		return false, errors.New("injected publication failure")
	})
	if err == nil {
		t.Fatal("publication failure ignored")
	}
	if _, _, _, err = life.nativeRecoveryInputs(root, settings, key, accounts); !errors.Is(err, ErrNativeRecovery) {
		t.Fatal("partial publication qualified", err)
	}
	if _, err = life.ApplyDirectory(settings.IssuerURL, ev, "subject", 3, "published", true, func() (bool, error) { return false, nil }); err != nil {
		t.Fatal("newer publication retry blocked", err)
	}
	c, err = life.BeginNativeRecoveryHeld(context.Background(), root, settings, key, accounts, c.SystemID, c.KeyFingerprint)
	if err != nil {
		t.Fatal("published owner with dominated floor refused", err)
	}
	body, headers = recoveryPayload(t, c, key, func(e *nativeRecoveryEvidence) { r := int64(4); e.Subjects[0].Revision = &r })
	if err = life.AcceptNativeRecoveryHeld(context.Background(), root, settings, key, accounts, body, headers); err != nil {
		t.Fatal(err)
	}
	f, err := life.load()
	if err != nil {
		t.Fatal(err)
	}
	if f.RecoveryFloors[directoryKey(settings.IssuerURL, "subject")] != 2 {
		t.Fatal("published floor advanced")
	}
	called := false
	if _, err = life.ApplyDirectory(settings.IssuerURL, syncauth.Event{ID: "offboard", Type: "user.updated", At: time.Now()}, "subject", 4, "offboard", false, func() (bool, error) { called = true; return true, nil }); err != nil || !called {
		t.Fatal("queued observed-revision loss blocked", err)
	}
	f, err = life.load()
	if err != nil || f.RecoveryReceipt != nil {
		t.Fatal("loss did not invalidate receipt", err)
	}
}

func TestNativeRecoveryUnlinkedPublicationInvalidatesChallenge(t *testing.T) {
	life, root, settings, key, c := recoveryFixture(t)
	body, headers := recoveryPayload(t, c, key, nil)
	before, err := os.ReadFile(life.path)
	if err != nil {
		t.Fatal(err)
	}
	if err = life.AcceptNativeRecoveryHeld(context.Background(), root, settings, key, []users.User{{ID: "reserved-mailbox"}}, body, headers); !errors.Is(err, ErrNativeRecovery) {
		t.Fatal("publication under reserved ID did not invalidate challenge", err)
	}
	after, err := os.ReadFile(life.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("refusal changed lifecycle")
	}
}
