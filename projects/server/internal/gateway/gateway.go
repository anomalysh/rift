// Package gateway terminates agent WebSocket connections and turns each one
// into a core.Session the ingress can route public requests through.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/anomalysh/rift/projects/server/internal/auth"
	"github.com/anomalysh/rift/projects/server/internal/config"
	"github.com/anomalysh/rift/projects/server/internal/core"
	"github.com/anomalysh/rift/projects/server/internal/policy"
	"github.com/anomalysh/rift/projects/server/internal/tunnelproto"
)

// subdomainGenerationAttempts bounds retries when the server allocates a
// random label and loses a race to another agent.
const subdomainGenerationAttempts = 5

// Gateway accepts agent connections.
type Gateway struct {
	cfg          *config.Config
	logger       *slog.Logger
	tokens       core.TokenStore
	reservations core.ReservationStore
	tunnels      core.TunnelStore
	domains      core.DomainStore
	registry     core.Registry

	// tcp accepts public TCP connections for tcp tunnels. Nil when tcp tunnels
	// are disabled.
	tcp *tcpForwarder

	// udp accepts public UDP datagrams for udp tunnels. Nil when udp tunnels
	// are disabled.
	udp *udpForwarder

	mu       sync.Mutex
	sessions map[*session]struct{}
}

// New builds a Gateway. It does not listen; mount Handler on a mux.
func New(
	cfg *config.Config,
	logger *slog.Logger,
	tokens core.TokenStore,
	reservations core.ReservationStore,
	tunnels core.TunnelStore,
	domains core.DomainStore,
	reg core.Registry,
) *Gateway {
	return &Gateway{
		cfg:          cfg,
		logger:       logger.With(slog.String("component", "gateway")),
		tokens:       tokens,
		reservations: reservations,
		tunnels:      tunnels,
		domains:      domains,
		registry:     reg,
		tcp:          newTCPForwarder(cfg, logger),
		udp:          newUDPForwarder(cfg, logger),
		sessions:     make(map[*session]struct{}),
	}
}

// Handler upgrades the request and serves one tunnel for its lifetime.
func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols:   []string{tunnelproto.Subprotocol},
			OriginPatterns: g.cfg.Gateway.AllowedOrigins,
			// Agents are not browsers. When no origins are configured the
			// upgrade must still succeed for a request that carries no Origin
			// header, which is the default behaviour we rely on.
		})
		if err != nil {
			g.logger.Debug("websocket upgrade failed", slog.Any("error", err))
			return
		}
		conn.SetReadLimit(int64(tunnelproto.MaxFrameBytes))

		if conn.Subprotocol() != tunnelproto.Subprotocol {
			_ = conn.Close(websocket.StatusPolicyViolation,
				fmt.Sprintf("expected subprotocol %s", tunnelproto.Subprotocol))
			return
		}

		g.serve(r, conn)
	})
}

