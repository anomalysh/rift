// Package policy compiles a core.Policy into the fast, stateless per-request
// checks the ingress runs before serving a public request: HTTP Basic auth (A2)
// and IP allow/deny by CIDR (A3). Rate limiting (A5) and lifetime bounds (A4)
// are stateful and live with the rate limiter and the gateway session; this
// package owns only what can be decided from the request alone.
//
// It imports core (the shared policy type) and nothing from ingress/gateway, so
// the access rules are unit-testable in isolation.
package policy

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net"
	"sync"

	"golang.org/x/crypto/bcrypt"

	"github.com/anomalysh/rift/projects/server/internal/core"
)

// Bounds on an agent-supplied policy. The policy arrives from the agent, and
// every visitor request is checked against it, so its cost per request must be
// bounded by the server, not by whoever wrote the policy.
const (
	// MaxBcryptCost caps the work factor of a basic-auth hash. Each visitor
	// request runs one bcrypt comparison, and the cost is exponential: at the
	// format's maximum (31) a single comparison takes days of CPU, so a hostile
	// agent could pin every core with a handful of requests. 12 is already
	// several times the library default (10) that the CLI uses.
	MaxBcryptCost = 12

	// MaxBasicAuthCreds caps the credentials on one tunnel. A request is
	// checked against a single matching user, but the list is scanned (and
	// held in memory) per tunnel, and a real deployment names a few users.
	MaxBasicAuthCreds = 16

	// MaxIPRules caps each of allow_ips and deny_ips. Every visitor request
	// walks both lists linearly.
	MaxIPRules = 256
)

// Compiled is a per-tunnel policy prepared once for many requests: CIDRs parsed,
// credentials held ready. Build it with Compile. A Compiled whose Empty()
// reports true enforces nothing, so callers can skip it on the common tunnel.
type Compiled struct {
	allow []*net.IPNet
	deny  []*net.IPNet
	basic []compiledCred
	rate  *core.RateLimit

	// dummyCost is the bcrypt cost CheckBasicAuth burns for an unknown user,
	// matching the configured hashes so a miss costs what a hit does.
	dummyCost int
}

// compiledCred is a basic-auth credential with its user name pre-hashed, so
// the user comparison is constant-time regardless of name length.
type compiledCred struct {
	userSum [sha256.Size]byte
	hash    []byte
}

// Compile parses a policy's CIDRs and validates them, returning an error naming
// the offending entry so a misconfigured tunnel fails loudly at connect time
// rather than silently admitting or rejecting every visitor.
func Compile(p core.Policy) (*Compiled, error) {
	c := &Compiled{rate: p.RateLimit, dummyCost: bcrypt.DefaultCost}
	var err error
	if len(p.AllowIPs) > MaxIPRules {
		return nil, fmt.Errorf("allow_ips: %d entries exceeds the limit of %d", len(p.AllowIPs), MaxIPRules)
	}
	if len(p.DenyIPs) > MaxIPRules {
		return nil, fmt.Errorf("deny_ips: %d entries exceeds the limit of %d", len(p.DenyIPs), MaxIPRules)
	}
	if c.allow, err = parseCIDRs(p.AllowIPs); err != nil {
		return nil, fmt.Errorf("allow_ips: %w", err)
	}
	if c.deny, err = parseCIDRs(p.DenyIPs); err != nil {
		return nil, fmt.Errorf("deny_ips: %w", err)
	}
	if c.rate != nil && c.rate.RPS <= 0 {
		return nil, fmt.Errorf("rate_limit: rps must be > 0, got %v", c.rate.RPS)
	}
	if c.basic, c.dummyCost, err = compileCreds(p.BasicAuth); err != nil {
		return nil, fmt.Errorf("basic_auth: %w", err)
	}
	return c, nil
}

// compileCreds checks every credential's hash is a bcrypt hash with a cost in
// [bcrypt.MinCost, MaxBcryptCost], and returns the highest cost seen (for the
// unknown-user dummy comparison).
func compileCreds(creds []core.BasicAuthCred) ([]compiledCred, int, error) {
	if len(creds) > MaxBasicAuthCreds {
		return nil, 0, fmt.Errorf("%d credentials exceeds the limit of %d", len(creds), MaxBasicAuthCreds)
	}
	out := make([]compiledCred, 0, len(creds))
	maxCost := bcrypt.DefaultCost
	for i, cred := range creds {
		cost, err := bcrypt.Cost([]byte(cred.Hash))
		if err != nil {
			return nil, 0, fmt.Errorf("credential %d (%q): hash is not a bcrypt hash", i, cred.User)
		}
		if cost < bcrypt.MinCost || cost > MaxBcryptCost {
			return nil, 0, fmt.Errorf("credential %d (%q): bcrypt cost %d is outside the allowed range %d-%d",
				i, cred.User, cost, bcrypt.MinCost, MaxBcryptCost)
		}
		if i == 0 || cost > maxCost {
			maxCost = cost
		}
		out = append(out, compiledCred{userSum: sha256.Sum256([]byte(cred.User)), hash: []byte(cred.Hash)})
	}
	return out, maxCost, nil
}

