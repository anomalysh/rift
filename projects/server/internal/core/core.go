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
	// ProtocolUDP tunnels raw UDP datagrams, reached on a public UDP port the
	// gateway allocates for the tunnel. Each client flow is carried as a
	// length-delimited datagram stream over a raw tunnel stream (P4).
	ProtocolUDP Protocol = "udp"
	// ProtocolGRPC tunnels cleartext HTTP/2 (h2c), routed by the request's
	// :authority to a subdomain and passed through to the agent's local gRPC
	// server. Piping the bytes raw preserves HTTP/2 streaming and trailers,
	// which gRPC uses for grpc-status (P7).
	ProtocolGRPC Protocol = "grpc"
)

// Valid reports whether the protocol is one this build can serve.
func (p Protocol) Valid() bool {
	switch p {
	case ProtocolHTTP, ProtocolTCP, ProtocolTLS, ProtocolUDP, ProtocolGRPC:
		return true
	default:
		return false
	}
}

// String implements fmt.Stringer.
func (p Protocol) String() string { return string(p) }

// Sentinel errors. Adapters wrap these; callers match with errors.Is.
var (
	ErrNotFound          = errors.New("core: not found")
	ErrUnauthorized      = errors.New("core: unauthorized")
	ErrSubdomainTaken    = errors.New("core: subdomain already in use")
	ErrSubdomainReserved = errors.New("core: subdomain is reserved")
	ErrSubdomainInvalid  = errors.New("core: subdomain is invalid")
	ErrConflict          = errors.New("core: conflicting write")
	// ErrDomainOwned means a custom domain is already registered to a different
	// token, so this token may not claim it (E1).
	ErrDomainOwned = errors.New("core: custom domain owned by another token")

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

// CustomDomain maps a customer-owned hostname (e.g. app.acme.com) to the
// subdomain whose tunnel serves it (E1). The mapping is upserted each time an
// agent connects with --domain, so it follows the live tunnel across reconnects
// even when the subdomain is regenerated.
//
// The mapping names a subdomain, not a tunnel, and subdomains are reusable:
// once the owner disconnects anyone may claim the same label. So the mapping
// is honoured only while the tunnel holding Subdomain belongs to TokenID;
// routing on Subdomain alone would hand the domain to whoever claims the label
// next.
type CustomDomain struct {
	Domain    string // fully qualified, lower-cased, no trailing dot
	Subdomain string // the rift subdomain this domain routes to
	TokenID   string // owning token; another token cannot claim the same domain
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
	// Policy is the visitor-access policy the agent attached at connect time.
	// The zero value enforces nothing.
	Policy Policy
}

// The store ports below are implemented by the Postgres and in-memory
// adapters, and internal/store/storetest pins them to the same behaviour: the
// errors, orderings and edge cases documented here are the contract, not an
// artifact of either backend.

// TokenStore persists API tokens.
type TokenStore interface {
	// FindByHash returns the token whose TokenHash equals hash.
	// Returns ErrNotFound when no such token exists.
	FindByHash(ctx context.Context, hash string) (*Token, error)
	// FindByID returns the token with id, or ErrNotFound.
	FindByID(ctx context.Context, id string) (*Token, error)
	// Create inserts t. A duplicate ID or TokenHash is ErrConflict.
	Create(ctx context.Context, t *Token) error
	// List returns every token, oldest first (by ID).
	List(ctx context.Context) ([]Token, error)
	// Revoke and TouchLastUsed return ErrNotFound for an unknown id.
	Revoke(ctx context.Context, id string, at time.Time) error
	TouchLastUsed(ctx context.Context, id string, at time.Time) error
}

// ReservationStore persists subdomain reservations.
type ReservationStore interface {
	// Get returns the reservation for subdomain, or ErrNotFound.
	Get(ctx context.Context, subdomain string) (*Reservation, error)
	// Create inserts r. An already-reserved subdomain is ErrConflict and an
	// unknown TokenID is ErrNotFound.
	Create(ctx context.Context, r *Reservation) error
	// List returns every reservation, by subdomain.
	List(ctx context.Context) ([]Reservation, error)
	// Delete removes the reservation, or returns ErrNotFound.
	Delete(ctx context.Context, subdomain string) error
}

// DomainStore persists custom-domain -> subdomain mappings (E1).
type DomainStore interface {
	// Upsert records that a custom domain routes to a subdomain for a token.
	// It refreshes the subdomain when the same token reconnects, and returns
	// ErrDomainOwned when the domain is already held by a different token.
	Upsert(ctx context.Context, d CustomDomain) error
	// Transfer is Upsert for a domain the caller believes is held by
	// fromTokenID: it succeeds when the domain is unmapped or still owned by
	// fromTokenID (or already by d.TokenID), and returns ErrDomainOwned when a
	// third token got there first. The ownership check and the write are one
	// atomic step, so two agents reclaiming the same abandoned domain cannot
	// both win.
	Transfer(ctx context.Context, d CustomDomain, fromTokenID string) error
	// Lookup returns the mapping for a custom domain, or ErrNotFound. Callers
	// must check that the tunnel on Subdomain belongs to TokenID before routing.
	Lookup(ctx context.Context, domain string) (*CustomDomain, error)
	// List returns every custom-domain mapping, by domain.
	List(ctx context.Context) ([]CustomDomain, error)
	// Delete removes a mapping. Deleting an absent domain is not an error.
	Delete(ctx context.Context, domain string) error
}

// TunnelStore persists live tunnel records. It is the authority on which
// subdomains are occupied across all gateway nodes.
type TunnelStore interface {
	// Claim atomically inserts the tunnel, failing with ErrSubdomainTaken if
	// the subdomain is already held by a different tunnel, and ErrConflict if
	// a tunnel with the same ID already exists.
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
