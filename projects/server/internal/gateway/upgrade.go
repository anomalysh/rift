package gateway

import (
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/anomalysh/rift/projects/server/internal/core"
	"github.com/anomalysh/rift/projects/server/internal/tunnelproto"
)

// Upgrade implements core.Upgrader: it forwards an Upgrade request and, if the
// local service switches protocols, hands back a full-duplex stream.
//
// It mirrors RoundTrip up to the response head, then diverges: no REQ_END is
// sent after the head, because for an upgrade the request body is the
// post-handshake client->service byte stream, which arrives later as REQ_BODY
// frames and is terminated by REQ_END only when the public client half-closes.
func (s *session) Upgrade(req *http.Request) (*http.Response, core.TunnelConn, error) {
	// An upgrade reaches the local service as an HTTP request just like
	// RoundTrip, so it counts against the same A4 quota.
	if err := s.admitRequest(); err != nil {
		return nil, nil, err
	}

	ctx := req.Context()
	head := s.requestHead(req, upgradeHeaders(req.Header))
	head.Upgrade = true
	st, err := s.startStream(ctx, head)
	if err != nil {
		return nil, nil, err
	}

	rh, err := s.awaitHead(ctx, st, "upgrade")
	if err != nil {
		return nil, nil, err
	}
	if rh.Status == http.StatusSwitchingProtocols {
		return upgradeResponse(req, rh), s.newTunnelConn(ctx, st), nil
	}
	// The service answered without switching protocols; relay it normally.
	if !validResponseStatus(rh.Status) {
		return nil, nil, s.rejectResponseHead(st, rh)
	}
	return s.buildResponse(req, st, rh), nil, nil
}

// OpenRaw opens a raw full-duplex byte stream to the agent's local service, for
// tcp/tls/grpc tunnels and udp flows. It signals the agent with a Raw REQ_HEAD
// and returns at once: there is no application handshake to await. Bytes
// written to the returned conn reach the local service; bytes read come from
// it. A failed local dial arrives later as a RESET, surfacing on the first Read.
func (s *session) OpenRaw(ctx context.Context) (core.TunnelConn, error) {
	st, err := s.startStream(ctx, tunnelproto.RequestHead{Raw: true})
	if err != nil {
		return nil, err
	}
	return s.newTunnelConn(ctx, st), nil
}

// upgradeResponse builds the 101 response. Unlike buildResponse it does NOT
// strip hop-by-hop headers: Connection and Upgrade are exactly what the public
// client needs to complete the switch, and the body is the duplex stream, not
// an http.Response body.
func upgradeResponse(req *http.Request, rh tunnelproto.ResponseHead) *http.Response {
	resp := newResponse(req, rh.Status, responseHeader(rh))
	resp.Body = http.NoBody
	return resp
}

// upgradeHeaders forwards every request header, preserving Connection and
// Upgrade (which forwardableHeaders would drop as hop-by-hop) because they are
// the upgrade. The agent replaces Host with the local target's.
func upgradeHeaders(h http.Header) map[string][]string {
	return wireHeaders(h, nil)
}

// newTunnelConn wraps st as the gateway end of a full-duplex pipe. The pipe
// outlives ctx (a handshake deadline, typically), keeping only its values.
func (s *session) newTunnelConn(ctx context.Context, st *stream) *tunnelConn {
	pipeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	return &tunnelConn{
		sess:   s,
		id:     st.id,
		rd:     &bodyReader{st: st, sess: s},
		ctx:    pipeCtx,
		cancel: cancel,
	}
}

// tunnelConn is the gateway end of an upgraded, full-duplex stream. It reuses
// bodyReader for the service->client direction and emits REQ_BODY/REQ_END for
// the client->service direction.
type tunnelConn struct {
	sess *session
	id   uint64
	rd   *bodyReader

	// ctx is cancelled by Close so a Write blocked on a full send queue (a slow
	// agent while the public client has already gone) unblocks promptly.
	ctx    context.Context
	cancel context.CancelFunc

	writeMu     sync.Mutex
	writeClosed bool
	closed      bool
}

// Read yields bytes the agent sent (RES_BODY), ending at RES_END or a reset.
func (c *tunnelConn) Read(p []byte) (int, error) { return c.rd.Read(p) }

// Write ships bytes to the agent as REQ_BODY frames, chunked to the max payload.
func (c *tunnelConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeClosed {
		return 0, io.ErrClosedPipe
	}
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > tunnelproto.MaxPayloadBytes {
			n = tunnelproto.MaxPayloadBytes
		}
		frame, err := tunnelproto.Encode(tunnelproto.FrameReqBody, c.id, p[:n])
		if err != nil {
			return total, err
		}
		if err := c.sess.enqueue(c.ctx, frame); err != nil {
			return total, err
		}
		p = p[n:]
		total += n
	}
	return total, nil
}

// CloseWrite half-closes the client->service direction with a REQ_END frame.
func (c *tunnelConn) CloseWrite() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeClosed {
		return nil
	}
	c.writeClosed = true
	frame, err := tunnelproto.Encode(tunnelproto.FrameReqEnd, c.id, nil)
	if err != nil {
		return err
	}
	return c.sess.enqueue(c.ctx, frame)
}

// Close tears the stream down in both directions. It aborts the stream, forgets
// it, and resets the agent if the service->client side was not fully drained.
//
// If that side did end cleanly but the client->service side was never
// half-closed, Close sends the REQ_END itself: the agent holds its local
// connection open until both halves end, and nothing else will ever end this
// one. Either way no frame for the stream follows Close; a later Write fails
// and a later CloseWrite is a no-op.
func (c *tunnelConn) Close() error {
	// Cancel before taking writeMu: a Write blocked in enqueue on a full send
	// queue holds writeMu, and only this cancel can unblock it. Locking first
	// would make Close wait for the very Write it exists to interrupt.
	// Cancelling twice is harmless, so this needs no guard.
	c.cancel()

	c.writeMu.Lock()
	if c.closed {
		c.writeMu.Unlock()
		return nil
	}
	c.closed = true
	writeOpen := !c.writeClosed
	c.writeClosed = true
	c.writeMu.Unlock()

	if c.rd.release() && writeOpen {
		if frame, err := tunnelproto.Encode(tunnelproto.FrameReqEnd, c.id, nil); err == nil {
			c.sess.trySend(frame)
		}
	}
	return nil
}