// dummyHashes holds one throwaway hash per allowed cost, generated on first
// use. CheckBasicAuth compares an unknown user's password against the one
// matching the tunnel's own cost, so the response time does not reveal which
// user names exist.
var dummyHashes [MaxBcryptCost + 1]struct {
	once sync.Once
	hash []byte
}

func dummyHash(cost int) []byte {
	if cost < bcrypt.MinCost || cost > MaxBcryptCost {
		cost = bcrypt.DefaultCost
	}
	d := &dummyHashes[cost]
	d.once.Do(func() {
		// The password is irrelevant: nothing is ever meant to match it.
		h, err := bcrypt.GenerateFromPassword([]byte("rift-unknown-user"), cost)
		if err == nil {
			d.hash = h
		}
	})
	return d.hash
}

// RateLimit returns the tunnel's rate limit, or nil if unlimited.
func (c *Compiled) RateLimit() *core.RateLimit { return c.rate }

// Validate reports whether a policy is well-formed and within the server's
// bounds (its CIDRs parse, its lists are not oversized, and every basic-auth
// hash is bcrypt with an acceptable cost), so the gateway can reject a
// misconfigured or hostile tunnel at connect time rather than let it 500, or
// burn CPU on, every visitor. It is Compile without keeping the result.
func Validate(p core.Policy) error {
	_, err := Compile(p)
	return err
}

// Empty reports whether the stateless policy admits everyone (no IP rules, no
// basic auth, no rate limit), so the ingress can skip the whole check.
func (c *Compiled) Empty() bool {
	return c == nil || (len(c.allow) == 0 && len(c.deny) == 0 && len(c.basic) == 0 && c.rate == nil)
}

// AllowsIP applies A3: a deny match rejects; if any allow range is set the
// default flips to deny, so only listed ranges are admitted. An unparseable ip
// (should not happen — the caller resolves it) is rejected, failing closed.
func (c *Compiled) AllowsIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range c.deny {
		if n.Contains(ip) {
			return false
		}
	}
	if len(c.allow) == 0 {
		return true
	}
	for _, n := range c.allow {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// RequiresBasicAuth reports whether the tunnel gates visitors on a credential.
func (c *Compiled) RequiresBasicAuth() bool { return c != nil && len(c.basic) > 0 }

// CheckBasicAuth applies A2: the supplied user must match a configured user
// (constant-time) and its password must verify against that user's bcrypt hash.
// The plaintext password never reached the server on the policy — only the hash
// did — so this is where the visitor's password is finally checked.
//
// Every configured user is compared, and an unknown user still pays for one
// bcrypt comparison (against a dummy hash of the same cost), so neither which
// users exist nor their position in the list shows in the response time.
func (c *Compiled) CheckBasicAuth(user, pass string) bool {
	sum := sha256.Sum256([]byte(user))
	var match []byte
	for _, cred := range c.basic {
		if subtle.ConstantTimeCompare(cred.userSum[:], sum[:]) == 1 && match == nil {
			match = cred.hash
		}
	}
	if match == nil {
		if h := dummyHash(c.dummyCost); h != nil {
			_ = bcrypt.CompareHashAndPassword(h, []byte(pass))
		}
		return false
	}
	return bcrypt.CompareHashAndPassword(match, []byte(pass)) == nil
}

func parseCIDRs(entries []string) ([]*net.IPNet, error) {
	out := make([]*net.IPNet, 0, len(entries))
	for _, e := range entries {
		// Accept a bare IP as a single-host CIDR, matching how the trusted-proxy
		// list is written.
		if ip := net.ParseIP(e); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			_, n, _ := net.ParseCIDR(fmt.Sprintf("%s/%d", e, bits))
			out = append(out, n)
			continue
		}
		_, n, err := net.ParseCIDR(e)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP or CIDR", e)
		}
		out = append(out, n)
	}
	return out, nil
}
