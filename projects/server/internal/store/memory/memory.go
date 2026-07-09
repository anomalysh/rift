// Package memory is an in-memory implementation of the core storage ports.
//
// It exists so the gateway and ingress can be exercised end to end without a
// database. It is not intended for production: nothing survives a restart.
package memory

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/anomalysh/rift/projects/server/internal/core"
)

// Store implements core.TokenStore, core.ReservationStore and core.TunnelStore.
type Store struct {
	mu           sync.RWMutex
	tokens       map[string]core.Token       // by id
	reservations map[string]core.Reservation // by subdomain
	tunnels      map[string]core.Tunnel      // by id
}

// New returns an empty store.
func New() *Store {
	return &Store{
		tokens:       make(map[string]core.Token),
		reservations: make(map[string]core.Reservation),
		tunnels:      make(map[string]core.Tunnel),
	}
}

// Tokens implements the token port.
func (s *Store) Tokens() core.TokenStore { return (*tokenStore)(s) }

// Reservations implements the reservation port.
func (s *Store) Reservations() core.ReservationStore { return (*reservationStore)(s) }

// Tunnels implements the tunnel port.
func (s *Store) Tunnels() core.TunnelStore { return (*tunnelStore)(s) }

type tokenStore Store

func (s *tokenStore) FindByHash(_ context.Context, hash string) (*core.Token, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.tokens {
		if t.TokenHash == hash {
			cp := t
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("memory: token by hash: %w", core.ErrNotFound)
}

func (s *tokenStore) FindByID(_ context.Context, id string) (*core.Token, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tokens[id]
	if !ok {
		return nil, fmt.Errorf("memory: token %q: %w", id, core.ErrNotFound)
	}
	return &t, nil
}

func (s *tokenStore) Create(_ context.Context, t *core.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.tokens {
		if existing.TokenHash == t.TokenHash {
			return fmt.Errorf("memory: duplicate token hash: %w", core.ErrConflict)
		}
	}
	s.tokens[t.ID] = *t
	return nil
}

func (s *tokenStore) List(context.Context) ([]core.Token, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]core.Token, 0, len(s.tokens))
	for _, t := range s.tokens {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (s *tokenStore) Revoke(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[id]
	if !ok {
		return fmt.Errorf("memory: token %q: %w", id, core.ErrNotFound)
	}
	t.RevokedAt = &at
	s.tokens[id] = t
	return nil
}

func (s *tokenStore) TouchLastUsed(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[id]
	if !ok {
		return fmt.Errorf("memory: token %q: %w", id, core.ErrNotFound)
	}
	t.LastUsedAt = &at
	s.tokens[id] = t
	return nil
}

type reservationStore Store

func (s *reservationStore) Get(_ context.Context, subdomain string) (*core.Reservation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.reservations[subdomain]
	if !ok {
		return nil, fmt.Errorf("memory: reservation %q: %w", subdomain, core.ErrNotFound)
	}
	return &r, nil
}

func (s *reservationStore) Create(_ context.Context, r *core.Reservation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.reservations[r.Subdomain]; exists {
		return fmt.Errorf("memory: reservation %q: %w", r.Subdomain, core.ErrConflict)
	}
	if _, ok := s.tokens[r.TokenID]; !ok {
		return fmt.Errorf("memory: token %q: %w", r.TokenID, core.ErrNotFound)
	}
	s.reservations[r.Subdomain] = *r
	return nil
}

func (s *reservationStore) List(context.Context) ([]core.Reservation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]core.Reservation, 0, len(s.reservations))
	for _, r := range s.reservations {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subdomain < out[j].Subdomain })
	return out, nil
}

func (s *reservationStore) Delete(_ context.Context, subdomain string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.reservations[subdomain]; !ok {
		return fmt.Errorf("memory: reservation %q: %w", subdomain, core.ErrNotFound)
	}
	delete(s.reservations, subdomain)
	return nil
}

type tunnelStore Store

func (s *tunnelStore) Claim(_ context.Context, t *core.Tunnel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.tunnels {
		if existing.Subdomain == t.Subdomain {
			return fmt.Errorf("memory: subdomain %q: %w", t.Subdomain, core.ErrSubdomainTaken)
		}
	}
	s.tunnels[t.ID] = *t
	return nil
}

func (s *tunnelStore) Release(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tunnels, id)
	return nil
}

func (s *tunnelStore) Heartbeat(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tunnels[id]
	if !ok {
		return fmt.Errorf("memory: tunnel %q: %w", id, core.ErrNotFound)
	}
	t.LastSeenAt = at
	s.tunnels[id] = t
	return nil
}

func (s *tunnelStore) GetBySubdomain(_ context.Context, subdomain string) (*core.Tunnel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.tunnels {
		if t.Subdomain == subdomain {
			cp := t
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("memory: tunnel for %q: %w", subdomain, core.ErrNotFound)
}

func (s *tunnelStore) CountByToken(_ context.Context, tokenID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, t := range s.tunnels {
		if t.TokenID == tokenID {
			n++
		}
	}
	return n, nil
}

func (s *tunnelStore) ListActive(context.Context) ([]core.Tunnel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]core.Tunnel, 0, len(s.tunnels))
	for _, t := range s.tunnels {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (s *tunnelStore) DeleteStale(_ context.Context, cutoff time.Time) ([]core.Tunnel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var reaped []core.Tunnel
	for id, t := range s.tunnels {
		if t.LastSeenAt.Before(cutoff) {
			reaped = append(reaped, t)
			delete(s.tunnels, id)
		}
	}
	return reaped, nil
}

func (s *tunnelStore) DeleteByNode(_ context.Context, nodeID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, t := range s.tunnels {
		if t.NodeID == nodeID {
			delete(s.tunnels, id)
			n++
		}
	}
	return n, nil
}

// Compile-time proof that the store satisfies every port.
var (
	_ core.TokenStore       = (*tokenStore)(nil)
	_ core.ReservationStore = (*reservationStore)(nil)
	_ core.TunnelStore      = (*tunnelStore)(nil)
)
