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
	"crypto/subtle"
	"fmt"
	"net"

	"golang.org/x/crypto/bcrypt"

	"github.com/anomalysh/rift/projects/server/internal/core"
)

// Compiled is a per-tunnel policy prepared once for many requests: CIDRs parsed,
// credentials held ready. Build it with Compile. A Compiled whose Empty()
// reports true enforces nothing, so callers can skip it on the common tunnel.
type Compiled struct {
	allow []*net.IPNet
	deny  []*net.IPNet
	basic []core.BasicAuthCred
}

// Compile parses a policy's CIDRs and validates them, returning an error naming
// the offending entry so a misconfigured tunnel fails loudly at connect time
// rather than silently admitting or rejecting every visitor.
func Compile(p core.Policy) (*Compiled, error) {
	c := &Compiled{basic: p.BasicAuth}
	var err error
	if c.allow, err = parseCIDRs(p.AllowIPs); err != nil {
		return nil, fmt.Errorf("allow_ips: %w", err)
	}
	if c.deny, err = parseCIDRs(p.DenyIPs); err != nil {
		return nil, fmt.Errorf("deny_ips: %w", err)
	}
	return c, nil
}

// Validate reports whether a policy is well-formed (its CIDRs parse), so the
// gateway can reject a misconfigured tunnel at connect time rather than let it
// 500 every visitor. It is Compile without keeping the result.
func Validate(p core.Policy) error {
	_, err := Compile(p)
	return err
}

// Empty reports whether the stateless policy admits everyone (no IP rules, no
// basic auth), so the ingress can skip the whole check.
func (c *Compiled) Empty() bool {
	return c == nil || (len(c.allow) == 0 && len(c.deny) == 0 && len(c.basic) == 0)
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
func (c *Compiled) CheckBasicAuth(user, pass string) bool {
	for _, cred := range c.basic {
		if subtle.ConstantTimeCompare([]byte(cred.User), []byte(user)) == 1 {
			return bcrypt.CompareHashAndPassword([]byte(cred.Hash), []byte(pass)) == nil
		}
	}
	return false
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
