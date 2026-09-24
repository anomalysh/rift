package ingress

import (
	"sync"
	"time"
)

// breakerThreshold is how many consecutive transport failures open a peer's
// circuit. One blip should not trip it; a genuinely dead node will exceed this
// in a handful of requests.
const breakerThreshold = 3

// breakerCooldown is how long a node stays skipped once its circuit opens.
// Long enough to stop hammering a dead node, short enough that a node coming
// back is routed to again promptly. It is deliberately near a heartbeat
// timeout: by the time it elapses, either the lease has expired or the node is
// back.
const breakerCooldown = 10 * time.Second

// breakerForget is how long a node's failure record outlives its last failure.
// Only a success used to delete a record, so a node that failed and then left
// the cluster (a replaced pod, a new advertise address) kept one forever, and
// failures minutes or days apart still counted as "consecutive" and could
// open the circuit on a healthy node. A minute of quiet wipes the slate; a
// node still dead then costs at most breakerThreshold dials to re-detect.
const breakerForget = time.Minute

// breaker is a per-peer circuit breaker. It exists so that a node which has
// died does not cost every subsequent request a full dial timeout before it is
// declared unreachable: after a few consecutive failures the circuit opens and
// forwards to that node are refused immediately until the cooldown elapses.
//
// This is intentionally a plain closed/open breaker with no half-open probe
// counting: the next request after the cooldown is the probe, and its result
// closes or re-opens the circuit.
type breaker struct {
	mu    sync.Mutex
	state map[string]*breakerEntry
	now   func() time.Time
}

type breakerEntry struct {
	failures    int
	openedAt    time.Time
	lastFailure time.Time
}

func newBreaker() *breaker {
	return &breaker{
		state: make(map[string]*breakerEntry),
		now:   time.Now,
	}
}

// isOpen reports whether forwards to node should be skipped right now.
func (b *breaker) isOpen(node string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	e := b.state[node]
	if e == nil || e.failures < breakerThreshold {
		return false
	}
	if b.now().Sub(e.openedAt) >= breakerCooldown {
		// Cooldown elapsed: let the next request through as a probe.
		return false
	}
	return true
}

func (b *breaker) recordFailure(node string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	// Sweep on the failure path only: it is rare, and the map holds at most
	// the nodes that failed within the last breakerForget.
	for n, e := range b.state {
		if now.Sub(e.lastFailure) >= breakerForget {
			delete(b.state, n)
		}
	}

	e := b.state[node]
	if e == nil {
		e = &breakerEntry{}
		b.state[node] = e
	}
	e.failures++
	e.lastFailure = now
	if e.failures == breakerThreshold {
		e.openedAt = now
	} else if e.failures > breakerThreshold && now.Sub(e.openedAt) >= breakerCooldown {
		// A probe after cooldown failed again: re-open from now.
		e.openedAt = now
	}
}

func (b *breaker) recordSuccess(node string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.state, node)
}
