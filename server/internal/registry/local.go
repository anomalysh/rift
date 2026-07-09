package registry

import (
	"context"
	"sync"

	"github.com/anomalysh/rift/server/internal/core"
)

// Local is the in-memory subdomain -> session map for this process.
// It is the whole registry in a single-node deployment.
type Local struct {
	mu       sync.RWMutex
	sessions map[string]core.Session // keyed by subdomain
}

// NewLocal returns an empty registry.
func NewLocal() *Local {
	return &Local{sessions: make(map[string]core.Session)}
}

// Register attaches s and returns whatever session it displaced.
func (l *Local) Register(_ context.Context, s core.Session) (core.Session, error) {
	sub := s.Tunnel().Subdomain

	l.mu.Lock()
	defer l.mu.Unlock()

	displaced := l.sessions[sub]
	if displaced == s {
		displaced = nil
	}
	l.sessions[sub] = s
	return displaced, nil
}

// Unregister detaches s only if it is still the session holding its subdomain.
//
// A session that was displaced by a reconnecting agent must not remove its
// replacement when its own read loop finally notices the socket is gone. The
// identity check is what makes a slow disconnect harmless.
func (l *Local) Unregister(_ context.Context, s core.Session) error {
	sub := s.Tunnel().Subdomain

	l.mu.Lock()
	defer l.mu.Unlock()

	if cur, ok := l.sessions[sub]; ok && cur == s {
		delete(l.sessions, sub)
	}
	return nil
}

// Lookup returns the session attached to this node for subdomain.
func (l *Local) Lookup(_ context.Context, subdomain string) (core.Session, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	s, ok := l.sessions[subdomain]
	return s, ok
}

// LocatePeer always reports "not elsewhere": a local-only registry has no peers.
func (l *Local) LocatePeer(context.Context, string) (string, bool, error) {
	return "", false, nil
}

// InvalidatePeer is a no-op: a local-only registry publishes no leases.
func (l *Local) InvalidatePeer(context.Context, string, string) error {
	return nil
}

// Subdomains snapshots the attached subdomains. Used by the Redis lease
// refresher and by diagnostics.
func (l *Local) Subdomains() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()

	out := make([]string, 0, len(l.sessions))
	for sub := range l.sessions {
		out = append(out, sub)
	}
	return out
}

// Sessions snapshots the attached sessions, for graceful shutdown.
func (l *Local) Sessions() []core.Session {
	l.mu.RLock()
	defer l.mu.RUnlock()

	out := make([]core.Session, 0, len(l.sessions))
	for _, s := range l.sessions {
		out = append(out, s)
	}
	return out
}

// Close releases nothing; the local registry owns no background resources.
func (l *Local) Close() error { return nil }
