package api

import (
	"context"
	"errors"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
)

// MH4: the retained directory subject's reconcile releases the directory fence before
// allocation. With every lock on real fences, allocation must resume on the parent context:
// alone (the released fence's context is cancelled) and under an outer org fence (the
// released directory fence's rank would refuse allocation's domain fence).
func TestFencedReconcilePublishesRetainedNativeSubject(t *testing.T) {
	dbbackend.RequirePostgres(t)
	for _, outer := range []string{"none", "org-fence"} {
		t.Run(outer, func(t *testing.T) {
			s := newNativeRuntimeServer(t)
			s.nativeDomains.SetLookupForTest(func(context.Context, string) ([]string, error) { return nil, errors.New("DNS unavailable") })
			directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "runtime-pending", 1, runtimeDirectoryUser(true)))
			if _, err := s.users.GetBySSOSub("native-runtime-one"); !errors.Is(err, users.ErrNotFound) {
				t.Fatalf("pending provision created a user: %v", err)
			}
			s.nativeDomains.SetLookupForTest(func(context.Context, string) ([]string, error) {
				d, err := s.nativeDomains.Read()
				return []string{d.RecordValue()}, err
			})
			for _, root := range []string{s.configDir, s.stateDir} {
				vol.FenceRootForTest(t, root, state.VolFence(s.stateDir))
			}
			reconcile := func(ctx context.Context) error {
				return s.reconcileNativeSubjectHeld(ctx, "https://idp.example", "native-runtime-one")
			}
			root, err := state.New(s.stateDir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if outer == "org-fence" {
				err = root.WithFences(context.Background(), []state.Fence{{Name: vol.FenceOrg + "org-one", Shared: true}}, reconcile)
			} else {
				err = reconcile(context.Background())
			}
			if err != nil {
				t.Fatal("fenced reconcile failed", err)
			}
			if u, err := s.users.GetBySSOSub("native-runtime-one"); err != nil || u.NativeMailboxSource == "" {
				t.Fatal("fenced reconcile did not publish the mailbox-backed account", err)
			}
		})
	}
}
