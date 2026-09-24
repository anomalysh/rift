// Package ingress serves public traffic for *.BASE_DOMAIN by routing each
// request to the agent session holding the requested subdomain.
//
// It depends only on core and tunnelproto. It does not know that tunnels are
// carried over WebSockets.
package ingress

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/anomalysh/rift/projects/server/internal/config"
	"github.com/anomalysh/rift/projects/server/internal/core"
	"github.com/anomalysh/rift/projects/server/internal/tunnelproto"
)

// copyBufferSize is the chunk size used when streaming a tunnel response to
// the public client.
const copyBufferSize = 32 << 10

// Ingress routes public requests into tunnels.
type Ingress struct {
	cfg          *config.Config
	logger       *slog.Logger
	registry     core.Registry
	tunnels      core.TunnelStore
	reservations core.ReservationStore
	domains      core.DomainStore

	// peers forwards to another node when Redis says it owns the subdomain.
	peers *http.Client

	// breaker stops forwarding to a peer node that has failed repeatedly, so a
	// dead node costs one dial timeout rather than one per request.
	breaker *breaker

	trusted []netAddr

	// policies memoizes each tunnel's compiled visitor policy (parsed CIDRs)
	// keyed by tunnel ID, so the stateless enforce() gate parses once per tunnel.
	policies *policyCache

	// limiter enforces per-tunnel (or per-IP) request rate limits (A5).
	limiter *rateLimiter

	// errorPages holds branded gateway error templates (T4); nil serves the
	// built-in plain-text/JSON bodies.
	errorPages *errorPages

	// ready reports whether this node's dependencies are usable. Nil means
	// "nothing to check", which is what the tests and a store-less build want.
	ready ReadyFunc
}

// ReadyFunc reports whether a dependency is usable right now.
type ReadyFunc func(context.Context) error

// readyTimeout bounds the readiness probe. A probe that can hang is worse than
// no probe: an orchestrator waits on it instead of restarting the process.
const readyTimeout = 2 * time.Second

type netAddr struct {
	ip  net.IP
	net *net.IPNet
}

// New builds the ingress.
func New(
	cfg *config.Config,
	logger *slog.Logger,
	reg core.Registry,
	tunnels core.TunnelStore,
	reservations core.ReservationStore,
	domains core.DomainStore,
) *Ingress {
	return &Ingress{
		cfg:          cfg,
		logger:       logger.With(slog.String("component", "ingress")),
		registry:     reg,
		tunnels:      tunnels,
		reservations: reservations,
		domains:      domains,
		peers: &http.Client{
			// No client timeout: a tunnelled response may legitimately stream
			// for a long time. The per-request context carries the deadline.
			Transport: &http.Transport{
				// Never an environment HTTP proxy: node-to-node hops carry the
				// peer secret and visitors' requests, and belong on the
				// cluster's private network, not relayed through whatever
				// HTTP_PROXY the process happened to inherit.
				Proxy:                 nil,
				MaxIdleConnsPerHost:   32,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: cfg.Tunnel.RequestTimeout,
			},
		},
		breaker:    newBreaker(),
		trusted:    parseTrusted(cfg.Ingress.TrustedProxyIPs),
		policies:   newPolicyCache(),
		limiter:    newRateLimiter(),
		errorPages: loadErrorPages(cfg.Ingress.ErrorPageDir, logger),
	}
}

// SetReadyCheck installs the readiness probe's dependency check. Call it
// before serving.
func (i *Ingress) SetReadyCheck(fn ReadyFunc) { i.ready = fn }

