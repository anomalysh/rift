package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/anomalysh/rift/server/internal/core"
)

const (
	tlsRecordHandshake      = 0x16
	tlsHandshakeClientHello = 0x01
	tlsExtServerName        = 0x0000
	tlsSNIHostName          = 0x00
	// A TLS record's length field is 16-bit; a ClientHello never exceeds it.
	maxClientHelloRecord = 1 << 14
)

var (
	errNotTLSHandshake = errors.New("gateway: first record is not a TLS handshake")
	errMalformedHello  = errors.New("gateway: malformed ClientHello")
	errNoSNI           = errors.New("gateway: ClientHello carries no SNI")
)

// ServeTLSTunnels accepts passthrough TLS connections, routes each by its
// ClientHello SNI to the tls tunnel serving that subdomain, and pipes the
// still-encrypted bytes through. The agent's local service terminates TLS.
// It blocks until ctx is cancelled.
func (g *Gateway) ServeTLSTunnels(ctx context.Context) error {
	ln, err := net.Listen("tcp", g.cfg.TLSTunnel.ListenAddr)
	if err != nil {
		return fmt.Errorf("gateway: listen tls tunnels on %s: %w", g.cfg.TLSTunnel.ListenAddr, err)
	}
	return g.ServeTLSTunnelsListener(ctx, ln)
}

// ServeTLSTunnelsListener serves passthrough TLS on an already-bound listener.
// ServeTLSTunnels calls it after binding the configured address; a test can
// pass its own listener to learn the bound port.
func (g *Gateway) ServeTLSTunnelsListener(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	g.logger.Info("listening",
		slog.String("server", "tls-tunnel"),
		slog.String("addr", ln.Addr().String()))

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("gateway: tls tunnel accept: %w", err)
			}
		}
		go g.handleTLSTunnel(ctx, conn)
	}
}

// handleTLSTunnel routes and pipes one passthrough connection. A recover guards
// the goroutine: the ClientHello parser reads untrusted bytes, and a bug there
// must not take the process down.
func (g *Gateway) handleTLSTunnel(ctx context.Context, conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			g.logger.Error("tls tunnel handler panicked", slog.Any("recover", r))
		}
		_ = conn.Close()
	}()

	// Bound the SNI peek so a client that connects and stalls cannot pin a
	// goroutine (and its port) indefinitely.
	_ = conn.SetReadDeadline(time.Now().Add(g.cfg.Gateway.HandshakeTimeout))
	sni, buffered, err := peekClientHello(conn)
	if err != nil {
		g.logger.Debug("tls tunnel: could not read ClientHello", slog.Any("error", err))
		return
	}
	// The pipe that follows is long-lived; drop the handshake deadline.
	_ = conn.SetReadDeadline(time.Time{})

	sub, ok := core.SubdomainFromHost(strings.ToLower(sni), g.cfg.Tunnel.BaseDomain)
	if !ok {
		g.logger.Debug("tls tunnel: SNI is not under the base domain", slog.String("sni", sni))
		return
	}
	sess, found := g.registry.Lookup(ctx, sub)
	if !found {
		g.logger.Debug("tls tunnel: no session for SNI", slog.String("subdomain", sub))
		return
	}
	// Refuse to raw-pipe an http tunnel that merely shares the subdomain: only a
	// tls tunnel's agent expects encrypted bytes.
	if sess.Tunnel().Protocol != core.ProtocolTLS {
		g.logger.Debug("tls tunnel: subdomain is not a tls tunnel", slog.String("subdomain", sub))
		return
	}
	opener, ok := sess.(core.RawOpener)
	if !ok {
		return
	}
	tconn, err := opener.OpenRaw(ctx)
	if err != nil {
		g.logger.Debug("tls tunnel: could not open raw stream", slog.String("subdomain", sub), slog.Any("error", err))
		return
	}
	defer func() { _ = tconn.Close() }()

	g.logger.Debug("tls passthrough established", slog.String("subdomain", sub))
	// Replay the ClientHello we consumed while peeking, then pipe the rest.
	pipeRawPrefixed(conn, buffered, tconn)
}

