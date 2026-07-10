package ingress

import (
	"testing"
	"time"
)

func TestRateLimiterBurstThenRefill(t *testing.T) {
	now := time.Unix(0, 0)
	rl := newRateLimiter()
	rl.now = func() time.Time { return now }

	// 2 req/s, burst 2: the first two are admitted, the third is not.
	if !rl.allow("k", 2, 2) || !rl.allow("k", 2, 2) {
		t.Fatal("the burst allowance was not granted")
	}
	if rl.allow("k", 2, 2) {
		t.Fatal("a request past the burst was admitted")
	}

	// After 0.5s one token has refilled (2/s), so one more is admitted.
	now = now.Add(500 * time.Millisecond)
	if !rl.allow("k", 2, 2) {
		t.Fatal("a token did not refill after 0.5s at 2/s")
	}
	if rl.allow("k", 2, 2) {
		t.Fatal("a second request was admitted before another token refilled")
	}
}

func TestRateLimiterDefaultBurst(t *testing.T) {
	now := time.Unix(0, 0)
	rl := newRateLimiter()
	rl.now = func() time.Time { return now }
	// burst<=0 defaults to ceil(rps): 5/s tolerates a burst of 5.
	for i := 0; i < 5; i++ {
		if !rl.allow("k", 5, 0) {
			t.Fatalf("request %d within the default burst was denied", i+1)
		}
	}
	if rl.allow("k", 5, 0) {
		t.Fatal("a sixth request past the default burst was admitted")
	}
}

func TestRateLimiterKeysAreIndependent(t *testing.T) {
	now := time.Unix(0, 0)
	rl := newRateLimiter()
	rl.now = func() time.Time { return now }
	if !rl.allow("a", 1, 1) {
		t.Fatal("key a first request denied")
	}
	if !rl.allow("b", 1, 1) {
		t.Fatal("key b must have its own bucket")
	}
	if rl.allow("a", 1, 1) {
		t.Fatal("key a second request should be denied")
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	if got := retryAfterSeconds(20); got != 1 {
		t.Fatalf("retryAfter(20/s) = %d, want 1 (floor)", got)
	}
	if got := retryAfterSeconds(0.5); got != 2 {
		t.Fatalf("retryAfter(0.5/s) = %d, want 2", got)
	}
}
