// Package registry resolves a subdomain to the session serving it.
//
// Single-node deployments use only the in-memory map. When Redis is enabled
// the map is shadowed by a short-lived key per subdomain naming the node that
// holds it, so an ingress on one node can forward to the agent attached to
// another.
package registry

import (
	"context"
	"log/slog"

	"github.com/anomaly-sh/rift/server/internal/config"
	"github.com/anomaly-sh/rift/server/internal/core"
)

// New builds the registry implied by cfg: local-only, or Redis-backed.
func New(ctx context.Context, cfg *config.Config, logger *slog.Logger) (core.Registry, error) {
	local := NewLocal()
	if !cfg.Redis.Enabled {
		return local, nil
	}
	return newDistributed(ctx, cfg, local, logger)
}