// Handler mounts the public routes plus the internal endpoints Caddy and peer
// nodes use.
//
// The internal endpoints (health, readiness, tls-ask, peer proxy) answer only
// on names that are not tunnel hosts: IP literals, single-label names such as
// the `riftd` container name or localhost, and domains nobody registered.
// Those are what Caddy's ask URL, container health checks and peer nodes
// dial. A Host that resolves to a tunnel -- anything under the base domain,
// the base domain and gateway hostname themselves, or a registered custom
// domain -- always goes to the tunnel, for two reasons:
//
//   - a tunnelled app must be able to serve its own /healthz or /readyz,
//     which a Host-blind mux would shadow on every tunnel
//   - Caddy proxies every path on *.base, so a Host-blind tls-ask would let
//     anyone on the internet probe which subdomains and custom domains are
//     live or reserved
//
// The one exception is the peer hop, which keeps the visitor's Host on the
// request: RouteInternalProxy carrying a peer token (with Redis enabled) goes
// to handleInternalProxy whatever the Host, and is authenticated there.
func (i *Ingress) Handler() http.Handler {
	internal := http.NewServeMux()
	internal.HandleFunc(config.RouteHealth, i.handleHealth)
	internal.HandleFunc(config.RouteReady, i.handleReady)
	internal.HandleFunc(config.RouteTLSAsk, i.handleTLSAsk)
	internal.HandleFunc(config.RouteInternalProxy, i.handleInternalProxy)
	internal.HandleFunc("/", i.handleNotATunnel)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if i.isPeerHop(r) {
			i.handleInternalProxy(w, r)
			return
		}
		if t, public := i.resolveHost(r.Context(), r.Host); public {
			i.servePublic(w, r, t)
			return
		}
		internal.ServeHTTP(w, r)
	})
}

// isPeerHop reports whether r claims to be a node-to-node forward. It is only
// a routing decision; handleInternalProxy still authenticates the peer token.
func (i *Ingress) isPeerHop(r *http.Request) bool {
	return i.cfg.Redis.Enabled &&
		r.URL.Path == config.RouteInternalProxy &&
		r.Header.Get(config.HeaderRiftPeerToken) != ""
}

// handleNotATunnel answers a request to an internal name on a path that is not
// an internal route.
func (i *Ingress) handleNotATunnel(w http.ResponseWriter, r *http.Request) {
	i.writeGatewayError(w, r, http.StatusNotFound, "not_a_tunnel",
		"This host does not correspond to a tunnel.")
}

