package ingress

import (
	"testing"
	"time"
)

func TestBreakerOpensAfterThreshold(t *testing.T) {
	b := newBreaker()
	const node = "http://10.0.0.2:8080"

	for i := 0; i < breakerThreshold-1; i++ {
		b.recordFailure(node)
		if b.isOpen(node) {
			t.Fatalf("circuit opened early after %d failures", i+1)
		}
	}
	b.recordFailure(node)
	if !b.isOpen(node) {
		t.Fatal("circuit should be open at the threshold")
	}
}

func TestBreakerClosesAfterCooldown(t *testing.T) {
	b := newBreaker()
	now := time.Unix(0, 0)
	b.now = func() time.Time { return now }
	const node = "http://10.0.0.2:8080"

	for i := 0; i < breakerThreshold; i++ {
		b.recordFailure(node)
	}
	if !b.isOpen(node) {
		t.Fatal("expected open circuit")
	}

	now = now.Add(breakerCooldown - time.Millisecond)
	if !b.isOpen(node) {
		t.Fatal("circuit should still be open before cooldown elapses")
	}

	now = now.Add(2 * time.Millisecond)
	if b.isOpen(node) {
		t.Fatal("circuit should allow a probe once cooldown elapses")
	}
}

func TestBreakerSuccessResets(t *testing.T) {
	b := newBreaker()
	const node = "http://10.0.0.2:8080"

	for i := 0; i < breakerThreshold; i++ {
		b.recordFailure(node)
	}
	b.recordSuccess(node)
	if b.isOpen(node) {
		t.Fatal("a success must reset the circuit")
	}
}

// A per-node breaker must not let one dead node affect another.
func TestBreakerIsPerNode(t *testing.T) {
	b := newBreaker()
	dead := "http://10.0.0.2:8080"
	alive := "http://10.0.0.3:8080"

	for i := 0; i < breakerThreshold; i++ {
		b.recordFailure(dead)
	}
	if b.isOpen(alive) {
		t.Fatal("a healthy node's circuit was opened by another node's failures")
	}
}