func (g *Gateway) serve(r *http.Request, conn *websocket.Conn) {
	// The handshake gets its own deadline; the tunnel itself must outlive the
	// inbound request context, which net/http cancels when the handler returns.
	hsCtx, cancel := context.WithTimeout(r.Context(), g.cfg.Gateway.HandshakeTimeout)
	defer cancel()

	hello, err := readHello(hsCtx, conn)
	if err != nil {
		g.logger.Debug("handshake read failed", slog.Any("error", err))
		_ = conn.Close(websocket.StatusPolicyViolation, "handshake failed")
		return
	}

	tunnel, token, herr := g.authorize(hsCtx, r, hello)
	if herr != nil {
		g.rejectHandshake(hsCtx, conn, herr)
		return
	}

	sessLogger := g.logger.With(
		slog.String("tunnel_id", tunnel.ID),
		slog.String("subdomain", tunnel.Subdomain),
		slog.String("token_id", token.ID),
	)
	sess := newSession(conn, *tunnel, g.cfg, g.tunnels, g.tokens, sessLogger)

	// The session outlives the HTTP handler that created it.
	runCtx, runCancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer runCancel()

	displaced, err := g.registry.Register(runCtx, sess)
	if err != nil {
		g.logger.Error("could not register tunnel", slog.Any("error", err))
		g.rejectHandshake(hsCtx, conn, &handshakeError{code: tunnelproto.ErrCodeInternal, message: "could not register tunnel"})
		_ = g.tunnels.Release(runCtx, tunnel.ID)
		return
	}
	if displaced != nil {
		_ = displaced.Close(string(tunnelproto.ShutdownReplaced))
	}

	g.track(sess)
	defer g.untrack(sess)

	// A raw tunnel (tcp/tls) is reached at a host:port, not the http URL. Work
	// it out now, while we can still cleanly reject the handshake on failure.
	bindAddr := ""
	switch tunnel.Protocol {
	case core.ProtocolTCP:
		if g.tcp == nil {
			g.rejectAfterRegister(hsCtx, runCtx, conn, sess, tunnelproto.ErrCodeUnsupportedProtocol,
				"tcp tunnels are not enabled on this server")
			return
		}
		addr, err := g.tcp.bind(sess)
		if err != nil {
			g.logger.Error("could not allocate tcp port", slog.Any("error", err))
			g.rejectAfterRegister(hsCtx, runCtx, conn, sess, tunnelproto.ErrCodeInternal,
				"could not allocate a public tcp port")
			return
		}
		bindAddr = addr
		defer g.tcp.release(sess)

	case core.ProtocolUDP:
		if g.udp == nil {
			g.rejectAfterRegister(hsCtx, runCtx, conn, sess, tunnelproto.ErrCodeUnsupportedProtocol,
				"udp tunnels are not enabled on this server")
			return
		}
		addr, err := g.udp.bind(sess)
		if err != nil {
			g.logger.Error("could not allocate udp port", slog.Any("error", err))
			g.rejectAfterRegister(hsCtx, runCtx, conn, sess, tunnelproto.ErrCodeInternal,
				"could not allocate a public udp port")
			return
		}
		bindAddr = addr
		defer g.udp.release(sess)

	case core.ProtocolTLS:
		if !g.cfg.TLSTunnel.Enabled {
			g.rejectAfterRegister(hsCtx, runCtx, conn, sess, tunnelproto.ErrCodeUnsupportedProtocol,
				"tls tunnels are not enabled on this server")
			return
		}
		// A tls tunnel needs no per-tunnel listener: the shared SNI-routed
		// listener multiplexes them. It is reached at its subdomain host.
		bindAddr = net.JoinHostPort(
			core.Hostname(tunnel.Subdomain, g.cfg.Tunnel.BaseDomain),
			strconv.Itoa(g.cfg.TLSTunnel.Port()))

	case core.ProtocolGRPC:
		if !g.cfg.GRPC.Enabled {
			g.rejectAfterRegister(hsCtx, runCtx, conn, sess, tunnelproto.ErrCodeUnsupportedProtocol,
				"grpc tunnels are not enabled on this server")
			return
		}
		// Like tls, a grpc tunnel needs no per-tunnel listener: the shared h2c
		// listener routes by :authority. It is reached at its subdomain host.
		bindAddr = net.JoinHostPort(
			core.Hostname(tunnel.Subdomain, g.cfg.Tunnel.BaseDomain),
			strconv.Itoa(g.cfg.GRPC.Port()))
	}

	ok := tunnelproto.HelloOK{
		TunnelID:            tunnel.ID,
		Subdomain:           tunnel.Subdomain,
		Hostname:            core.Hostname(tunnel.Subdomain, g.cfg.Tunnel.BaseDomain),
		URL:                 g.cfg.Tunnel.PublicURL(tunnel.Subdomain),
		HeartbeatIntervalMS: g.cfg.Tunnel.HeartbeatInterval.Milliseconds(),
		BindAddr:            bindAddr,
		ProtocolVersion:     tunnelproto.Version,
	}
	frame, err := tunnelproto.EncodeControl(tunnelproto.ControlHelloOK, ok)
	if err != nil {
		g.logger.Error("could not encode hello_ok", slog.Any("error", err))
		_ = conn.Close(websocket.StatusInternalError, "internal error")
		return
	}
	if err := conn.Write(hsCtx, websocket.MessageBinary, frame); err != nil {
		g.logger.Debug("could not send hello_ok", slog.Any("error", err))
		// The session's loops never started, so nothing else owns this socket.
		_ = conn.CloseNow()
		g.cleanupSession(runCtx, sess)
		return
	}

	sessLogger.Info("tunnel established", slog.String("url", ok.URL))

	sess.wg.Add(3)
	go sess.writeLoop()
	go sess.watchdog(runCtx)
	go sess.readLoop(runCtx)

	// Stop routing as soon as teardown is *requested*, not once the socket has
	// finished closing. An unresponsive agent can drag the close handshake out,
	// and until the session leaves the registry its subdomain answers 502
	// instead of 404 and cannot be reclaimed.
	<-sess.closing
	cleanupCtx := context.WithoutCancel(runCtx)
	g.cleanupSession(cleanupCtx, sess)

	sess.wg.Wait()
	sessLogger.Info("tunnel closed", slog.String("reason", closeReasonOf(sess)))
}

