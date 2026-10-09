package backup

import (
	"context"
	"errors"
	"testing"

	"github.com/Busnes-app/ky-primitives/recoveryclient"
)

func TestPairIfMissingPreservesDestinationPinAndToken(t *testing.T) {
	s := validService(t)
	key := pinTestKey(t, s)
	sealer, e := s.loadSealer()
	if e != nil {
		t.Fatal(e)
	}
	if e = s.store.BackupSettingsTransaction(func(st recoveryclient.Settings) error {
		return recoveryclient.StorePairing(st, sealer, "https://recovery.example.com", "retained-test-token")
	}); e != nil {
		t.Fatal(e)
	}
	before, e := s.settings.Get("kyrecovery_token_enc")
	if e != nil {
		t.Fatal(e)
	}
	got, e := s.PairIfMissing(context.Background(), "https://recovery.example.com", "already-spent")
	if e != nil || got.Public.ID() != key.Public().ID() {
		t.Fatalf("rerun failed: %v", e)
	}
	after, e := s.settings.Get("kyrecovery_token_enc")
	if e != nil || before != after {
		t.Fatal("rerun changed token")
	}
	if _, e = s.PairIfMissing(context.Background(), "https://other.example.com", "123456"); e == nil {
		t.Fatal("changed destination passed")
	}
	release, e := s.lock()
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	if _, e = s.PairIfMissing(context.Background(), "https://recovery.example.com", "123456"); !errors.Is(e, recoveryclient.ErrInProgress) {
		t.Fatalf("bypassed backup fence: %v", e)
	}
}
