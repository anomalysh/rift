package core

// Policy is the per-tunnel visitor-access policy an agent attaches at connect
// time (carried in the Hello) and, durably, in the tunnels table. It is the one
// object the ingress consults before serving a public request, and the gateway
// consults over the tunnel's lifetime. Every field is optional; the zero Policy
// enforces nothing (the historical behaviour), so it is safe on every tunnel.
//
// It lives in core, dependency-free, so the wire type (tunnelproto), the
// enforcement (internal/policy), the gateway session, and the store all share
// one definition rather than re-encoding the contract. JSON tags are the wire
// and jsonb form; `omitempty` keeps an unset policy a `{}` on the wire.
type Policy struct {
	// BasicAuth: any matching credential admits the request (A2). The password
	// is stored bcrypt-hashed by the agent; the plaintext never reaches riftd.
	BasicAuth []BasicAuthCred `json:"basic_auth,omitempty"`

	// AllowIPs / DenyIPs are CIDRs against the resolved client IP (A3). A deny
	// match rejects; if AllowIPs is non-empty the default flips to deny, so only
	// listed ranges are admitted.
	AllowIPs []string `json:"allow_ips,omitempty"`
	DenyIPs  []string `json:"deny_ips,omitempty"`

	// RateLimit throttles requests (A5); nil means unlimited.
	RateLimit *RateLimit `json:"rate_limit,omitempty"`

	// Lifetime bounds (A4). TTLSeconds retires the tunnel after a wall-clock
	// budget; Once retires it after the first completed request; MaxRequests
	// after N. Zero/false means no bound.
	TTLSeconds  int  `json:"ttl_seconds,omitempty"`
	Once        bool `json:"once,omitempty"`
	MaxRequests int  `json:"max_requests,omitempty"`
}

// BasicAuthCred is one accepted HTTP Basic credential; Hash is a bcrypt hash of
// the password so a leaked policy (or DB row) never exposes the plaintext.
type BasicAuthCred struct {
	User string `json:"user"`
	Hash string `json:"hash"`
}

// RateLimit is a token-bucket rate: RPS requests per second with a burst
// allowance. PerIP applies the limit per client IP rather than per tunnel.
type RateLimit struct {
	RPS   float64 `json:"rps"`
	Burst int     `json:"burst,omitempty"`
	PerIP bool    `json:"per_ip,omitempty"`
}

// IsZero reports whether the policy enforces nothing, so callers can skip the
// whole machinery on the common (unpolicied) tunnel.
func (p Policy) IsZero() bool {
	return len(p.BasicAuth) == 0 &&
		len(p.AllowIPs) == 0 &&
		len(p.DenyIPs) == 0 &&
		p.RateLimit == nil &&
		p.TTLSeconds == 0 &&
		!p.Once &&
		p.MaxRequests == 0
}

// HasLifetimeBound reports whether any A4 bound is set, so the gateway only
// arms a session timer/counter when one is actually needed.
func (p Policy) HasLifetimeBound() bool {
	return p.TTLSeconds > 0 || p.Once || p.MaxRequests > 0
}