func closeReasonOf(s *session) string {
	if v, ok := s.closeReason.Load().(string); ok {
		return v
	}
	return "unknown"
}

// cleanupSession detaches the session from routing and drops its store row,
// but only if this session is still the current holder of the subdomain.
func (g *Gateway) cleanupSession(ctx context.Context, s *session) {
	ctx, cancel := context.WithTimeout(ctx, g.cfg.Gateway.WriteTimeout)
	defer cancel()

	if err := g.registry.Unregister(ctx, s); err != nil {
		g.logger.Warn("could not unregister tunnel", slog.Any("error", err))
	}
	// Release is keyed by tunnel ID, so a session displaced by a reconnecting
	// agent deletes its own row and never the replacement's.
	if err := g.tunnels.Release(ctx, s.tunnel.ID); err != nil {
		g.logger.Warn("could not release tunnel row", slog.Any("error", err))
	}
}

func (g *Gateway) track(s *session) {
	g.mu.Lock()
	g.sessions[s] = struct{}{}
	g.mu.Unlock()
}

func (g *Gateway) untrack(s *session) {
	g.mu.Lock()
	delete(g.sessions, s)
	g.mu.Unlock()
}

// Shutdown closes every live tunnel, telling agents the server is going away
// so they reconnect rather than treat it as a fatal error.
func (g *Gateway) Shutdown(context.Context) error {
	g.mu.Lock()
	sessions := make([]*session, 0, len(g.sessions))
	for s := range g.sessions {
		sessions = append(sessions, s)
	}
	g.mu.Unlock()

	for _, s := range sessions {
		_ = s.Close(string(tunnelproto.ShutdownServerShutdown))
	}
	return nil
}

// handshakeError carries a rejection back to the agent.
type handshakeError struct {
	code    tunnelproto.ErrorCode
	message string
}

func (e *handshakeError) Error() string { return string(e.code) + ": " + e.message }

// rejectAfterRegister tears down a session that was registered but cannot be
// served (e.g. its protocol is disabled or no port is free), then rejects the
// handshake so the agent learns why.
func (g *Gateway) rejectAfterRegister(hsCtx, runCtx context.Context, conn *websocket.Conn, sess *session, code tunnelproto.ErrorCode, message string) {
	g.cleanupSession(runCtx, sess)
	g.rejectHandshake(hsCtx, conn, &handshakeError{code: code, message: message})
}

