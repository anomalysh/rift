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
	select {
	case <-s.closing:
		return nil, nil, errSessionClosed
	default:
	}

	ctx := req.Context()
	id := s.nextID.Add(1)
	st := newStream(id, s.cfg.Tunnel.StreamBufferSize)

	s.streamsMu.Lock()
	s.streams[id] = st
	s.streamsMu.Unlock()

	head := tunnelproto.RequestHead{
		Method:     req.Method,
		Path:       requestTarget(req),
		Headers:    upgradeHeaders(req.Header),
		Host:       req.Host,
		Scheme:     s.cfg.Tunnel.PublicScheme,
		RemoteAddr: req.RemoteAddr,
		HasBody:    false,
		Upgrade:    true,
	}
	frame, err := tunnelproto.EncodeJSONFrame(tunnelproto.FrameReqHead, id, head)
	if err != nil {
		s.forgetStream(id)
		return nil, nil, err
	}
	if err := s.enqueue(ctx, frame); err != nil {
		s.forgetStream(id)
		return nil, nil, err
	}

	select {
	case rh := <-st.head:
		if rh.Status == http.StatusSwitchingProtocols {
			pipeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			conn := &tunnelConn{
				sess:   s,
				id:     id,
				rd:     &bodyReader{st: st, sess: s},
				ctx:    pipeCtx,
				cancel: cancel,
			}
			return upgradeResponse(req, rh), conn, nil
		}
		// The service answered without switching protocols; relay it normally.
		return s.buildResponse(req, st, rh), nil, nil

	case <-st.done:
		s.forgetStream(id)
		if err := st.reason(); err != nil {
			return nil, nil, err
		}
		return nil, nil, errSessionClosed
	case <-s.closing:
		s.forgetStream(id)
		return nil, nil, errSessionClosed
	case <-ctx.Done():
		s.forgetStream(id)
		s.sendReset(id, tunnelproto.StreamReset{
			Code:    tunnelproto.ResetClientDisconnected,
			Message: "public client canceled the upgrade",
		})
		return nil, nil, ctx.Err()
	}
}

// OpenRaw opens a raw full-duplex byte stream to the agent's local service, for
// tcp/tls tunnels. It signals the agent with a Raw REQ_HEAD and returns at once:
// there is no application handshake to await. Bytes written to the returned conn
// reach the local service; bytes read come from it. A failed local dial arrives
// later as a RESET, surfacing on the first Read.
func (s *session) OpenRaw(ctx context.Context) (core.TunnelConn, error) {
	select {
	case <-s.closing:
		return nil, errSessionClosed
	default:
	}

	id := s.nextID.Add(1)
	st := newStream(id, s.cfg.Tunnel.StreamBufferSize)
	s.streamsMu.Lock()
	s.streams[id] = st
	s.streamsMu.Unlock()

	frame, err := tunnelproto.EncodeJSONFrame(tunnelproto.FrameReqHead, id, tunnelproto.RequestHead{Raw: true})
	if err != nil {
		s.forgetStream(id)
		return nil, err
	}
	if err := s.enqueue(ctx, frame); err != nil {
		s.forgetStream(id)
		return nil, err
	}

	pipeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	return &tunnelConn{
		sess:   s,
		id:     id,
		rd:     &bodyReader{st: st, sess: s},
		ctx:    pipeCtx,
		cancel: cancel,
	}, nil
}

// upgradeResponse builds the 101 response. Unlike buildResponse it does NOT
// strip hop-by-hop headers: Connection and Upgrade are exactly what the public
// client needs to complete the switch, and the body is the duplex stream, not
// an http.Response body.
func upgradeResponse(req *http.Request, rh tunnelproto.ResponseHead) *http.Response {
	header := make(http.Header, len(rh.Headers))
	for k, vs := range rh.Headers {
		for _, v := range vs {
			header.Add(k, v)
		}
	}
	return &http.Response{
		Status:     http.StatusText(rh.Status),
		StatusCode: rh.Status,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     header,
		Body:       http.NoBody,
		Request:    req,
	}
}

// upgradeHeaders forwards every request header, preserving Connection and
// Upgrade (which forwardableHeaders would drop as hop-by-hop) because they are
// the upgrade. The agent replaces Host with the local target's.
func upgradeHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, vs := range h {
		cp := make([]string, len(vs))
		copy(cp, vs)
		out[lowerASCII(k)] = cp
	}
	return out
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
func (c *tunnelConn) Close() error {
	c.writeMu.Lock()
	if c.closed {
		c.writeMu.Unlock()
		return nil
	}
	c.closed = true
	c.writeMu.Unlock()

	c.cancel()
	return c.rd.Close()
}
