package state

import (
	"context"
	"errors"
	"testing"
)

func TestAgentTokenStore(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tok := func(id, user string, created int64) AgentToken {
		return AgentToken{ID: id, UserID: user, Label: "agent", Hash: "h", Scope: AgentScopeRead, CreatedAt: created}
	}
	if err := s.CreateAgentToken(ctx, AgentToken{ID: "x", UserID: "u", Scope: "dav"}, 5); err == nil {
		t.Error("a dav-scoped agent token was stored")
	}
	for i, id := range []string{"a", "b"} {
		if err := s.CreateAgentToken(ctx, tok(id, "u1", int64(100+i)), 2); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateAgentToken(ctx, tok("c", "u1", 102), 2); !errors.Is(err, ErrAgentTokenLimit) {
		t.Errorf("third token past a cap of 2: %v", err)
	}
	if err := s.CreateAgentToken(ctx, tok("d", "u2", 50), 2); err != nil {
		t.Errorf("the cap is per user: %v", err)
	}
	if err := s.SetAgentTokenScope(ctx, "u2", "a", AgentScopeOrganize); !errors.Is(err, ErrAgentTokenNotFound) {
		t.Errorf("another user's token changed: %v", err)
	}
	if err := s.DeleteAgentToken(ctx, "u2", "a"); !errors.Is(err, ErrAgentTokenNotFound) {
		t.Errorf("another user's token deleted: %v", err)
	}
	if err := s.TouchAgentToken(ctx, "a", 1000); err != nil {
		t.Fatal(err)
	}
	gone, err := s.ExpireIdleAgentTokens(ctx, 500, "")
	if err != nil || len(gone) != 2 {
		t.Fatalf("expired %+v %v, want b and d (a was used at 1000)", gone, err)
	}
	if list, _ := s.AgentTokens(ctx, "u1"); len(list) != 1 || list[0].ID != "a" || list[0].LastActive() != 1000 {
		t.Errorf("left %+v", list)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := FenceRestoredAgentTokens(dir); err != nil {
		t.Fatal(err)
	}
	s, err = New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if list, _ := s.AgentTokens(ctx, "u1"); len(list) != 0 {
		t.Errorf("a restore kept %d agent tokens", len(list))
	}
	if err := FenceRestoredAgentTokens(t.TempDir()); err != nil {
		t.Errorf("a restore with no state.db: %v", err)
	}
}