func (g *Gateway) rejectHandshake(ctx context.Context, conn *websocket.Conn, herr *handshakeError) {
	frame, err := tunnelproto.EncodeControl(tunnelproto.ControlHelloError,
		tunnelproto.HelloError{Code: herr.code, Message: herr.message})
	if err == nil {
		_ = conn.Write(ctx, websocket.MessageBinary, frame)
	}
	_ = conn.Close(websocket.StatusNormalClosure, string(herr.code))
	g.logger.Info("handshake rejected", slog.String("code", string(herr.code)), slog.String("reason", herr.message))
}

func readHello(ctx context.Context, conn *websocket.Conn) (*tunnelproto.Hello, error) {
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageBinary {
		return nil, errors.New("gateway: first message was not binary")
	}
	frame, err := tunnelproto.Decode(data)
	if err != nil {
		return nil, err
	}
	if frame.Type != tunnelproto.FrameControl {
		return nil, fmt.Errorf("gateway: first frame was %s, want CONTROL", frame.Type)
	}
	env, err := tunnelproto.DecodeControl(frame.Payload)
	if err != nil {
		return nil, err
	}
	if env.Type != tunnelproto.ControlHello {
		return nil, fmt.Errorf("gateway: first control message was %q, want %q", env.Type, tunnelproto.ControlHello)
	}
	var hello tunnelproto.Hello
	if err := tunnelproto.UnmarshalPayload(env, &hello); err != nil {
		return nil, err
	}
	return &hello, nil
}

// authorize validates the hello, claims a subdomain, and persists the tunnel.
func (g *Gateway) authorize(ctx context.Context, r *http.Request, hello *tunnelproto.Hello) (*core.Tunnel, *core.Token, *handshakeError) {
	// Accept any agent inside the supported protocol range. Outside it, reject
	// with a message that points at the side that needs upgrading -- retrying
	// cannot fix a version gap, so the agent treats this as fatal.
	if hello.ProtocolVersion < tunnelproto.MinVersion || hello.ProtocolVersion > tunnelproto.Version {
		var message string
		if hello.ProtocolVersion > tunnelproto.Version {
			message = fmt.Sprintf("this gateway speaks protocol up to v%d but the agent offered v%d; upgrade the gateway",
				tunnelproto.Version, hello.ProtocolVersion)
		} else {
			message = fmt.Sprintf("this gateway requires protocol v%d-v%d but the agent offered v%d; upgrade rift",
				tunnelproto.MinVersion, tunnelproto.Version, hello.ProtocolVersion)
		}
		return nil, nil, &handshakeError{code: tunnelproto.ErrCodeUnsupportedVersion, message: message}
	}
	if !core.Protocol(hello.Protocol).Valid() {
		return nil, nil, &handshakeError{
			code:    tunnelproto.ErrCodeUnsupportedProtocol,
			message: fmt.Sprintf("protocol %q is not supported; this server serves %q", hello.Protocol, core.ProtocolHTTP),
		}
	}

	now := time.Now()
	token, err := auth.Authenticate(ctx, g.tokens, hello.Token, now)
	if err != nil {
		return nil, nil, &handshakeError{code: tunnelproto.ErrCodeUnauthorized, message: "invalid or expired token"}
	}
	if err := g.tokens.TouchLastUsed(ctx, token.ID, now); err != nil {
		g.logger.Warn("could not record token use", slog.Any("error", err))
	}

	tunnelID, err := core.NewID(now)
	if err != nil {
		return nil, nil, &handshakeError{code: tunnelproto.ErrCodeInternal, message: "could not allocate tunnel id"}
	}

	tunnel := &core.Tunnel{
		ID:          tunnelID,
		TokenID:     token.ID,
		Protocol:    core.Protocol(hello.Protocol),
		LocalPort:   hello.LocalPort,
		NodeID:      g.cfg.NodeID,
		ClientAddr:  r.RemoteAddr,
		ConnectedAt: now,
		LastSeenAt:  now,
	}
	// Carry the agent's declared visitor-access policy (basic-auth, IP rules,
	// rate limit, lifetime bounds). Absent -> the zero policy, which enforces
	// nothing. The ingress reads it via sess.Tunnel().Policy. Reject a
	// malformed policy at connect so it cannot 500 every visitor later.
	if hello.Policy != nil {
		if err := policy.Validate(*hello.Policy); err != nil {
			return nil, nil, &handshakeError{
				code:    tunnelproto.ErrCodeInvalidPolicy,
				message: fmt.Sprintf("invalid policy: %v", err),
			}
		}
		tunnel.Policy = *hello.Policy
	}

	requested := core.NormalizeSubdomain(hello.Subdomain)
	if requested == "" {
		if herr := g.claimGenerated(ctx, tunnel, token); herr != nil {
			return nil, nil, herr
		}
	} else if herr := g.claimRequested(ctx, tunnel, token, requested); herr != nil {
		return nil, nil, herr
	}

	// E1: register any BYO custom domains against the now-claimed subdomain, so
	// the TLS-ask endpoint will authorize a certificate and the ingress can
	// route the domain to this tunnel.
	if herr := g.registerDomains(ctx, hello.Domains, tunnel.Subdomain, token.ID); herr != nil {
		return nil, nil, herr
	}
	return tunnel, token, nil
}

