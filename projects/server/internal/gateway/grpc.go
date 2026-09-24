package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/anomalysh/rift/projects/server/internal/core"
)

// grpcMaxHeaderList bounds the HPACK header block the router will decode while
// peeking, so a client cannot make the peek allocate without bound.
const grpcMaxHeaderList = 1 << 20 // 1 MiB

// grpcPeekMaxFrameSize is the largest frame the routing peek will read. It is
// HTTP/2's initial SETTINGS_MAX_FRAME_SIZE: a client may not send anything
// larger until the server advertises a bigger limit, and during the peek the
// server has advertised nothing. Without it the framer would accept (and
// allocate for) any declared length up to 16 MiB per connection.
const grpcPeekMaxFrameSize = 16 << 10

// grpcPeekMaxBytes caps everything the peek reads and buffers for replay
// (preface, SETTINGS, WINDOW_UPDATE, the request HEADERS). A real client's
// opening flight is a few hundred bytes; the cap only exists so a client that
// never sends HEADERS cannot make the gateway buffer its stream in memory.
const grpcPeekMaxBytes = 64 << 10

// grpcPeekMaxFrames caps how many frames may precede the first HEADERS. A
// client opens with SETTINGS and perhaps a WINDOW_UPDATE or PRIORITY; dozens of
// tiny frames is not a client looking for a route.
const grpcPeekMaxFrames = 16

var (
	errNotH2CPreface = errors.New("gateway: connection does not begin with the HTTP/2 preface")
	errNoAuthority   = errors.New("gateway: first HEADERS frame carries no :authority")
	errPeekTooLarge  = errors.New("gateway: h2c opening flight exceeds the routing peek limit")
)

// ServeGRPCTunnels accepts cleartext HTTP/2 (h2c) connections, routes each by
// the :authority of its first HEADERS frame to the grpc tunnel on that
// subdomain, and pipes the raw h2c bytes through. Piping raw (rather than
// terminating HTTP/2) is what preserves gRPC's streaming and trailers: the
// agent's local server sees an untouched h2c connection. It blocks until ctx is
// cancelled.
func (g *Gateway) ServeGRPCTunnels(ctx context.Context) error {
	ln, err := net.Listen("tcp", g.cfg.GRPC.ListenAddr)
	if err != nil {
		return fmt.Errorf("gateway: listen grpc tunnels on %s: %w", g.cfg.GRPC.ListenAddr, err)
	}
	return g.ServeGRPCTunnelsListener(ctx, ln)
}

// ServeGRPCTunnelsListener serves h2c on an already-bound listener, so a test
// can pass its own listener to learn the bound port.
func (g *Gateway) ServeGRPCTunnelsListener(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	g.logger.Info("listening",
		slog.String("server", "grpc-tunnel"),
		slog.String("addr", ln.Addr().String()))

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("gateway: grpc tunnel accept: %w", err)
			}
		}
		go g.handleGRPCTunnel(ctx, conn)
	}
}

// handleGRPCTunnel routes and pipes one h2c connection. A recover guards the
// goroutine: the framing/HPACK parser reads untrusted bytes, and a bug there
// must not take the process down.
func (g *Gateway) handleGRPCTunnel(ctx context.Context, conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			g.logger.Error("grpc tunnel handler panicked", slog.Any("recover", r))
		}
		_ = conn.Close()
	}()

	tuneTCPConn(conn, g.cfg.TCP, g.logger)

	// Bound the routing peek so a client that connects and stalls cannot pin a
	// goroutine indefinitely.
	_ = conn.SetReadDeadline(time.Now().Add(g.cfg.Gateway.HandshakeTimeout))
	authority, buffered, err := peekH2Authority(conn)
	if err != nil {
		g.logger.Debug("grpc tunnel: could not read :authority", slog.Any("error", err))
		return
	}
	// The pipe that follows is long-lived; drop the routing deadline.
	_ = conn.SetReadDeadline(time.Time{})

	host := authority
	if h, _, splitErr := net.SplitHostPort(authority); splitErr == nil {
		host = h
	}
	sub, ok := core.SubdomainFromHost(strings.ToLower(host), g.cfg.Tunnel.BaseDomain)
	if !ok {
		g.logger.Debug("grpc tunnel: :authority is not under the base domain", slog.String("authority", authority))
		return
	}
	sess, found := g.registry.Lookup(ctx, sub)
	if !found {
		g.logger.Debug("grpc tunnel: no session for authority", slog.String("subdomain", sub))
		return
	}
	// Only a grpc tunnel's agent expects a raw h2c byte stream.
	if sess.Tunnel().Protocol != core.ProtocolGRPC {
		g.logger.Debug("grpc tunnel: subdomain is not a grpc tunnel", slog.String("subdomain", sub))
		return
	}
	opener, ok := sess.(core.RawOpener)
	if !ok {
		return
	}
	tconn, err := opener.OpenRaw(ctx)
	if err != nil {
		g.logger.Debug("grpc tunnel: could not open raw stream", slog.String("subdomain", sub), slog.Any("error", err))
		return
	}
	defer func() { _ = tconn.Close() }()

	g.logger.Debug("grpc passthrough established", slog.String("subdomain", sub))
	// Replay the preface and frames we consumed while routing, then pipe the rest.
	pipeRawPrefixed(conn, buffered, tconn)
}

// peekH2Authority reads the HTTP/2 client preface and the frames up to and
// including the first HEADERS frame, returns the request's :authority, and
// returns every byte read so the caller can replay it to the agent. It reads
// through a tee so the raw bytes are captured exactly as the framer consumes
// them; nothing is written back to the client, so the local server does the
// real HTTP/2 handshake once the pipe is established.
func peekH2Authority(conn net.Conn) (authority string, buffered []byte, err error) {
	var captured bytes.Buffer
	// Limit before the tee, so neither the framer nor the replay buffer can
	// ever see more than grpcPeekMaxBytes.
	tee := io.TeeReader(io.LimitReader(conn, grpcPeekMaxBytes), &captured)

	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(tee, preface); err != nil {
		return "", nil, err
	}
	if string(preface) != http2.ClientPreface {
		return "", nil, errNotH2CPreface
	}

	fr := http2.NewFramer(io.Discard, tee)
	fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	fr.MaxHeaderListSize = grpcMaxHeaderList
	fr.SetMaxReadFrameSize(grpcPeekMaxFrameSize)

	for n := 0; ; n++ {
		if n >= grpcPeekMaxFrames {
			return "", nil, errPeekTooLarge
		}
		frame, err := fr.ReadFrame()
		if err != nil {
			// Running into the byte cap surfaces as a short read; name it.
			if captured.Len() >= grpcPeekMaxBytes {
				return "", nil, errPeekTooLarge
			}
			return "", nil, err
		}
		mh, ok := frame.(*http2.MetaHeadersFrame)
		if !ok {
			// SETTINGS, WINDOW_UPDATE, etc. precede the request HEADERS; skip them.
			continue
		}
		a := mh.PseudoValue("authority")
		if a == "" {
			// Fall back to a Host header if the client omitted :authority.
			for _, hf := range mh.RegularFields() {
				if strings.EqualFold(hf.Name, "host") {
					a = hf.Value
					break
				}
			}
		}
		if a == "" {
			return "", nil, errNoAuthority
		}
		return a, captured.Bytes(), nil
	}
}
