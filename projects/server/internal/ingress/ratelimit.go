package ingress

import (
	"math"
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
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{buckets: make(map[string]*tokenBucket), now: time.Now}
}

// rateLimiterCap bounds the bucket map so a flood of distinct per-IP keys cannot
// grow it without bound; on overflow it is dropped and refills.
const rateLimiterCap = 65536

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
			rl.buckets = make(map[string]*tokenBucket)
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
