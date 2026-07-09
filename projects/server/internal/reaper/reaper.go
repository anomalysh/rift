// Package reaper deletes tunnel rows whose agents stopped heartbeating.
//
// A tunnel row is a claim on a subdomain. A process that dies without
// releasing its rows would hold those subdomains forever, so the reaper is
// what makes the claim a lease rather than a lock.
package reaper

import (
	"context"
	"log/slog"
	"time"

	"github.com/anomalysh/rift/projects/server/internal/config"
	"github.com/anomalysh/rift/projects/server/internal/core"
)

// Reaper periodically collects stale tunnels.
type Reaper struct {
	cfg     *config.Config
	logger  *slog.Logger
	tunnels core.TunnelStore
}

// New builds a Reaper.
func New(cfg *config.Config, logger *slog.Logger, tunnels core.TunnelStore) *Reaper {
	return &Reaper{
		cfg:     cfg,
		logger:  logger.With(slog.String("component", "reaper")),
		tunnels: tunnels,
	}
}

// Run blocks until ctx is done, sweeping on every tick.
func (r *Reaper) Run(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.Tunnel.ReaperInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sweep(ctx)
		}
	}
}

// sweep deletes every tunnel that has not heartbeated within the timeout.
//
// A live session on this node whose row is deleted here would be a bug; it
// cannot happen, because the session's own watchdog uses the same timeout and
// closes the connection first. If a row is nonetheless reaped from under a
// session, that session's next heartbeat gets ErrNotFound and it closes.
func (r *Reaper) sweep(ctx context.Context) {
	cutoff := time.Now().Add(-r.cfg.Tunnel.HeartbeatTimeout)

	reaped, err := r.tunnels.DeleteStale(ctx, cutoff)
	if err != nil {
		r.logger.Error("could not reap stale tunnels", slog.Any("error", err))
		return
	}
	for _, t := range reaped {
		r.logger.Info("reaped stale tunnel",
			slog.String("tunnel_id", t.ID),
			slog.String("subdomain", t.Subdomain),
			slog.String("node_id", t.NodeID),
			slog.Time("last_seen_at", t.LastSeenAt))
	}
}
