package users

import (
	"context"
	"errors"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNativePublicationFailureRetryAndOwnership(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	source := "native:" + strings.Repeat("ab", 32)
	issuer := "https://identity.example.test"
	prepare := func() (string, error) {
		raw, err := os.ReadFile(s.path)
		if err != nil {
			return "", err
		}
		if strings.Contains(string(raw), "reserved-one") {
			t.Fatal("user exposed before preparation")
		}
		return source, nil
	}
	failed := errors.New("disk unavailable")
	if _, err := s.PublishPreparedSSOUser(ctx, "reserved-one", "alice", RoleUser, issuer, "one", "alice", "", func() (string, error) { return "", failed }); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	if _, err := s.GetBySSOSub("one"); !errors.Is(err, ErrNotFound) {
		t.Fatal("failed preparation exposed user", err)
	}
	u, err := s.PublishPreparedSSOUser(ctx, "reserved-one", "alice", RoleUser, issuer, "one", "alice", "", prepare)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetBySSOSubIssuer("https://other.test", "one"); !errors.Is(err, ErrNativeAccountConflict) {
		t.Fatal("issuer changed", err)
	}
	if _, err = s.GetBySSOSubIssuer(issuer, "one"); err != nil {
		t.Fatal(err)
	}
	if err = s.LinkSSO(u.ID, "two", "", " "); !errors.Is(err, ErrNativeAccountConflict) {
		t.Fatal("native rebind", err)
	}
	if err = s.LinkSSO(u.ID, "one", "", ""); !errors.Is(err, ErrNativeAccountConflict) {
		t.Fatal("issuerless native relink", err)
	}
	if err = s.UnlinkSSO(u.ID); err != nil {
		t.Fatal(err)
	}
	revoked, err := s.GetBySSOSub("one")
	if err != nil || !revoked.SSOLinkRevoked() || revoked.NativeMailboxIssuer != issuer {
		t.Fatal("owner erased", revoked, err)
	}
	retry, err := s.PublishPreparedSSOUser(ctx, u.ID, "alice", RoleAdmin, issuer, "one", "alice", "", func() (string, error) { return source, nil })
	if err != nil || retry.Role != RoleUser || !retry.SSOLinkRevoked() {
		t.Fatal("retry reauthorized", retry, err)
	}
	if _, err = s.CreateSSOUser("other", RoleUser, "one", "", ""); !errors.Is(err, ErrSSOSubTaken) {
		t.Fatal("duplicate subject", err)
	}
	legacy, err := s.CreateSSOUser("legacy", RoleUser, "legacy", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.LinkSSO(legacy.ID, "one", "", ""); !errors.Is(err, ErrSSOSubTaken) {
		t.Fatal("duplicate link", err)
	}
	if err = s.UnlinkSSO(legacy.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetBySSOSub("legacy"); !errors.Is(err, ErrNotFound) {
		t.Fatal("legacy unlink changed", err)
	}
}

func TestNativePublicationCancelledAfterPreparationAndLock(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	_, err := s.PublishPreparedSSOUser(ctx, "reserved-one", "alice", RoleUser, "https://id.test", "one", "", "", func() (string, error) { cancel(); return "native:" + strings.Repeat("ab", 32), nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal("published cancelled preparation", err)
	}
	if _, err = s.GetBySSOSub("one"); !errors.Is(err, ErrNotFound) {
		t.Fatal("cancelled exposed user", err)
	}
	release, err := fsutil.LockFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel2()
	_, err = s.PublishPreparedSSOUser(ctx2, "reserved-one", "alice", RoleUser, "https://id.test", "one", "", "", func() (string, error) { t.Fatal("prepared while lock blocked"); return "", nil })
	release()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestNativePublicationRacesLegacyAcrossStores(t *testing.T) {
	s := newTestStore(t)
	other := newStore(s.path)
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := s.PublishPreparedSSOUser(context.Background(), "reserved-one", "native-name", RoleUser, "https://id.test", "one", "", "", func() (string, error) { return "native:" + strings.Repeat("ab", 32), nil })
		results <- err
	}()
	go func() { <-start; _, err := other.CreateSSOUser("legacy-name", RoleUser, "one", "", ""); results <- err }()
	close(start)
	success := 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if !errors.Is(err, ErrSSOSubTaken) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatal("two accounts own subject", success)
	}
	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, u := range all {
		if u.SSOSub == "one" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("duplicate durable owner", count)
	}
}