// pipeRawPrefixed is pipeRaw with prefix bytes prepended to the client->agent
// direction (the ClientHello consumed during SNI peeking).
func pipeRawPrefixed(client net.Conn, prefix []byte, tconn core.TunnelConn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(tconn, io.MultiReader(bytes.NewReader(prefix), client))
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

// peekClientHello reads the first TLS record, returns the SNI host name it
// advertises, and returns every byte read so the caller can replay them.
func peekClientHello(conn net.Conn) (sni string, buffered []byte, err error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", nil, err
	}
	if header[0] != tlsRecordHandshake {
		return "", nil, errNotTLSHandshake
	}
	recLen := int(binary.BigEndian.Uint16(header[3:5]))
	if recLen == 0 || recLen > maxClientHelloRecord {
		return "", nil, fmt.Errorf("%w: record length %d", errMalformedHello, recLen)
	}
	body := make([]byte, recLen)
	if _, err := io.ReadFull(conn, body); err != nil {
		return "", nil, err
	}
	buffered = make([]byte, 0, len(header)+len(body))
	buffered = append(buffered, header...)
	buffered = append(buffered, body...)

	sni, err = sniFromClientHello(body)
	if err != nil {
		return "", nil, err
	}
	return sni, buffered, nil
}

// sniFromClientHello extracts the server_name host from a ClientHello handshake
// message. Every read is bounds-checked through the cursor, so malformed or
// truncated input yields an error rather than a panic.
func sniFromClientHello(b []byte) (string, error) {
	c := &cursor{b: b}
	if c.u8() != tlsHandshakeClientHello {
		return "", errMalformedHello
	}
	c.skip(3)           // handshake length
	c.skip(2 + 32)      // client_version + random
	c.skip(c.u8())      // session_id
	c.skip(c.u16Len(2)) // cipher_suites (2-byte length)
	c.skip(c.u8())      // compression_methods
	if c.err != nil {
		return "", errMalformedHello
	}

	// Extensions are optional; their absence just means no SNI.
	if c.remaining() == 0 {
		return "", errNoSNI
	}
	extTotal := c.u16()
	extEnd := c.p + extTotal
	if c.err != nil || extEnd > len(b) {
		return "", errMalformedHello
	}
	for c.p+4 <= extEnd {
		extType := c.u16()
		extLen := c.u16()
		if c.err != nil || c.p+extLen > extEnd {
			return "", errMalformedHello
		}
		if extType == tlsExtServerName {
			return serverNameFromExtension(c.bytes(extLen))
		}
		c.skip(extLen)
	}
	return "", errNoSNI
}

// serverNameFromExtension pulls the first host_name entry from a server_name
// extension body.
func serverNameFromExtension(ext []byte) (string, error) {
	c := &cursor{b: ext}
	listLen := c.u16()
	listEnd := c.p + listLen
	if c.err != nil || listEnd > len(ext) {
		return "", errMalformedHello
	}
	for c.p+3 <= listEnd {
		nameType := c.u8()
		name := c.bytes(c.u16())
		if c.err != nil {
			return "", errMalformedHello
		}
		if nameType == tlsSNIHostName {
			return string(name), nil
		}
	}
	return "", errNoSNI
}

// cursor reads big-endian TLS fields with a bounds check on every access. Once
// any read runs past the buffer, err is set and further reads are no-ops.
type cursor struct {
	b   []byte
	p   int
	err error
}

func (c *cursor) remaining() int {
	if c.err != nil {
		return 0
	}
	return len(c.b) - c.p
}

func (c *cursor) u8() int {
	if c.err != nil || c.p+1 > len(c.b) {
		c.fail()
		return 0
	}
	v := int(c.b[c.p])
	c.p++
	return v
}

func (c *cursor) u16() int {
	if c.err != nil || c.p+2 > len(c.b) {
		c.fail()
		return 0
	}
	v := int(binary.BigEndian.Uint16(c.b[c.p:]))
	c.p += 2
	return v
}

// u16Len reads a big-endian length of `size` bytes (1 or 2) as a value to skip.
func (c *cursor) u16Len(size int) int {
	if size == 2 {
		return c.u16()
	}
	return c.u8()
}

func (c *cursor) skip(n int) {
	if c.err != nil || n < 0 || c.p+n > len(c.b) {
		c.fail()
		return
	}
	c.p += n
}

func (c *cursor) bytes(n int) []byte {
	if c.err != nil || n < 0 || c.p+n > len(c.b) {
		c.fail()
		return nil
	}
	v := c.b[c.p : c.p+n]
	c.p += n
	return v
}

func (c *cursor) fail() {
	if c.err == nil {
		c.err = errMalformedHello
	}
}
