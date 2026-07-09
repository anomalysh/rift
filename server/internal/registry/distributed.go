package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/anomaly-sh/rift/server/internal/config"
	"github.com/anomaly-sh/rift/server/internal/core"
)

// routeKeyNamespace segments subdomain->node leases from anything else sharing
// the Redis instance.
const routeKeyNamespace = "route:"

// leaseRefreshDivisor sets how often a lease is renewed relative to its TTL.
// Renewing at TTL/3 tolerates two consecutive failed renewals before a live
// tunnel would be declared unroutable by its peers.
const leaseRefreshDivisor = 3

// distributed shadows the local map with a per-subdomain Redis lease naming
// the node that holds the agent connection.
//
// The lease is advisory: Postgres remains the authority on who owns a
// subdomain. Redis only answers "which node do I forward to", and a stale
// answer costs one failed forward, not a wrong tunnel.
type distributed struct {
	*Local

	rdb          *redis.Client
	prefix       string
	advertiseURL string
	leaseTTL     time.Duration
	logger       *slog.Logger

	cancel context.CancelFunc
	done   chan struct{}
}

func newDistributed(ctx context.Context, cfg *config.Config, local *Local, logger *slog.Logger) (core.Registry, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("registry: ping redis at %s: %w", cfg.Redis.Addr, err)
	}

	refreshCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	d := &distributed{
		Local:        local,
		rdb:          rdb,
		prefix:       cfg.Redis.Prefix + routeKeyNamespace,
		advertiseURL: cfg.Tunnel.AdvertiseURL,
		// A lease must outlive the heartbeat timeout, or a healthy tunnel
		// would stop being routable from peers between renewals.
		leaseTTL: cfg.Tunnel.HeartbeatTimeout,
		logger:   logger.With(slog.String("component", "registry")),
		cancel:   cancel,
		done:     make(chan struct{}),
	}

	go d.refreshLeases(refreshCtx)
	return d, nil
}

func (d *distributed) key(subdomain string) string { return d.prefix + subdomain }

// Register claims the subdomain locally, then publishes the lease.
func (d *distributed) Register(ctx context.Context, s core.Session) (core.Session, error) {
	displaced, err := d.Local.Register(ctx, s)
	if err != nil {
		return nil, err
	}
	sub := s.Tunnel().Subdomain
	if err := d.rdb.Set(ctx, d.key(sub), d.advertiseURL, d.leaseTTL).Err(); err != nil {
		// The tunnel still works through this node's own ingress; only
		// peer-forwarding is degraded until the next refresh tick.
		d.logger.WarnContext(ctx, "could not publish route lease; peers cannot forward to this node until the next refresh",
			slog.String("subdomain", sub), slog.Any("error", err))
	}
	return displaced, nil
}

// Unregister drops the local entry and, only if this node still owns the
// lease, deletes it. A compare-and-delete avoids erasing the lease a peer
// published for a reconnected agent.
func (d *distributed) Unregister(ctx context.Context, s core.Session) error {
	sub := s.Tunnel().Subdomain

	// Only the session that still holds the subdomain locally may retract the
	// lease; a displaced session must not.
	if cur, ok := d.Local.Lookup(ctx, sub); !ok || cur != s {
		return d.Local.Unregister(ctx, s)
	}
	if err := d.Local.Unregister(ctx, s); err != nil {
		return err
	}
	if err := compareAndDelete.Run(ctx, d.rdb, []string{d.key(sub)}, d.advertiseURL).Err(); err != nil && !errors.Is(err, redis.Nil) {
		d.logger.WarnContext(ctx, "could not retract route lease; it will expire on its own",
			slog.String("subdomain", sub), slog.Any("error", err))
	}
	return nil
}

// compareAndDelete removes the key only when it still holds our value, so a
// slow retraction cannot delete the lease of a newer owner.
var compareAndDelete = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0
`)

// LocatePeer reports the node currently holding subdomain, when it is not us.
func (d *distributed) LocatePeer(ctx context.Context, subdomain string) (string, bool, error) {
	nodeURL, err := d.rdb.Get(ctx, d.key(subdomain)).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("registry: locate %q: %w", subdomain, err)
	}
	if nodeURL == "" || nodeURL == d.advertiseURL {
		return "", false, nil
	}
	return nodeURL, true, nil
}

// refreshLeases renews every locally held lease before it expires.
func (d *distributed) refreshLeases(ctx context.Context) {
	defer close(d.done)

	interval := d.leaseTTL / leaseRefreshDivisor
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, sub := range d.Local.Subdomains() {
				if err := d.rdb.Set(ctx, d.key(sub), d.advertiseURL, d.leaseTTL).Err(); err != nil {
					d.logger.WarnContext(ctx, "could not refresh route lease",
						slog.String("subdomain", sub), slog.Any("error", err))
				}
			}
		}
	}
}

// Close stops lease refreshing and releases the Redis client.
func (d *distributed) Close() error {
	d.cancel()
	<-d.done
	return d.rdb.Close()
}
