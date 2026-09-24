package ingress

import (
	"math"
	"sort"
	"sync"
	"time"
)

// rateLimiter is a keyed token-bucket limiter (A5). One bucket per key -- a
// tunnel subdomain, or subdomain+client-IP when the policy is per-IP. It mirrors
// the breaker's shape: a mutex-guarded map with an injectable clock for tests.
//
// This is a single-node limiter. In a Redis cluster each node limits
// independently, so the effective public rate is per-node; a shared limiter is
// a follow-up (the roadmap's "Redis for cluster").
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	now     func() time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
	// rps and burst are the limit this bucket was last charged under, kept so
	// eviction can tell whether the bucket has refilled completely.
	rps   float64
	burst int
}

// full reports whether the bucket would be back at its burst by now. A full
// bucket is indistinguishable from a missing one, so dropping it loses
// nothing: the client's next request recreates it in the same state.
func (b *tokenBucket) full(now time.Time) bool {
	return b.tokens+now.Sub(b.last).Seconds()*b.rps >= float64(b.burst)
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{buckets: make(map[string]*tokenBucket), now: time.Now}
}

// rateLimiterCap bounds the bucket map so a flood of distinct per-IP keys cannot
// grow it without bound.
const rateLimiterCap = 65536

// rateLimiterEvictBatch is how far below the cap an eviction pass drains the
// map, so the (linear) pass runs once per batch of new keys rather than on
// every insert once the map is full.
const rateLimiterEvictBatch = rateLimiterCap / 16

// evict makes room for new keys. It must be called with mu held.
//
// It never drops everything. Dropping the whole map on overflow (as this once
// did) let anyone with enough source addresses -- trivial from an IPv6 /48 --
// reset every tunnel's limit on the whole server at will. Instead it first
// removes buckets that have refilled completely, which is lossless, and only
// if the map is still too full does it drop the least recently used of the
// rest. A limited client is by definition recently active, so the buckets that
// go are the idle ones, and a flood of fresh keys mostly displaces itself.
func (rl *rateLimiter) evict(now time.Time) {
	for k, b := range rl.buckets {
		if b.full(now) {
			delete(rl.buckets, k)
		}
	}
	target := rateLimiterCap - rateLimiterEvictBatch
	excess := len(rl.buckets) - target
	if excess <= 0 {
		return
	}
	type aged struct {
		key  string
		last time.Time
	}
	all := make([]aged, 0, len(rl.buckets))
	for k, b := range rl.buckets {
		all = append(all, aged{k, b.last})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].last.Before(all[j].last) })
	for _, a := range all[:excess] {
		delete(rl.buckets, a.key)
	}
}

// allow refills the bucket for key at rps (capped at burst) and consumes one
// token, returning whether the request is admitted. burst<=0 defaults to
// ceil(rps) with a floor of 1, so a "20/s" limit tolerates a burst of 20.
func (rl *rateLimiter) allow(key string, rps float64, burst int) bool {
	if burst <= 0 {
		burst = int(math.Ceil(rps))
		if burst < 1 {
			burst = 1
		}
	}
	now := rl.now()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[key]
	if !ok {
		if len(rl.buckets) >= rateLimiterCap {
			rl.evict(now)
		}
		b = &tokenBucket{tokens: float64(burst), last: now}
		rl.buckets[key] = b
	} else {
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens = math.Min(float64(burst), b.tokens+elapsed*rps)
			b.last = now
		}
	}
	b.rps, b.burst = rps, burst

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// retryAfterSeconds is the whole seconds a client should wait before it would
// have a token again at rps, with a floor of 1 for the Retry-After header.
func retryAfterSeconds(rps float64) int {
	if rps <= 0 {
		return 1
	}
	s := int(math.Ceil(1 / rps))
	if s < 1 {
		s = 1
	}
	return s
}
