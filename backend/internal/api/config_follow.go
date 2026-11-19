package api

import (
	"context"
	"time"

	"github.com/Busnes-app/ky-primitives/netallow"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
)

// followInterval is how often a PostgreSQL replica re-reads what another replica may have
// changed: config.yaml on the shared volume and the stored office networks.
const followInterval = 5 * time.Second

// StartConfigFollower keeps a PostgreSQL replica's settings in step with the others: the
// API's config reloads when config.yaml's identity changes, and the office networks follow
// the database. Every process runs it; SQLite has one API and does nothing.
func (s *Server) StartConfigFollower(ctx context.Context) {
	if s.shared == nil {
		return
	}
	t := time.NewTicker(followInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.followConfig()
		s.followExemptNetworks()
	}
}

// followConfig reloads config.yaml when its size, modification time or inode changed since
// this process last read or wrote it. Values Run derives at start (timezone, label allowlist)
// and the server-owned VAPID material are carried over.
func (s *Server) followConfig() {
	stamp := fileIdentityOf(s.configPath)
	s.cfgMu.RLock()
	same := stamp == s.cfgStamp
	s.cfgMu.RUnlock()
	if same || !stamp.ok {
		return
	}
	next, err := config.Load(s.configPath)
	if err != nil {
		s.logger.Error("config.yaml changed but could not be read; keeping the settings in use", "error", err.Error())
		return
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if next.Timezone == "" {
		next.Timezone = s.cfg.Timezone
	}
	if len(next.Labels.Allowlist) == 0 {
		next.Labels.Allowlist = s.cfg.Labels.Allowlist
	}
	next.Notifications = s.cfg.Notifications
	s.cfg, s.cfgStamp = next, stamp
}

// followExemptNetworks rebuilds the office list when the stored networks changed.
func (s *Server) followExemptNetworks() {
	stored, err := s.storedExemptNetworks()
	if err != nil {
		return // keep the list in use; a later read retries
	}
	if s.exemptStored.Load() != nil && *s.exemptStored.Load() == stored {
		return
	}
	list, err := netallow.Parse(s.exemptNetworksEnv + "," + stored)
	if err != nil {
		list, _ = netallow.Parse(s.exemptNetworksEnv)
	}
	s.exemptNetworks.Store(list)
	s.exemptStored.Store(&stored)
}