// registerDomains upserts each requested custom domain against the tunnel's
// subdomain. It rejects a malformed domain, one that lies under the base domain
// (that is a subdomain, requested differently), or one owned by another token.
func (g *Gateway) registerDomains(ctx context.Context, domains []string, subdomain, tokenID string) *handshakeError {
	if len(domains) == 0 {
		return nil
	}
	if g.domains == nil {
		return &handshakeError{
			code:    tunnelproto.ErrCodeInvalidDomain,
			message: "custom domains are not supported by this server",
		}
	}
	now := time.Now()
	for _, raw := range domains {
		d := core.NormalizeDomain(raw)
		if d == "" {
			return &handshakeError{
				code:    tunnelproto.ErrCodeInvalidDomain,
				message: fmt.Sprintf("invalid custom domain %q", raw),
			}
		}
		if _, ok := core.SubdomainFromHost(d, g.cfg.Tunnel.BaseDomain); ok {
			return &handshakeError{
				code:    tunnelproto.ErrCodeInvalidDomain,
				message: fmt.Sprintf("%q is under this server's base domain; request it as a subdomain instead", d),
			}
		}
		err := g.domains.Upsert(ctx, core.CustomDomain{
			Domain: d, Subdomain: subdomain, TokenID: tokenID, CreatedAt: now,
		})
		switch {
		case errors.Is(err, core.ErrDomainOwned):
			return &handshakeError{
				code:    tunnelproto.ErrCodeDomainOwned,
				message: fmt.Sprintf("custom domain %q is registered to another token", d),
			}
		case err != nil:
			g.logger.Error("could not register custom domain", slog.String("domain", d), slog.Any("error", err))
			return &handshakeError{code: tunnelproto.ErrCodeInternal, message: "could not register custom domain"}
		}
	}
	return nil
}

// claimGenerated allocates a random label, retrying on collision.
func (g *Gateway) claimGenerated(ctx context.Context, tunnel *core.Tunnel, token *core.Token) *handshakeError {
	if herr := g.checkTunnelLimit(ctx, token); herr != nil {
		return herr
	}
	for attempt := 0; attempt < subdomainGenerationAttempts; attempt++ {
		sub, err := g.cfg.SubdomainRules.GenerateSubdomain()
		if err != nil {
			return &handshakeError{code: tunnelproto.ErrCodeInternal, message: "could not generate a subdomain"}
		}
		// A generated label must not collide with someone's reservation.
		if _, err := g.reservations.Get(ctx, sub); err == nil {
			continue
		} else if !errors.Is(err, core.ErrNotFound) {
			return &handshakeError{code: tunnelproto.ErrCodeInternal, message: "could not check reservations"}
		}

		tunnel.Subdomain = sub
		err = g.tunnels.Claim(ctx, tunnel)
		if err == nil {
			return nil
		}
		if !errors.Is(err, core.ErrSubdomainTaken) {
			g.logger.Error("could not claim generated subdomain", slog.Any("error", err))
			return &handshakeError{code: tunnelproto.ErrCodeInternal, message: "could not claim a subdomain"}
		}
	}
	return &handshakeError{code: tunnelproto.ErrCodeInternal, message: "could not find a free subdomain"}
}

