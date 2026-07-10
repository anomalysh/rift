package ingress

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"

	"github.com/anomalysh/rift/projects/server/internal/core"
	"github.com/anomalysh/rift/projects/server/internal/policy"
)

// policyCacheCap bounds the compiled-policy cache. Tunnel IDs are unique per
// connection, so a long-lived server would otherwise accumulate one entry per
// tunnel ever seen. On overflow the whole map is dropped and refills lazily --
// cheap and rare, since only policied tunnels are cached at all.
const policyCacheCap = 4096

// policyCache memoizes a tunnel's compiled visitor policy keyed by tunnel ID.
type policyCache struct {
	mu sync.Mutex
	m  map[string]*policy.Compiled
}

func newPolicyCache() *policyCache { return &policyCache{m: make(map[string]*policy.Compiled)} }

// get returns the compiled policy for a tunnel, compiling and caching on first
// use. The tunnel's CIDRs were already validated at connect time (the gateway
// rejects an invalid policy), so a compile error here is not expected; it is
// surfaced so enforce() can fail closed rather than admit everyone.
func (c *policyCache) get(t core.Tunnel) (*policy.Compiled, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if compiled, ok := c.m[t.ID]; ok {
		return compiled, nil
	}
	compiled, err := policy.Compile(t.Policy)
	if err != nil {
		return nil, err
	}
	if len(c.m) >= policyCacheCap {
		c.m = make(map[string]*policy.Compiled)
	}
	c.m[t.ID] = compiled
	return compiled, nil
}

// enforce applies the stateless visitor-access policy (A3 IP allow/deny, then A2
// basic-auth) before a request is proxied. It returns true to allow; on false it
// has already written the response and the caller must return. It is invoked at
// the top of both proxy() and proxyUpgrade(), which are the two paths reached
// from handlePublic AND from the cross-node handleInternalProxy -- so a policy
// covers a peer-forwarded request too, checked against the real client IP that
// the edge stamped in X-Forwarded-For / X-Real-IP.
func (i *Ingress) enforce(w http.ResponseWriter, r *http.Request, sess core.Session, sub string) bool {
	t := sess.Tunnel()
	if t.Policy.IsZero() {
		return true
	}
	compiled, err := i.policies.get(t)
	if err != nil {
		i.logger.Error("invalid tunnel policy", slog.String("subdomain", sub), slog.Any("error", err))
		i.writeGatewayError(w, r, http.StatusInternalServerError, "policy_error",
			"This tunnel's access policy could not be applied.")
		return false
	}
	if compiled.Empty() {
		return true
	}

	// A3: IP allow/deny against the resolved client (honours the trusted-proxy
	// chain via clientIP).
	ip := net.ParseIP(i.clientIP(r))
	if !compiled.AllowsIP(ip) {
		i.logger.Info("visitor blocked by ip policy",
			slog.String("subdomain", sub), slog.String("client", i.clientIP(r)))
		i.writeGatewayError(w, r, http.StatusForbidden, "ip_forbidden",
			"Your network address is not permitted to reach this tunnel.")
		return false
	}

	// A2: HTTP Basic auth. A missing or wrong credential gets a 401 with a
	// challenge so a browser prompts.
	if compiled.RequiresBasicAuth() {
		user, pass, ok := r.BasicAuth()
		if !ok || !compiled.CheckBasicAuth(user, pass) {
			w.Header().Set("WWW-Authenticate", `Basic realm="rift"`)
			i.writeGatewayError(w, r, http.StatusUnauthorized, "unauthorized",
				"Authentication is required to reach this tunnel.")
			return false
		}
	}

	// A5: rate limit. Keyed per tunnel, or per tunnel+client-IP when per_ip.
	if rl := compiled.RateLimit(); rl != nil {
		key := sub
		if rl.PerIP {
			key = sub + "|" + i.clientIP(r)
		}
		if !i.limiter.allow(key, rl.RPS, rl.Burst) {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(rl.RPS)))
			i.writeGatewayError(w, r, http.StatusTooManyRequests, "rate_limited",
				"Too many requests to this tunnel; slow down and retry.")
			return false
		}
	}
	return true
}
