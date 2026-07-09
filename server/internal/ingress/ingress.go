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
	"strings"
	"time"

	"github.com/anomaly-sh/rift/server/internal/config"
	"github.com/anomaly-sh/rift/server/internal/core"
	"github.com/anomaly-sh/rift/server/internal/tunnelproto"
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

	// peers forwards to another node when Redis says it owns the subdomain.
	peers *http.Client

	trusted []netAddr

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
) *Ingress {
	return &Ingress{
		cfg:          cfg,
		logger:       logger.With(slog.String("component", "ingress")),
		registry:     reg,
		tunnels:      tunnels,
		reservations: reservations,
		peers: &http.Client{
			// No client timeout: a tunnelled response may legitimately stream
			// for a long time. The per-request context carries the deadline.
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConnsPerHost:   32,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: cfg.Tunnel.RequestTimeout,
			},
		},
		trusted: parseTrusted(cfg.Ingress.TrustedProxyIPs),
	}
}

// SetReadyCheck installs the readiness probe's dependency check. Call it
// before serving.
func (i *Ingress) SetReadyCheck(fn ReadyFunc) { i.ready = fn }

// Handler mounts the public routes plus the internal endpoints Caddy and peer
// nodes use.
func (i *Ingress) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(config.RouteHealth, i.handleHealth)
	mux.HandleFunc(config.RouteReady, i.handleReady)
	mux.HandleFunc(config.RouteTLSAsk, i.handleTLSAsk)
	mux.HandleFunc(config.RouteInternalProxy, i.handleInternalProxy)
	mux.HandleFunc("/", i.handlePublic)
	return mux
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
// explicitly reserved gets a certificate.
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

	sub, ok := core.SubdomainFromHost(domain, i.cfg.Tunnel.BaseDomain)
	if !ok {
		i.logger.Debug("refusing certificate for foreign domain", slog.String("domain", domain))
		http.Error(w, "domain is not served by this host", http.StatusForbidden)
		return
	}

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

// ownsHostname reports whether domain is a name this deployment serves in its
// own right, rather than a tunnel subdomain.
func (i *Ingress) ownsHostname(domain string) bool {
	if i.cfg.Gateway.Hostname != "" && domain == i.cfg.Gateway.Hostname {
		return true
	}
	return domain == strings.ToLower(i.cfg.Tunnel.BaseDomain)
}

// handlePublic routes a request from the public internet into a tunnel.
func (i *Ingress) handlePublic(w http.ResponseWriter, r *http.Request) {
	sub, ok := core.SubdomainFromHost(r.Host, i.cfg.Tunnel.BaseDomain)
	if !ok {
		i.writeGatewayError(w, r, http.StatusNotFound, "not_a_tunnel",
			"This host does not correspond to a tunnel.")
		return
	}

	ctx := r.Context()
	if sess, found := i.registry.Lookup(ctx, sub); found {
		i.proxy(w, r, sess, sub)
		return
	}

	nodeURL, remote, err := i.registry.LocatePeer(ctx, sub)
	if err != nil {
		i.logger.Error("peer lookup failed", slog.String("subdomain", sub), slog.Any("error", err))
	} else if remote {
		i.forwardToPeer(w, r, nodeURL, sub)
		return
	}

	i.writeGatewayError(w, r, http.StatusNotFound, "tunnel_not_found",
		"No tunnel is currently serving "+core.Hostname(sub, i.cfg.Tunnel.BaseDomain)+".")
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
	sub := r.Header.Get(config.HeaderRiftSubdomain)
	if sub == "" {
		http.Error(w, "missing "+config.HeaderRiftSubdomain, http.StatusBadRequest)
		return
	}
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

	i.proxy(w, r, sess, sub)
}

// proxy ships one request through the tunnel and streams the response back.
func (i *Ingress) proxy(w http.ResponseWriter, r *http.Request, sess core.Session, sub string) {
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
		i.logger.Debug("response stream ended early",
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
		switch {
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
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSONError(w, status, code, message)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, message+"\n")
}
