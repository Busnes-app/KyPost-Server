package ingress

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

func persistedLimits(t *testing.T, s *Store) Limits {
	t.Helper()
	var l Limits
	if err := s.db.QueryRow("SELECT message_bytes,payload_bytes,records FROM limits WHERE id=1").Scan(&l.MessageBytes, &l.PayloadBytes, &l.Records); err != nil {
		t.Fatal(err)
	}
	return l
}

// A spool created with the earlier 4 MiB / 64 MiB limits is raised by the
// first opener with today's, keeps its held mail, and is never lowered.
func TestHoldingLimitsRaiseOnly(t *testing.T) {
	ctx := context.Background()
	old := Limits{MessageBytes: 4 << 20, PayloadBytes: 64 << 20, Records: 10000}
	dir := filepath.Join(t.TempDir(), "holding")
	s, err := Open(dir, old)
	if err != nil {
		t.Fatal(err)
	}
	route := proofRoute("one@example.test", "one", 1)
	for _, err := range []error{s.SetRoute(ctx, route), s.Bind(ctx, "g", "held", "", route.Address), s.Accept(ctx, "g", "held", "", strings.NewReader("held mail"))} {
		if err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	for _, open := range []func(string, Limits) (*Store, error){OpenExisting, OpenExisting, Open} {
		s, err := open(dir, ReceivingLimits)
		if err != nil {
			t.Fatal(err)
		}
		if got := persistedLimits(t, s); got != ReceivingLimits {
			t.Fatalf("not raised: %+v", got)
		}
		if d, err := s.Get(ctx, "g", "held"); err != nil || string(d.Raw) != "held mail" {
			t.Fatalf("held mail: %+v %v", d, err)
		}
		s.Close()
	}
	lower := ReceivingLimits
	lower.Records--
	for _, open := range []func(string, Limits) (*Store, error){OpenExisting, Open} {
		if _, err := open(dir, lower); !errors.Is(err, errLimits) {
			t.Fatalf("lowered: %v", err)
		}
	}
	mixed := ReceivingLimits
	mixed.Records++
	mixed.PayloadBytes--
	if _, err := OpenExisting(dir, mixed); !errors.Is(err, errLimits) {
		t.Fatalf("mixed change: %v", err)
	}
}

// Growth that would leave less than the drive reserve free is a temporary
// capacity refusal; exact replays still answer.
func TestDriveReserveRefusesGrowth(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	route := proofRoute("one@example.test", "one", 1)
	for _, err := range []error{s.SetRoute(ctx, route), s.Bind(ctx, "g", "kept", "", route.Address), s.Accept(ctx, "g", "kept", "", strings.NewReader("kept mail")), s.Bind(ctx, "g", "next", "", route.Address)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	defer func(orig func(string) (uint64, uint64, error)) { fsutil.DiskSpace = orig }(fsutil.DiskSpace)
	const total = 100 << 30         // reserve: 10 GiB
	free := uint64(10<<30 + 64<<10) // under the reserve plus one write's growth
	fsutil.DiskSpace = func(string) (uint64, uint64, error) { return free, total, nil }
	for _, err := range []error{s.Bind(ctx, "g", "new", "", route.Address), s.Accept(ctx, "g", "next", "", strings.NewReader("new mail"))} {
		if !errors.Is(err, ErrCapacity) || !errors.Is(err, fsutil.ErrDriveReserve) {
			t.Fatalf("growth inside the reserve: %v", err)
		}
	}
	if err := s.Accept(ctx, "g", "kept", "", strings.NewReader("kept mail")); err != nil {
		t.Fatalf("exact replay refused: %v", err)
	}
	free = 10<<30 + 1<<20
	if err := s.Accept(ctx, "g", "next", "", strings.NewReader("new mail")); err != nil {
		t.Fatalf("growth above the reserve: %v", err)
	}
}
