package api

import (
	"context"
	"errors"

	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
)

// The org fence spans provisioning's three stores and the purge snapshot.
// Ordinary files retain their existing fences. A lost session cancels work;
// the purge also grows its durable snapshot before declaring completion.
func (s *Server) withAccountOrgFence(ctx context.Context, org string, shared bool, work func(context.Context) error) error {
	if !s.multiTenant() || !users.TableMode(s.configDir) {
		return work(ctx)
	}
	if s.globalStore == nil {
		return errors.New("account org fence needs the root state store")
	}
	return s.globalStore.WithFences(ctx, []state.Fence{{Name: vol.FenceOrg + org, Shared: shared}}, work)
}
