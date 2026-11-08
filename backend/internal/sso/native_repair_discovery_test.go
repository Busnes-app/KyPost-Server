//go:build linux

package sso

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestNativeRepairDiscoveryPendingAndIndependentChanges(t *testing.T) {
	ctx := context.Background()
	f := newAddressFixture(t, "one", "two")
	discover := func(want ...string) {
		t.Helper()
		got, err := f.life.NativeDirectoryRepairSubjects(ctx, nativeIssuer, f.accounts)
		slices.Sort(got)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("discovery: got %v, %v; want %v", got, err, want)
		}
	}
	discover()
	// A local deactivation must not become work that replays signed activity.
	if _, err := f.accounts.Deactivate(f.ids["one"]); err != nil {
		t.Fatal(err)
	}
	discover()
	other := NewLifecycleStore(f.config)
	nativeDesired(t, other, "one", "one@example.test", 2, false)
	discover("one")
	// A previously returned subject is no grant after an independent update.
	nativeDesired(t, other, "two", "two@example.test", 2, false)
	if _, err := f.life.AdmitNativeMailbox(ctx, f.root, nativeIssuer, f.ids["two"], f.ids["two"], f.accounts); err == nil {
		t.Fatal("discovery bypassed independent offboarding")
	}
	discover("one", "two")
}

func TestNativeRepairDiscoveryRetainsUnfinishedAndFlooredWork(t *testing.T) {
	for _, kind := range []string{"pending", "failed", "revision", "digest", "unpublished", "release-floor"} {
		t.Run(kind, func(t *testing.T) {
			f := newAddressFixture(t, "one")
			issuer := nativeIssuer
			switch kind {
			case "pending", "failed":
				editAssignment(t, f.life, func(a *NativeAssignment) { a.Status = kind })
			case "revision":
				editAssignment(t, f.life, func(a *NativeAssignment) { a.Revision++ })
			case "digest":
				editAssignment(t, f.life, func(a *NativeAssignment) { a.Digest = "different" })
			case "unpublished":
				nativeDesired(t, f.life, "new", "new@example.test", 1, true)
			case "release-floor":
				if err := f.life.RecordNativeReleaseFloors(issuer, map[string]NativeReleaseFloor{"one": {Revision: 1, Active: false}}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := f.life.NativeDirectoryRepairSubjects(context.Background(), issuer, f.accounts)
			want := "one"
			if kind == "unpublished" {
				want = "new"
			}
			if err != nil || !slices.Equal(got, []string{want}) {
				t.Fatalf("lost %s work: %v, %v", kind, got, err)
			}
		})
	}
}

func TestNativeRepairDiscoveryErrorsAndCancellation(t *testing.T) {
	for _, damage := range []string{"ledger-missing", "ledger-corrupt", "lifecycle-corrupt", "users-corrupt", "cancelled", "contended"} {
		t.Run(damage, func(t *testing.T) {
			f := newAddressFixture(t, "one")
			ctx := context.Background()
			switch damage {
			case "ledger-missing":
				if err := os.Remove(f.life.nativePath()); err != nil {
					t.Fatal(err)
				}
			case "ledger-corrupt", "lifecycle-corrupt", "users-corrupt":
				path := f.life.nativePath()
				if damage == "lifecycle-corrupt" {
					path = f.life.path
				}
				if damage == "users-corrupt" {
					path = filepath.Join(f.config, "users.json")
				}
				if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			case "cancelled", "contended":
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 25*time.Millisecond)
				defer cancel()
				if damage == "cancelled" {
					cancel()
				} else {
					release, err := NewLifecycleStore(f.config).LockDirectory()
					if err != nil {
						t.Fatal(err)
					}
					defer release()
				}
			}
			got, err := f.life.NativeDirectoryRepairSubjects(ctx, nativeIssuer, f.accounts)
			if damage == "users-corrupt" {
				// Lookup errors must reach the normal reconciliation path, not skip.
				if err != nil || !slices.Equal(got, []string{"one"}) {
					t.Fatal(got, err)
				}
			} else if err == nil || len(got) != 0 {
				t.Fatalf("failure became empty success: %v, %v", got, err)
			}
			if damage == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if damage == "contended" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
		})
	}
}
