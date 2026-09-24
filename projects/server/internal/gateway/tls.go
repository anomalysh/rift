package gateway

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/anomalysh/rift/projects/server/internal/core"
)

const (
	tlsRecordHeaderLen      = 5
	tlsRecordHandshake      = 0x16
	tlsHandshakeClientHello = 0x01
	tlsExtServerName        = 0x0000
	tlsSNIHostName          = 0x00
	// A TLS record carries at most 2^14 bytes of plaintext (RFC 8446 §5.1),
	// and a ClientHello arrives in plaintext.
	maxClientHelloRecord = 1 << 14
)

var (
	errNotTLSHandshake = errors.New("gateway: first record is not a TLS handshake")
	errMalformedHello  = errors.New("gateway: malformed ClientHello")
	errNoSNI           = errors.New("gateway: ClientHello carries no SNI")
)

// tlsPassthrough routes by ClientHello SNI and pipes the still-encrypted bytes
// through; the agent's local service terminates TLS.
var tlsPassthrough = passthrough{
	server:   "tls-tunnel",
	protocol: core.ProtocolTLS,
	peek:     peekClientHello,
}

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
	return g.servePassthrough(ctx, ln, tlsPassthrough)
}

// peekClientHello reads the first TLS record, returns the SNI host name it
// advertises, and returns every byte read so the caller can replay them.
func peekClientHello(conn net.Conn) (sni string, buffered []byte, err error) {
	var header [tlsRecordHeaderLen]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return "", nil, err
	}
	if header[0] != tlsRecordHandshake {
		return "", nil, errNotTLSHandshake
	}
	recLen := int(binary.BigEndian.Uint16(header[3:5]))
	if recLen == 0 || recLen > maxClientHelloRecord {
		return "", nil, fmt.Errorf("%w: record length %d", errMalformedHello, recLen)
	}
	buffered = make([]byte, tlsRecordHeaderLen+recLen)
	copy(buffered, header[:])
	if _, err := io.ReadFull(conn, buffered[tlsRecordHeaderLen:]); err != nil {
		return "", nil, err
	}

	sni, err = sniFromClientHello(buffered[tlsRecordHeaderLen:])
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
	c.skip(3)       // handshake length
	c.skip(2 + 32)  // client_version + random
	c.skip(c.u8())  // session_id
	c.skip(c.u16()) // cipher_suites (2-byte length)
	c.skip(c.u8())  // compression_methods
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
