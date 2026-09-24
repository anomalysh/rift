package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/anomalysh/rift/projects/server/internal/core"
)

// passthrough describes a shared public listener that routes each raw
// connection to a tunnel by a host name peeked from its opening bytes (the TLS
// ClientHello SNI, or the h2c :authority) and then pipes the connection
// through untouched, so the agent's local service does the real handshake.
type passthrough struct {
	// server names the listener in logs.
	server string
	// protocol is the only tunnel protocol this listener may reach. A tunnel
	// of another protocol that merely shares the subdomain does not expect
	// these bytes.
	protocol core.Protocol
	// peek reads just enough of conn to name the host it wants, and returns
	// every byte it consumed so they can be replayed to the agent. It parses
	// untrusted input.
	peek func(conn net.Conn) (host string, buffered []byte, err error)
}

// servePassthrough serves p on ln until ctx is cancelled.
func (g *Gateway) servePassthrough(ctx context.Context, ln net.Listener, p passthrough) error {
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	g.logger.Info("listening",
		slog.String("server", p.server),
		slog.String("addr", ln.Addr().String()))

	if err := acceptLoop(ctx, ln, g.logger, func(conn net.Conn) { g.handlePassthrough(ctx, conn, p) }); err != nil {
		return fmt.Errorf("gateway: %s accept: %w", p.server, err)
	}
	return nil
}

// handlePassthrough routes and pipes one connection. A recover guards the
// goroutine: p.peek parses untrusted bytes, and a bug there must not take the
// process down.
func (g *Gateway) handlePassthrough(ctx context.Context, conn net.Conn, p passthrough) {
	logger := g.logger.With(slog.String("server", p.server))
	defer func() {
		if r := recover(); r != nil {
			logger.Error("passthrough handler panicked", slog.Any("recover", r))
		}
		_ = conn.Close()
	}()

	tuneTCPConn(conn, g.cfg.TCP, g.logger)

	// Bound the routing peek so a client that connects and stalls cannot pin
	// a goroutine (and its socket) indefinitely.
	_ = conn.SetReadDeadline(time.Now().Add(g.cfg.Gateway.HandshakeTimeout))
	host, buffered, err := p.peek(conn)
	if err != nil {
		logger.Debug("could not read a routable host", slog.Any("error", err))
		return
	}
	// The pipe that follows is long-lived; drop the routing deadline.
	_ = conn.SetReadDeadline(time.Time{})

	sub, ok := core.SubdomainFromHost(host, g.cfg.Tunnel.BaseDomain)
	if !ok {
		logger.Debug("host is not under the base domain", slog.String("host", host))
		return
	}
	sess, found := g.registry.Lookup(ctx, sub)
	if !found {
		logger.Debug("no session for host", slog.String("subdomain", sub))
		return
	}
	if sess.Tunnel().Protocol != p.protocol {
		logger.Debug("subdomain is not a "+string(p.protocol)+" tunnel", slog.String("subdomain", sub))
		return
	}
	opener, ok := sess.(core.RawOpener)
	if !ok {
		return
	}
	tconn, err := opener.OpenRaw(ctx)
	if err != nil {
		logger.Debug("could not open raw stream", slog.String("subdomain", sub), slog.Any("error", err))
		return
	}
	defer func() { _ = tconn.Close() }()

	logger.Debug("passthrough established", slog.String("subdomain", sub))
	// Replay what the peek consumed, then pipe the rest.
	pipeRaw(conn, buffered, tconn)
}

// pipeRaw streams bytes between a public connection and a tunnel stream until
// either side closes, then tears both ends down so the other copy unblocks.
// prefix, when set, is sent to the agent ahead of the client's own bytes: the
// opening bytes a passthrough listener consumed while routing.
func pipeRaw(client net.Conn, prefix []byte, tconn core.TunnelConn) {
	var fromClient io.Reader = client
	if len(prefix) > 0 {
		fromClient = io.MultiReader(bytes.NewReader(prefix), client)
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(tconn, fromClient)
		_ = tconn.CloseWrite()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, tconn)
		done <- struct{}{}
	}()
	<-done
	_ = tconn.Close()
	_ = client.Close()
	<-done
}