// claimRequested claims a specific label on behalf of token.
func (g *Gateway) claimRequested(ctx context.Context, tunnel *core.Tunnel, token *core.Token, sub string) *handshakeError {
	if err := g.cfg.SubdomainRules.Validate(sub); err != nil {
		if errors.Is(err, core.ErrSubdomainReserved) {
			return &handshakeError{code: tunnelproto.ErrCodeSubdomainReserved, message: err.Error()}
		}
		return &handshakeError{code: tunnelproto.ErrCodeSubdomainInvalid, message: err.Error()}
	}

	// A reservation owned by someone else is an absolute bar.
	res, err := g.reservations.Get(ctx, sub)
	switch {
	case err == nil && res.TokenID != token.ID:
		return &handshakeError{
			code:    tunnelproto.ErrCodeSubdomainReserved,
			message: fmt.Sprintf("subdomain %q is reserved", sub),
		}
	case err != nil && !errors.Is(err, core.ErrNotFound):
		return &handshakeError{code: tunnelproto.ErrCodeInternal, message: "could not check reservations"}
	}

	// Take over our own stale tunnel *before* counting against the limit.
	// An agent reconnecting after a dropped socket still owns a tunnel row
	// that the reaper has not collected yet; counting it would make a token
	// with max_tunnels=1 unable to ever reconnect to its own subdomain.
	existing, err := g.tunnels.GetBySubdomain(ctx, sub)
	switch {
	case err == nil && existing.TokenID == token.ID:
		if err := g.tunnels.Release(ctx, existing.ID); err != nil {
			return &handshakeError{code: tunnelproto.ErrCodeInternal, message: "could not reclaim the previous tunnel"}
		}
	case err == nil:
		return &handshakeError{
			code:    tunnelproto.ErrCodeSubdomainTaken,
			message: fmt.Sprintf("subdomain %q is in use", sub),
		}
	case !errors.Is(err, core.ErrNotFound):
		return &handshakeError{code: tunnelproto.ErrCodeInternal, message: "could not check active tunnels"}
	}

	if herr := g.checkTunnelLimit(ctx, token); herr != nil {
		return herr
	}

	tunnel.Subdomain = sub
	if err := g.tunnels.Claim(ctx, tunnel); err != nil {
		if errors.Is(err, core.ErrSubdomainTaken) {
			return &handshakeError{
				code:    tunnelproto.ErrCodeSubdomainTaken,
				message: fmt.Sprintf("subdomain %q is in use", sub),
			}
		}
		g.logger.Error("could not claim subdomain", slog.Any("error", err))
		return &handshakeError{code: tunnelproto.ErrCodeInternal, message: "could not claim the subdomain"}
	}
	return nil
}

func (g *Gateway) checkTunnelLimit(ctx context.Context, token *core.Token) *handshakeError {
	limit := token.MaxTunnels
	if limit <= 0 {
		limit = g.cfg.Tunnel.MaxTunnelsPerToken
	}
	n, err := g.tunnels.CountByToken(ctx, token.ID)
	if err != nil {
		return &handshakeError{code: tunnelproto.ErrCodeInternal, message: "could not count active tunnels"}
	}
	if n >= limit {
		return &handshakeError{
			code:    tunnelproto.ErrCodeTunnelLimit,
			message: fmt.Sprintf("token already holds %d of %d allowed tunnels", n, limit),
		}
	}
	return nil
}
