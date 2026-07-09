// Package core holds the rift domain model and the ports (interfaces) that
// adapters implement. It depends on nothing but the standard library.
//
// Boundary rule: every other internal package may import core; core imports
// none of them. Storage, transport and HTTP concerns stay out of this package.
package core

import (
	"context"
	"errors"
	"time"
)

// Protocol is the tunnelled application protocol.
type Protocol string

const (
	// ProtocolHTTP tunnels HTTP, routed by subdomain and served over the shared
	// ingress (and TLS) listener.
	ProtocolHTTP Protocol = "http"
	// ProtocolTCP tunnels a raw TCP stream, reached on a public port the gateway
	// allocates for the tunnel.
	ProtocolTCP Protocol = "tcp"
	// ProtocolTLS tunnels raw TLS, routed by the ClientHello SNI to a subdomain
	// and passed through to the agent, which terminates TLS.
	ProtocolTLS Protocol = "tls"
)

// Valid reports whether the protocol is one this build can serve.
func (p Protocol) Valid() bool {
	switch p {
	case ProtocolHTTP, ProtocolTCP, ProtocolTLS:
		return true
	default:
		return false
	}
}

// IsRaw reports whether the protocol is carried as a raw byte stream rather
// than as HTTP request/response exchanges.
func (p Protocol) IsRaw() bool { return p == ProtocolTCP || p == ProtocolTLS }

// String implements fmt.Stringer.
func (p Protocol) String() string { return string(p) }

// Sentinel errors. Adapters wrap these; callers match with errors.Is.
var (
	ErrNotFound          = errors.New("core: not found")
	ErrUnauthorized      = errors.New("core: unauthorized")
	ErrSubdomainTaken    = errors.New("core: subdomain already in use")
	ErrSubdomainReserved = errors.New("core: subdomain is reserved")
	ErrSubdomainInvalid  = errors.New("core: subdomain is invalid")
	ErrTunnelLimit       = errors.New("core: tunnel limit reached for token")
	ErrConflict          = errors.New("core: conflicting write")
	ErrUnsupportedProto  = errors.New("core: unsupported protocol")

	// ErrTunnelUnavailable means the agent connection died with a request in
	// flight. The ingress turns this into a 502.
	ErrTunnelUnavailable = errors.New("core: tunnel unavailable")
)

// Token is an API credential. The plaintext secret is never persisted; only
// TokenHash (hex-encoded SHA-256 of the secret) is stored.
type Token struct {
	ID         string
	Name       string
	TokenHash  string
	MaxTunnels int
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
	ExpiresAt  *time.Time
}

// Active reports whether the token may open tunnels at time now.
func (t *Token) Active(now time.Time) bool {
	if t == nil || t.RevokedAt != nil {
		return false
	}
	if t.ExpiresAt != nil && !now.Before(*t.ExpiresAt) {
		return false
	}
	return true
}

// Reservation pins a subdomain to a token so no other token can claim it.
type Reservation struct {
	Subdomain string
	TokenID   string
	Note      string
	CreatedAt time.Time
}

// Tunnel is a live agent connection occupying a subdomain.
type Tunnel struct {
	ID          string
	Subdomain   string
	TokenID     string
	Protocol    Protocol
	LocalPort   int
	NodeID      string
	ClientAddr  string
	ConnectedAt time.Time
	LastSeenAt  time.Time
}

// Stale reports whether the tunnel has missed heartbeats past timeout.
func (t *Tunnel) Stale(now time.Time, timeout time.Duration) bool {
	return now.Sub(t.LastSeenAt) > timeout
}

// TokenStore persists API tokens.
type TokenStore interface {
	// FindByHash returns the token whose TokenHash equals hash.
	// Returns ErrNotFound when no such token exists.
	FindByHash(ctx context.Context, hash string) (*Token, error)
	FindByID(ctx context.Context, id string) (*Token, error)
	Create(ctx context.Context, t *Token) error
	List(ctx context.Context) ([]Token, error)
	Revoke(ctx context.Context, id string, at time.Time) error
	TouchLastUsed(ctx context.Context, id string, at time.Time) error
}

// ReservationStore persists subdomain reservations.
type ReservationStore interface {
	// Get returns the reservation for subdomain, or ErrNotFound.
	Get(ctx context.Context, subdomain string) (*Reservation, error)
	Create(ctx context.Context, r *Reservation) error
	List(ctx context.Context) ([]Reservation, error)
	Delete(ctx context.Context, subdomain string) error
}

// TunnelStore persists live tunnel records. It is the authority on which
// subdomains are occupied across all gateway nodes.
type TunnelStore interface {
	// Claim atomically inserts the tunnel, failing with ErrSubdomainTaken if
	// the subdomain is already held by a different tunnel.
	Claim(ctx context.Context, t *Tunnel) error
	// Release removes the tunnel by ID. Releasing an already-released tunnel
	// is not an error.
	Release(ctx context.Context, id string) error
	// Heartbeat advances last_seen_at. Returns ErrNotFound if the tunnel was
	// already reaped, which tells the gateway to stop serving it.
	Heartbeat(ctx context.Context, id string, at time.Time) error
	// GetBySubdomain returns the live tunnel on subdomain, or ErrNotFound.
	GetBySubdomain(ctx context.Context, subdomain string) (*Tunnel, error)
	// CountByToken returns how many tunnels the token currently holds.
	CountByToken(ctx context.Context, tokenID string) (int, error)
	// ListActive returns every live tunnel, newest first.
	ListActive(ctx context.Context) ([]Tunnel, error)
	// DeleteStale removes tunnels whose last_seen_at precedes cutoff and
	// returns them so their owners can be notified.
	DeleteStale(ctx context.Context, cutoff time.Time) ([]Tunnel, error)
	// DeleteByNode removes every tunnel owned by nodeID. Used on clean
	// startup to clear records a previous crash left behind.
	DeleteByNode(ctx context.Context, nodeID string) (int, error)
}