// handleHealth is liveness: the process is running and serving. It must never
// consult a dependency, or a database blip would make an orchestrator kill a
// healthy server.
func (i *Ingress) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// handleReady is readiness: this node can actually serve. It does consult the
// database, because a node that cannot reach Postgres cannot authorize a
// handshake or claim a subdomain, and should be taken out of rotation rather
// than restarted.
func (i *Ingress) handleReady(w http.ResponseWriter, r *http.Request) {
	if i.ready == nil {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ready\n")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()

	if err := i.ready(ctx); err != nil {
		i.logger.Warn("readiness probe failed", slog.Any("error", err))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		// The reason stays in the log; a probe response is not a place to
		// describe internal topology to whoever can reach the port.
		_, _ = io.WriteString(w, "not ready\n")
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ready\n")
}

// handleTLSAsk authorizes Caddy's on-demand certificate issuance.
//
// Caddy issues a certificate for any SNI this endpoint approves, so approving
// broadly would turn the server into an open certificate-issuance relay and
// burn the ACME rate limit. Only a subdomain that is currently tunnelled or
// explicitly reserved gets a certificate, and a custom domain only while its
// owning token can actually serve it.
func (i *Ingress) handleTLSAsk(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get(config.QueryParamDomain)
	if domain == "" {
		http.Error(w, "missing domain", http.StatusBadRequest)
		return
	}
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))

	// Two names this server owns are not tunnel subdomains, so the checks below
	// would refuse them, leaving each with no certificate at all:
	//
	//   - the gateway hostname, which agents dial over TLS
	//   - the base domain itself, which a wildcard certificate does not cover
	//     and which visitors reach by simply trimming a subdomain off the URL
	//
	// Both are names the operator configured, not names a client can suggest,
	// so authorizing them does not widen what an attacker can make us issue.
	if i.ownsHostname(domain) {
		w.WriteHeader(http.StatusOK)
		return
	}

	ctx := r.Context()

	// A name under the base domain is only ever a subdomain. It is never
	// looked up as a custom domain: the gateway refuses to register one there,
	// and consulting the table anyway would let a stale or hand-inserted row
	// mint a certificate for a multi-label name such as a.b.<base>.
	if core.IsServerHostname(domain, i.cfg.Tunnel.BaseDomain, i.cfg.Gateway.Hostname) {
		sub, ok := core.SubdomainFromHost(domain, i.cfg.Tunnel.BaseDomain)
		if !ok {
			i.logger.Debug("refusing certificate for a multi-label name", slog.String("domain", domain))
			http.Error(w, "domain is not served by this host", http.StatusForbidden)
			return
		}
		i.askSubdomain(w, r, sub)
		return
	}

	// E1: not a subdomain, but a registered BYO custom domain still gets a
	// certificate so Caddy can terminate TLS for it on demand.
	cd, err := i.lookupCustomDomain(ctx, domain)
	switch {
	case errors.Is(err, core.ErrNotFound):
		i.logger.Debug("refusing certificate for foreign domain", slog.String("domain", domain))
		http.Error(w, "domain is not served by this host", http.StatusForbidden)
		return
	case err != nil:
		i.logger.Error("tls-ask custom domain lookup failed", slog.Any("error", err))
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}

	// The mapping alone is not enough: it names a subdomain, which anyone may
	// hold once its owner leaves. Issue only while the owning token holds that
	// subdomain (live) or has it reserved (about to connect).
	servable, err := i.customDomainServable(ctx, cd, true)
	if err != nil {
		i.logger.Error("tls-ask custom domain owner check failed", slog.Any("error", err))
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	if !servable {
		i.logger.Debug("refusing certificate for custom domain with no owning tunnel", slog.String("domain", domain))
		http.Error(w, "no such tunnel", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// askSubdomain answers tls-ask for a tunnel subdomain: approved when it is
// live on any node or reserved.
func (i *Ingress) askSubdomain(w http.ResponseWriter, r *http.Request, sub string) {
	ctx := r.Context()
	if _, live := i.registry.Lookup(ctx, sub); live {
		w.WriteHeader(http.StatusOK)
		return
	}
	if _, err := i.tunnels.GetBySubdomain(ctx, sub); err == nil {
		w.WriteHeader(http.StatusOK)
		return
	} else if !errors.Is(err, core.ErrNotFound) {
		i.logger.Error("tls-ask tunnel lookup failed", slog.Any("error", err))
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	// A reserved subdomain gets a certificate before its agent connects, so
	// the first request after `rift http 3000 myapp` is not delayed by an
	// ACME round trip.
	if _, err := i.reservations.Get(ctx, sub); err == nil {
		w.WriteHeader(http.StatusOK)
		return
	} else if !errors.Is(err, core.ErrNotFound) {
		i.logger.Error("tls-ask reservation lookup failed", slog.Any("error", err))
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}

	i.logger.Debug("refusing certificate for inactive subdomain", slog.String("subdomain", sub))
	http.Error(w, "no such tunnel", http.StatusNotFound)
}

// lookupCustomDomain returns the BYO mapping for host (E1), or ErrNotFound
// when the store is absent, host is not a valid domain, or it is unmapped.
func (i *Ingress) lookupCustomDomain(ctx context.Context, host string) (*core.CustomDomain, error) {
	if i.domains == nil {
		return nil, core.ErrNotFound
	}
	d := core.NormalizeDomain(host)
	if d == "" {
		return nil, core.ErrNotFound
	}
	return i.domains.Lookup(ctx, d)
}

// customDomainServable reports whether the token that owns a custom-domain
// mapping currently holds the mapped subdomain, anywhere in the cluster. The
// tunnel store is the authority across nodes; a session attached here is
// checked first because it needs no database round trip. With
// allowReserved, a reservation of the subdomain by the same token also
// counts (tls-ask pre-issues for reserved names; routing does not).
func (i *Ingress) customDomainServable(ctx context.Context, cd *core.CustomDomain, allowReserved bool) (bool, error) {
	if sess, ok := i.registry.Lookup(ctx, cd.Subdomain); ok {
		return sess.Tunnel().TokenID == cd.TokenID, nil
	}
	t, err := i.tunnels.GetBySubdomain(ctx, cd.Subdomain)
	switch {
	case err == nil:
		return t.TokenID == cd.TokenID, nil
	case !errors.Is(err, core.ErrNotFound):
		return false, err
	}
	if !allowReserved {
		return false, nil
	}
	res, err := i.reservations.Get(ctx, cd.Subdomain)
	switch {
	case err == nil:
		return res.TokenID == cd.TokenID, nil
	case errors.Is(err, core.ErrNotFound):
		return false, nil
	default:
		return false, err
	}
}

// target is where a public request's Host points.
type target struct {
	// sub is the tunnel subdomain to route to. Empty means the Host is one of
	// this server's own names that no tunnel serves: the base domain, the
	// gateway hostname, or a multi-label name under the base.
	sub string
	// custom is the BYO mapping the Host matched, or nil for a subdomain Host.
	// A custom-domain request is served only by a tunnel of custom.TokenID.
	custom *core.CustomDomain
}

// resolveHost classifies a request Host.
//
// public is true when the Host is a name visitors reach tunnels on: the base
// domain, anything under it, the gateway hostname, or a registered custom
// domain. It is false for everything else -- IP literals, single-label names
// such as the `riftd` container name, and unregistered domains -- which are
// the names Caddy's ask URL, health checks and operators use to reach this
// process directly.
//
// A custom-domain lookup that fails is treated as NOT public: a database blip
// must not turn a liveness probe addressed to a dotted internal name into a
// 404 that gets a healthy process restarted. Nothing is lost by it, since a
// custom domain cannot be routed without that same lookup anyway, and the
// internal routes reveal nothing while the store is unreachable.
func (i *Ingress) resolveHost(ctx context.Context, rawHost string) (t target, public bool) {
	host := normalizeHost(rawHost)
	if host == "" || net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return target{}, false
	}
	if core.IsServerHostname(host, i.cfg.Tunnel.BaseDomain, i.cfg.Gateway.Hostname) {
		if i.ownsHostname(host) {
			return target{}, true
		}
		sub, ok := core.SubdomainFromHost(host, i.cfg.Tunnel.BaseDomain)
		if !ok {
			return target{}, true
		}
		return target{sub: sub}, true
	}
	cd, err := i.lookupCustomDomain(ctx, host)
	switch {
	case err == nil:
		return target{sub: cd.Subdomain, custom: cd}, true
	case errors.Is(err, core.ErrNotFound):
		return target{}, false
	default:
		i.logger.Error("custom domain lookup failed", slog.String("domain", host), slog.Any("error", err))
		return target{}, false
	}
}

// normalizeHost lower-cases a Host header and strips any port, IPv6 brackets
// and trailing dot.
func normalizeHost(raw string) string {
	h := strings.TrimSpace(raw)
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// ownsHostname reports whether domain is a name this deployment serves in its
// own right, rather than a tunnel subdomain.
func (i *Ingress) ownsHostname(domain string) bool {
	if gw := strings.ToLower(i.cfg.Gateway.Hostname); gw != "" && domain == gw {
		return true
	}
	return domain == strings.ToLower(i.cfg.Tunnel.BaseDomain)
}

// servePublic routes a request from the public internet, whose Host has been
// resolved to t, into a tunnel.
func (i *Ingress) servePublic(w http.ResponseWriter, r *http.Request, t target) {
	if t.sub == "" {
		i.writeGatewayError(w, r, http.StatusNotFound, "not_a_tunnel",
			"This host does not correspond to a tunnel.")
		return
	}
	sub := t.sub
	servedName := core.Hostname(sub, i.cfg.Tunnel.BaseDomain)
	if t.custom != nil {
		// Never name the backing subdomain for a custom domain; the visitor
		// asked for the domain, and the subdomain is the owner's business.
		servedName = t.custom.Domain
	}
	notFound := func() {
		i.writeGatewayError(w, r, http.StatusNotFound, "tunnel_not_found",
			"No tunnel is currently serving "+servedName+".")
	}

	// Resolve the client once, here at the public edge, and pin it to the
	// request so the policy, rate limit and peer hop all use the same value.
	// A client-supplied copy of the peer-hop client header is dropped first:
	// it is believed only from an authenticated peer.
	r.Header.Del(config.HeaderRiftClientIP)
	r = withClientIP(r, i.resolveClientIP(r))

	// Annotate once, here at the public edge, so the local service behind the
	// tunnel learns who actually connected. This must not happen again on the
	// internal peer hop (handleInternalProxy), or a forwarding node's own
	// address would overwrite the real client's.
	i.annotateForwarded(r)
	upgrade := isUpgradeRequest(r)

	ctx := r.Context()
	if sess, found := i.registry.Lookup(ctx, sub); found {
		// E1: a custom domain is served only by its owner's tunnel. Whoever
		// else now holds the subdomain gets nothing for it.
		if t.custom != nil && sess.Tunnel().TokenID != t.custom.TokenID {
			notFound()
			return
		}
		if upgrade {
			i.proxyUpgrade(w, r, sess, sub)
		} else {
			i.proxy(w, r, sess, sub)
		}
		return
	}

	if t.custom != nil {
		// Not attached here; ask the tunnel store (the cross-node authority)
		// who holds the subdomain before forwarding anywhere.
		owned, err := i.customDomainServable(ctx, t.custom, false)
		if err != nil {
			i.logger.Error("custom domain owner check failed",
				slog.String("domain", t.custom.Domain), slog.Any("error", err))
		}
		if !owned {
			notFound()
			return
		}
	}

	nodeURL, remote, err := i.registry.LocatePeer(ctx, sub)
	if err != nil {
		i.logger.Error("peer lookup failed", slog.String("subdomain", sub), slog.Any("error", err))
	} else if remote {
		if upgrade {
			// A WebSocket needs a hijacked, full-duplex socket, which the
			// node-to-node HTTP forward cannot carry. The agent must be attached
			// to the node the client reached.
			i.writeGatewayError(w, r, http.StatusBadGateway, "upgrade_not_local",
				"This tunnel is served by another node; rift cannot yet carry a connection upgrade across nodes.")
			return
		}
		i.forwardToPeer(w, r, nodeURL, sub)
		return
	}

	notFound()
}

// handleInternalProxy serves a request another node forwarded to us. It never
// forwards onward, so a stale Redis lease cannot create a routing loop.
func (i *Ingress) handleInternalProxy(w http.ResponseWriter, r *http.Request) {
	if !i.cfg.Redis.Enabled {
		http.NotFound(w, r)
		return
	}
	if !authenticatePeer(r, i.cfg.Cluster.PeerSecret) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// The forwarding node advertises its protocol version. An older node omits
	// the header, which we treat as compatible; a value outside our supported
	// range means the cluster is mid-upgrade across a breaking change and this
	// hop is not safe to serve.
	if v := r.Header.Get(config.HeaderRiftProtoVersion); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < tunnelproto.MinVersion || n > tunnelproto.Version {
			i.logger.Warn("peer forwarded with an incompatible protocol version", slog.String("peer_version", v))
			http.Error(w, "incompatible peer protocol version", http.StatusBadGateway)
			return
		}
	}
	sub := r.Header.Get(config.HeaderRiftSubdomain)
	if sub == "" {
		http.Error(w, "missing "+config.HeaderRiftSubdomain, http.StatusBadRequest)
		return
	}

	// The socket peer is the forwarding node, so the visitor's address comes
	// from the edge's stamp -- trustworthy because the hop is authenticated.
	// A peer that predates the stamp still set X-Real-IP at its edge; failing
	// both, the forwarding node's own address is all there is.
	clientIP := parseIPEntry(r.Header.Get(config.HeaderRiftClientIP))
	if clientIP == "" {
		clientIP = parseIPEntry(r.Header.Get(config.HeaderRealIP))
	}
	if clientIP == "" {
		clientIP = socketIP(r)
	}
	r.Header.Del(config.HeaderRiftClientIP)
	r.Header.Set(config.HeaderRealIP, clientIP)
	r = withClientIP(r, clientIP)

	sess, found := i.registry.Lookup(r.Context(), sub)
	if !found {
		// The lease was stale. Telling the peer plainly beats a 404 that it
		// would relay to the public client as "no such tunnel".
		http.Error(w, "tunnel not attached to this node", http.StatusServiceUnavailable)
		return
	}

	// Restore the original request target. Without this the agent would receive
	// RouteInternalProxy as the path instead of what the public client asked
	// for, and the local service would answer the wrong route.
	if fwd := r.Header.Get(config.HeaderRiftForwardedURI); fwd != "" {
		if u, err := url.ParseRequestURI(fwd); err == nil {
			r.URL.Path = u.Path
			r.URL.RawPath = u.RawPath
			r.URL.RawQuery = u.RawQuery
		} else {
			http.Error(w, "invalid "+config.HeaderRiftForwardedURI, http.StatusBadRequest)
			return
		}
	}
	r.Header.Del(config.HeaderRiftForwardedURI)
	r.Header.Del(config.HeaderRiftPeerToken)
	r.Header.Del(config.HeaderRiftSubdomain)
	r.Header.Del(config.HeaderRiftProtoVersion)

	i.proxy(w, r, sess, sub)
}

// annotateForwarded adds the standard reverse-proxy headers describing the
// public client, so the local service sees the caller rather than the gateway.
//
// X-Forwarded-For follows the usual proxy convention (nginx's
// $proxy_add_x_forwarded_for, Go's httputil.ReverseProxy): when the immediate
// sender is a trusted proxy its chain is kept and the sender's address is
// appended, so a service that walks the chain right to left, as clientIP
// does, finds the client. When the sender is NOT trusted, whatever chain it
// sent is discarded and replaced with its socket address: preserving it would
// hand the service a client-written "original client" to believe.
//
// X-Real-IP always carries the resolved client address, which honours the
// trusted-proxy allowlist and so is the value to trust. Proto and Host are
// filled only if absent, leaving an upstream's values intact.
func (i *Ingress) annotateForwarded(r *http.Request) {
	clientIP := i.clientIP(r)
	sender := socketIP(r)
	if i.isTrustedProxy(sender) {
		chain := strings.Join(forwardedForEntries(r.Header), ", ")
		if chain == "" && clientIP != sender {
			// The proxy named the client only in X-Real-IP; keep that fact.
			chain = clientIP
		}
		if chain != "" {
			chain += ", "
		}
		r.Header.Set(config.HeaderForwardedFor, chain+sender)
	} else {
		r.Header.Set(config.HeaderForwardedFor, sender)
	}
	r.Header.Set(config.HeaderRealIP, clientIP)
	if r.Header.Get(config.HeaderForwardedProto) == "" {
		r.Header.Set(config.HeaderForwardedProto, i.cfg.Tunnel.PublicScheme)
	}
	if r.Header.Get(config.HeaderForwardedHost) == "" {
		r.Header.Set(config.HeaderForwardedHost, r.Host)
	}
}

// proxy ships one request through the tunnel and streams the response back.
func (i *Ingress) proxy(w http.ResponseWriter, r *http.Request, sess core.Session, sub string) {
	// Visitor-access policy runs here (not only in servePublic) so a
	// peer-forwarded request, which reaches proxy via handleInternalProxy, is
	// checked too.
	if !i.enforce(w, r, sess, sub) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), i.cfg.Tunnel.RequestTimeout)
	defer cancel()

	outbound := r.Clone(ctx)
	outbound.RemoteAddr = i.clientIP(r)

	if i.cfg.Tunnel.MaxRequestBodyBytes > 0 && outbound.Body != nil {
		outbound.Body = http.MaxBytesReader(w, outbound.Body, i.cfg.Tunnel.MaxRequestBodyBytes)
	}

	resp, err := sess.RoundTrip(outbound)
	if err != nil {
		i.writeRoundTripError(w, r, sub, err)
		return
	}
	i.relayResponse(w, resp, sub, "tunnel")
}

// relayResponse writes resp to the public client -- headers, status, then the
// body streamed with flushes -- and closes its body. It is the one relay for a
// tunnel response, a declined upgrade and a peer node's response. source
// names which, for the log.
func (i *Ingress) relayResponse(w http.ResponseWriter, resp *http.Response, sub, source string) {
	defer func() { _ = resp.Body.Close() }()

	header := w.Header()
	for k, vs := range resp.Header {
		for _, v := range vs {
			header.Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	if err := streamBody(w, resp.Body); err != nil {
		// Headers are already on the wire; there is no status left to send.
		i.logger.Debug("response stream ended early", slog.String("source", source),
			slog.String("subdomain", sub), slog.Any("error", err))
	}
}

// streamBody copies the tunnel response to the client, flushing so that
// server-sent events and other incremental responses are not buffered.
func streamBody(w http.ResponseWriter, body io.Reader) error {
	rc := http.NewResponseController(w)
	buf := make([]byte, copyBufferSize)

	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			// A flush error means the client is gone or the writer does not
			// support flushing; neither is worth aborting the copy for.
			_ = rc.Flush()
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// writeRoundTripError turns a tunnel failure into the right public status.
func (i *Ingress) writeRoundTripError(w http.ResponseWriter, r *http.Request, sub string, err error) {
	// The public client hung up; there is nobody left to answer.
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		return
	}

	status, code, msg := http.StatusBadGateway, "tunnel_error", "The tunnel could not serve this request."

	if rc, ok := tunnelproto.ResetCodeOf(err); ok {
		switch rc {
		case tunnelproto.ResetUpstreamError:
			status, code, msg = http.StatusBadGateway, "upstream_error",
				"The local service behind this tunnel refused the connection."
		case tunnelproto.ResetUpstreamTimeout:
			status, code, msg = http.StatusGatewayTimeout, "upstream_timeout",
				"The local service behind this tunnel did not respond in time."
		case tunnelproto.ResetPayloadTooLarge:
			status, code, msg = http.StatusRequestEntityTooLarge, "payload_too_large",
				"The request body was too large."
		case tunnelproto.ResetClientDisconnected:
			return
		}
	} else {
		// MaxBytesReader trips inside the goroutine pumping the body to the
		// agent, so the cap surfaces here as a read error rather than as a
		// reset from the agent. Without this it would fall through to the
		// default 502, which tells the client nothing about what to fix.
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			status, code, msg = http.StatusRequestEntityTooLarge, "payload_too_large",
				"The request body was too large."
		case errors.Is(err, context.DeadlineExceeded):
			status, code, msg = http.StatusGatewayTimeout, "upstream_timeout",
				"The local service behind this tunnel did not respond in time."
		case errors.Is(err, core.ErrTunnelUnavailable):
			status, code, msg = http.StatusBadGateway, "tunnel_unavailable",
				"The tunnel disconnected while the request was in flight."
		}
	}

	i.logger.Info("tunnel request failed",
		slog.String("subdomain", sub),
		slog.Int("status", status),
		slog.Any("error", err))
	i.writeGatewayError(w, r, status, code, msg)
}

func (i *Ingress) writeGatewayError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	// A JSON client wants a machine-readable body regardless of any branding.
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSONError(w, status, code, message)
		return
	}
	// T4: a branded HTML page, if one is configured for this status. The
	// message is NOT always a constant -- tunnel_not_found names the requested
	// host, which comes from the client -- so render escapes every value.
	if i.errorPages != nil {
		if body, ok := i.errorPages.render(status, code, message); ok {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, message+"\n")
}
