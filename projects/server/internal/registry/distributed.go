package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/anomalysh/rift/projects/server/internal/config"
	"github.com/anomalysh/rift/projects/server/internal/core"
)

// routeKeyNamespace segments subdomain->node leases from anything else sharing
// the Redis instance.
const routeKeyNamespace = "route:"

// leaseRefreshDivisor sets how often a lease is renewed relative to its TTL.
// Renewing at TTL/3 tolerates two consecutive failed renewals before a live
// tunnel would be declared unroutable by its peers.
const leaseRefreshDivisor = 3

// leaseStore is the shared key-value store holding route leases. Redis in
// production; the interface exists so the lease logic can be tested without
// one.
type leaseStore interface {
	// set writes key=value with a TTL.
	set(ctx context.Context, key, value string, ttl time.Duration) error
	// get returns the value at key; ok is false when there is none.
	get(ctx context.Context, key string) (value string, ok bool, err error)
	// compareAndDelete deletes key only while it still holds value.
	compareAndDelete(ctx context.Context, key, value string) error
	close() error
}

// distributed shadows the local map with a per-subdomain Redis lease naming
// the node that holds the agent connection.
//
// The lease is advisory: Postgres remains the authority on who owns a
// subdomain. Redis only answers "which node do I forward to", and a stale
// answer costs one failed forward, not a wrong tunnel.
type distributed struct {
	*Local

	leases       leaseStore
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

	// A lease must outlive the heartbeat timeout, or a healthy tunnel would
	// stop being routable from peers between renewals.
	d := newDistributedWith(local, redisLeases{rdb}, cfg.Redis.Prefix+routeKeyNamespace,
		cfg.Tunnel.AdvertiseURL, cfg.Tunnel.HeartbeatTimeout, logger)
	refreshCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	d.cancel = cancel
	d.done = make(chan struct{})
	go d.refreshLeases(refreshCtx)
	return d, nil
}

// newDistributedWith builds the registry over any lease store. It starts no
// refresher; newDistributed does.
func newDistributedWith(local *Local, leases leaseStore, prefix, advertiseURL string, ttl time.Duration, logger *slog.Logger) *distributed {
	done := make(chan struct{})
	close(done) // until a refresher runs, Close has nothing to wait for
	return &distributed{
		Local:        local,
		leases:       leases,
		prefix:       prefix,
		advertiseURL: advertiseURL,
		leaseTTL:     ttl,
		logger:       logger.With(slog.String("component", "registry")),
		cancel:       func() {},
		done:         done,
	}
}

func (d *distributed) key(subdomain string) string { return d.prefix + subdomain }

// publish (re)writes subdomain's lease naming this node.
func (d *distributed) publish(ctx context.Context, subdomain string) error {
	return d.leases.set(ctx, d.key(subdomain), d.advertiseURL, d.leaseTTL)
}

// Register claims the subdomain locally, then publishes the lease.
func (d *distributed) Register(ctx context.Context, s core.Session) (core.Session, error) {
	displaced, err := d.Local.Register(ctx, s)
	if err != nil {
		return nil, err
	}
	sub := s.Tunnel().Subdomain
	if err := d.publish(ctx, sub); err != nil {
		// The tunnel still works through this node's own ingress; only
		// peer-forwarding is degraded until the next refresh tick.
		d.logger.WarnContext(ctx, "could not publish route lease; peers cannot forward to this node until the next refresh",
			slog.String("subdomain", sub), slog.Any("error", err))
	}
	return displaced, nil
}

// Unregister drops the local entry and, only if s still held the subdomain,
// retracts the lease with a compare-and-delete, so a lease a peer published
// for a reconnected agent survives.
//
// A reconnect to this node is the subtle case: its lease names this node too,
// so the compare cannot tell the two sessions apart. The local removal is
// therefore check-and-delete in one step, and if the subdomain is registered
// here again by the time the lease is gone, the lease is put back.
func (d *distributed) Unregister(ctx context.Context, s core.Session) error {
	if !d.Local.unregister(s) {
		// A displaced session must not retract its replacement's lease.
		return nil
	}
	sub := s.Tunnel().Subdomain
	if err := d.leases.compareAndDelete(ctx, d.key(sub), d.advertiseURL); err != nil {
		d.logger.WarnContext(ctx, "could not retract route lease; it will expire on its own",
			slog.String("subdomain", sub), slog.Any("error", err))
		return nil
	}
	if _, again := d.Local.Lookup(ctx, sub); again {
		if err := d.publish(ctx, sub); err != nil {
			d.logger.WarnContext(ctx, "could not restore route lease after a reconnect; the next refresh will",
				slog.String("subdomain", sub), slog.Any("error", err))
		}
	}
	return nil
}

// LocatePeer reports the node currently holding subdomain, when it is not us.
func (d *distributed) LocatePeer(ctx context.Context, subdomain string) (string, bool, error) {
	nodeURL, ok, err := d.leases.get(ctx, d.key(subdomain))
	if err != nil {
		return "", false, fmt.Errorf("registry: locate %q: %w", subdomain, err)
	}
	if !ok || nodeURL == "" || nodeURL == d.advertiseURL {
		return "", false, nil
	}
	return nodeURL, true, nil
}

// InvalidatePeer removes subdomain's lease only if it still names nodeURL, so
// forwarding to a node that has died drops the stale belief without racing a
// reconnected agent that has already republished the lease elsewhere.
func (d *distributed) InvalidatePeer(ctx context.Context, subdomain, nodeURL string) error {
	// Never invalidate a subdomain this node itself serves; that is not a stale
	// peer lease, it is our own.
	if _, ok := d.Local.Lookup(ctx, subdomain); ok {
		return nil
	}
	if err := d.leases.compareAndDelete(ctx, d.key(subdomain), nodeURL); err != nil {
		return fmt.Errorf("registry: invalidate %q: %w", subdomain, err)
	}
	return nil
}

// refreshLeases renews every locally held lease before it expires. It closes
// d.done when it stops.
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
				if err := d.publish(ctx, sub); err != nil {
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
	return d.leases.close()
}

// redisLeases is the production leaseStore.
type redisLeases struct{ rdb *redis.Client }

func (r redisLeases) set(ctx context.Context, key, value string, ttl time.Duration) error {
	return r.rdb.Set(ctx, key, value, ttl).Err()
}

func (r redisLeases) get(ctx context.Context, key string) (string, bool, error) {
	v, err := r.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// compareAndDeleteScript removes the key only when it still holds our value,
// so a slow retraction cannot delete the lease of a newer owner.
var compareAndDeleteScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0
`)

func (r redisLeases) compareAndDelete(ctx context.Context, key, value string) error {
	err := compareAndDeleteScript.Run(ctx, r.rdb, []string{key}, value).Err()
	if errors.Is(err, redis.Nil) {
		return nil
	}
	return err
}

func (r redisLeases) close() error { return r.rdb.Close() }
