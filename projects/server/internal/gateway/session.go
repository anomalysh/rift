package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/anomalysh/rift/projects/server/internal/config"
	"github.com/anomalysh/rift/projects/server/internal/core"
	"github.com/anomalysh/rift/projects/server/internal/tunnelproto"
)

// requestChunkSize is how much request body we read per REQ_BODY frame. Well
// under tunnelproto.MaxPayloadBytes so a chunk never has to be split again.
const requestChunkSize = 64 << 10

// hopByHopHeaders are connection-scoped and must not be forwarded through a
// proxy (RFC 7230 section 6.1). Keys are canonical MIME header form.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// session is a live agent WebSocket, multiplexing many proxied requests.
// It implements core.Session.
type session struct {
	conn    *websocket.Conn
	tunnel  core.Tunnel
	cfg     *config.Config
	logger  *slog.Logger
	tunnels core.TunnelStore
	tokens  core.TokenStore

	// out serialises frames onto the socket. A WebSocket connection permits
	// exactly one concurrent writer, and a bounded queue turns a slow agent
	// into backpressure instead of unbounded memory growth.
	//
	// writeLoop is the only goroutine that ever touches the socket's write
	// side, including the final shutdown frame. Writing from Close as well
	// would put two writers on one connection, and the loser blocks until its
	// context expires.
	out chan []byte

	// closing is signalled to request teardown; closed is signalled once the
	// socket is actually gone. Close returns without waiting for teardown, so
	// it is safe to call from writeLoop's own error path.
	closing chan struct{}
	closed  chan struct{}

	closeOnce    sync.Once
	teardownOnce sync.Once
	closeReason  atomic.Value // string

	streamsMu sync.Mutex
	streams   map[uint64]*stream
	nextID    atomic.Uint64

	lastSeenNanos atomic.Int64

	// requestsServed counts proxied requests, for the A4 --once/--max-requests
	// lifetime bound. ttlTimer, when set, retires the tunnel after the A4 --ttl
	// wall-clock budget.
	requestsServed atomic.Int64
	ttlTimer       *time.Timer

	wg sync.WaitGroup
}

func newSession(conn *websocket.Conn, t core.Tunnel, cfg *config.Config, tunnels core.TunnelStore, tokens core.TokenStore, logger *slog.Logger) *session {
	s := &session{
		conn:    conn,
		tunnel:  t,
		cfg:     cfg,
		logger:  logger,
		tunnels: tunnels,
		tokens:  tokens,
		out:     make(chan []byte, cfg.Tunnel.StreamBufferSize),
		closing: make(chan struct{}),
		closed:  make(chan struct{}),
		streams: make(map[uint64]*stream),
	}
	s.lastSeenNanos.Store(time.Now().UnixNano())
	// A4: retire the tunnel after a wall-clock TTL. The timer fires Close, which
	// is idempotent, so a session that closes first simply makes it a no-op.
	if ttl := t.Policy.TTLSeconds; ttl > 0 {
		s.ttlTimer = time.AfterFunc(time.Duration(ttl)*time.Second, func() {
			s.logger.Info("tunnel reached its ttl", slog.Int("ttl_seconds", ttl))
			_ = s.Close(string(tunnelproto.ShutdownPolicyExpired))
		})
	}
	return s
}

// Tunnel implements core.Session.
func (s *session) Tunnel() core.Tunnel { return s.tunnel }

// maxRequests returns the A4 request quota for this tunnel: 1 for --once, the
// configured cap for --max-requests, or 0 (unbounded).
func (s *session) maxRequests() int64 {
	if s.tunnel.Policy.Once {
		return 1
	}
	return int64(s.tunnel.Policy.MaxRequests)
}

// Close implements core.Session. It requests teardown and returns immediately;
// it never touches the socket. Safe to call repeatedly, from any goroutine,
// including writeLoop itself.
func (s *session) Close(reason string) error {
	s.closeOnce.Do(func() {
		s.closeReason.Store(reason)
		if s.ttlTimer != nil {
			s.ttlTimer.Stop()
		}
		close(s.closing)
	})
	return nil
}

// closeHandshakeGrace bounds the WebSocket close handshake. An agent that has
// already stopped reading — which is exactly what a well-behaved agent does
// after receiving our shutdown frame — will never send its own close frame,
// and waiting for one would stall teardown.
const closeHandshakeGrace = time.Second

// teardown is called only by writeLoop. It sends the agent a final shutdown
// frame explaining why, then closes the socket.
func (s *session) teardown() {
	s.teardownOnce.Do(func() {
		reason := closeReasonOf(s)

		// Best effort: tell the agent why before the socket goes away, so it
		// knows whether reconnecting is pointless.
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Gateway.WriteTimeout)
		if frame, err := tunnelproto.EncodeControl(tunnelproto.ControlShutdown,
			tunnelproto.Shutdown{Reason: tunnelproto.ShutdownReason(reason)}); err == nil {
			_ = s.conn.Write(ctx, websocket.MessageBinary, frame)
		}
		cancel()

		closed := make(chan struct{})
		go func() {
			_ = s.conn.Close(websocket.StatusNormalClosure, reason)
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(closeHandshakeGrace):
			_ = s.conn.CloseNow()
		}

		close(s.closed)
	})
}

// abortAllStreams fails every in-flight request. Called once the socket dies.
func (s *session) abortAllStreams(err error) {
	s.streamsMu.Lock()
	sts := make([]*stream, 0, len(s.streams))
	for _, st := range s.streams {
		sts = append(sts, st)
	}
	s.streams = make(map[uint64]*stream)
	s.streamsMu.Unlock()

	for _, st := range sts {
		st.abort(err)
	}
}

func (s *session) forgetStream(id uint64) {
	s.streamsMu.Lock()
	delete(s.streams, id)
	s.streamsMu.Unlock()
}

func (s *session) lookupStream(id uint64) (*stream, bool) {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	st, ok := s.streams[id]
	return st, ok
}

// enqueue hands a frame to the writer goroutine.
func (s *session) enqueue(ctx context.Context, frame []byte) error {
	select {
	case s.out <- frame:
		return nil
	case <-s.closing:
		return errSessionClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sendReset aborts a stream on the agent. Best effort: a dead session has
// nothing to cancel.
func (s *session) sendReset(id uint64, rs tunnelproto.StreamReset) {
	frame, err := tunnelproto.EncodeJSONFrame(tunnelproto.FrameReset, id, rs)
	if err != nil {
		return
	}
	select {
	case s.out <- frame:
	case <-s.closing:
	default:
		// Queue is full and the agent is not draining; the session is already
		// in trouble and the socket teardown will abort the stream anyway.
	}
}

// writeLoop owns the socket's write side for the whole life of the session.
func (s *session) writeLoop() {
	defer s.wg.Done()
	defer s.teardown()

	for {
		// A pending close outranks queued frames, so teardown cannot be
		// starved by a busy stream.
		select {
		case <-s.closing:
			return
		default:
		}

		select {
		case <-s.closing:
			return
		case frame := <-s.out:
			ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Gateway.WriteTimeout)
			err := s.conn.Write(ctx, websocket.MessageBinary, frame)
			cancel()
			if err != nil {
				s.logger.Debug("tunnel write failed", slog.Any("error", err))
				_ = s.Close(string(tunnelproto.ShutdownServerShutdown))
				return
			}
		}
	}
}

// readLoop owns the socket's read side and dispatches every inbound frame.
func (s *session) readLoop(ctx context.Context) {
	defer s.wg.Done()
	defer s.abortAllStreams(errSessionClosed)

	for {
		typ, data, err := s.conn.Read(ctx)
		if err != nil {
			s.logger.Debug("tunnel read ended", slog.Any("error", err))
			_ = s.Close(string(tunnelproto.ShutdownServerShutdown))
			return
		}
		if typ != websocket.MessageBinary {
			s.logger.Warn("agent sent a non-binary message; closing tunnel")
			_ = s.Close(string(tunnelproto.ShutdownServerShutdown))
			return
		}

		frame, err := tunnelproto.Decode(data)
		if err != nil {
			s.logger.Warn("malformed frame from agent; closing tunnel", slog.Any("error", err))
			_ = s.Close(string(tunnelproto.ShutdownServerShutdown))
			return
		}
		if !s.dispatch(ctx, frame) {
			return
		}
	}
}

// dispatch handles one frame. It returns false when the session must stop.
func (s *session) dispatch(ctx context.Context, f tunnelproto.Frame) bool {
	switch f.Type {
	case tunnelproto.FrameControl:
		return s.handleControl(ctx, f.Payload)

	case tunnelproto.FrameResHead:
		st, ok := s.lookupStream(f.StreamID)
		if !ok {
			return true // the public client went away; the agent will learn via RESET
		}
		var head tunnelproto.ResponseHead
		if err := unmarshalFrame(f.Payload, &head); err != nil {
			st.abort(err)
			return true
		}
		select {
		case st.head <- head:
		default:
			// A second RES_HEAD on one stream is a protocol violation.
			st.abort(errors.New("gateway: agent sent more than one response head"))
		}

	case tunnelproto.FrameResBody:
		st, ok := s.lookupStream(f.StreamID)
		if !ok {
			return true
		}
		s.deliverBody(st, f.Payload)

	case tunnelproto.FrameResEnd:
		if st, ok := s.lookupStream(f.StreamID); ok {
			st.endBody()
		}

	case tunnelproto.FrameReset:
		st, ok := s.lookupStream(f.StreamID)
		if !ok {
			return true
		}
		var rs tunnelproto.StreamReset
		if err := unmarshalFrame(f.Payload, &rs); err != nil {
			st.abort(err)
			return true
		}
		st.abort(tunnelproto.NewStreamResetError(rs))

	default:
		// Unknown frame types are ignored so a newer agent stays compatible.
		s.logger.Debug("ignoring unknown frame type", slog.String("type", f.Type.String()))
	}
	return true
}

// deliverBody pushes a chunk to the stream's consumer.
//
// The chunk channel is bounded, so a public client that stops reading will
// eventually stall this loop and, with it, every other stream on the tunnel.
// Rather than block forever we give the slow stream RequestTimeout to catch
// up, then reset just that stream and keep the tunnel serving the others.
func (s *session) deliverBody(st *stream, chunk []byte) {
	// coder/websocket allocates a fresh buffer per Read, so retaining the
	// payload beyond this call is safe.
	select {
	case st.body <- chunk:
		return
	case <-st.done:
		return
	case <-s.closing:
		return
	default:
	}

	timer := time.NewTimer(s.cfg.Tunnel.RequestTimeout)
	defer timer.Stop()

	select {
	case st.body <- chunk:
	case <-st.done:
	case <-s.closing:
	case <-timer.C:
		st.abort(tunnelproto.NewStreamResetError(tunnelproto.StreamReset{
			Code:    tunnelproto.ResetUpstreamTimeout,
			Message: "public client did not read the response body in time",
		}))
		s.sendReset(st.id, tunnelproto.StreamReset{Code: tunnelproto.ResetCanceled})
	}
}

func (s *session) handleControl(ctx context.Context, payload []byte) bool {
	env, err := tunnelproto.DecodeControl(payload)
	if err != nil {
		s.logger.Warn("malformed control frame; closing tunnel", slog.Any("error", err))
		_ = s.Close(string(tunnelproto.ShutdownServerShutdown))
		return false
	}

	switch env.Type {
	case tunnelproto.ControlPing:
		var hb tunnelproto.Heartbeat
		if err := tunnelproto.UnmarshalPayload(env, &hb); err != nil {
			return true
		}
		s.lastSeenNanos.Store(time.Now().UnixNano())

		// A reaped or taken-over tunnel is gone from the store. Learning that
		// here is how a node discovers another node claimed its subdomain.
		if err := s.tunnels.Heartbeat(ctx, s.tunnel.ID, time.Now()); err != nil {
			if errors.Is(err, core.ErrNotFound) {
				s.logger.Info("tunnel no longer registered; closing")
				_ = s.Close(string(tunnelproto.ShutdownReplaced))
				return false
			}
			s.logger.Error("heartbeat persistence failed", slog.Any("error", err))
		}

		frame, err := tunnelproto.EncodeControl(tunnelproto.ControlPong, tunnelproto.Heartbeat{TS: hb.TS})
		if err != nil {
			return true
		}
		_ = s.enqueue(ctx, frame)

	case tunnelproto.ControlPong:
		s.lastSeenNanos.Store(time.Now().UnixNano())

	default:
		s.logger.Debug("ignoring unexpected control message", slog.String("type", string(env.Type)))
	}
	return true
}

// watchdog closes the session when heartbeats stop arriving, and periodically
// re-checks that the tunnel's token is still valid.
//
// Revocation must terminate the tunnels a token already opened, not merely
// stop it from opening new ones. The check lives here rather than on the
// heartbeat path so that an agent which stops sending pings cannot postpone
// its own revocation.
func (s *session) watchdog(ctx context.Context) {
	defer s.wg.Done()

	ticker := time.NewTicker(s.cfg.Tunnel.HeartbeatInterval)
	defer ticker.Stop()

	lastTokenCheck := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.closing:
			return
		case <-ticker.C:
			// The session logger already carries tunnel_id and token_id.
			last := time.Unix(0, s.lastSeenNanos.Load())
			if time.Since(last) > s.cfg.Tunnel.HeartbeatTimeout {
				s.logger.Info("tunnel heartbeat timed out; closing",
					slog.Duration("since_last_seen", time.Since(last)))
				_ = s.Close(string(tunnelproto.ShutdownHeartbeatTimeout))
				return
			}

			if time.Since(lastTokenCheck) < s.cfg.Tunnel.TokenRevalidateInterval {
				continue
			}
			lastTokenCheck = time.Now()
			if s.tokenRevoked(ctx) {
				s.logger.Info("token is no longer valid; closing tunnel")
				_ = s.Close(string(tunnelproto.ShutdownTokenRevoked))
				return
			}
		}
	}
}

// tokenRevoked reports whether the tunnel's token has been revoked, expired,
// or deleted. A store failure is not treated as revocation: a database blip
// must not disconnect every live tunnel at once.
func (s *session) tokenRevoked(ctx context.Context) bool {
	token, err := s.tokens.FindByID(ctx, s.tunnel.TokenID)
	if errors.Is(err, core.ErrNotFound) {
		return true
	}
	if err != nil {
		s.logger.Warn("could not revalidate token", slog.Any("error", err))
		return false
	}
	return !token.Active(time.Now())
}

// RoundTrip implements http.RoundTripper: it ships one public request through
// the tunnel and returns the agent's response.
func (s *session) RoundTrip(req *http.Request) (*http.Response, error) {
	select {
	case <-s.closing:
		return nil, errSessionClosed
	default:
	}

	// A4: --once / --max-requests. Requests 1..max are served; the one that would
	// exceed the quota is refused and retires the tunnel, so subsequent requests
	// 404 rather than error.
	if maxReq := s.maxRequests(); maxReq > 0 {
		if s.requestsServed.Add(1) > maxReq {
			_ = s.Close(string(tunnelproto.ShutdownPolicyExpired))
			return nil, errSessionClosed
		}
	}

	ctx := req.Context()
	id := s.nextID.Add(1)
	st := newStream(id, s.cfg.Tunnel.StreamBufferSize)

	s.streamsMu.Lock()
	s.streams[id] = st
	s.streamsMu.Unlock()

	hasBody := req.Body != nil && req.Body != http.NoBody && req.ContentLength != 0

	head := tunnelproto.RequestHead{
		Method:     req.Method,
		Path:       requestTarget(req),
		Headers:    forwardableHeaders(req.Header),
		Host:       req.Host,
		Scheme:     s.cfg.Tunnel.PublicScheme,
		RemoteAddr: req.RemoteAddr,
		HasBody:    hasBody,
	}
	frame, err := tunnelproto.EncodeJSONFrame(tunnelproto.FrameReqHead, id, head)
	if err != nil {
		s.forgetStream(id)
		return nil, err
	}
	if err := s.enqueue(ctx, frame); err != nil {
		s.forgetStream(id)
		return nil, err
	}

	if hasBody {
		go s.pumpRequestBody(ctx, id, st, req.Body)
	} else {
		endFrame, err := tunnelproto.Encode(tunnelproto.FrameReqEnd, id, nil)
		if err == nil {
			_ = s.enqueue(ctx, endFrame)
		}
	}

	select {
	case rh := <-st.head:
		return s.buildResponse(req, st, rh), nil

	case <-st.done:
		s.forgetStream(id)
		if err := st.reason(); err != nil {
			return nil, err
		}
		return nil, errSessionClosed

	case <-s.closing:
		s.forgetStream(id)
		return nil, errSessionClosed

	case <-ctx.Done():
		s.forgetStream(id)
		s.sendReset(id, tunnelproto.StreamReset{
			Code:    tunnelproto.ResetClientDisconnected,
			Message: "public client canceled the request",
		})
		return nil, ctx.Err()
	}
}

func (s *session) buildResponse(req *http.Request, st *stream, rh tunnelproto.ResponseHead) *http.Response {
	header := make(http.Header, len(rh.Headers))
	for k, vs := range rh.Headers {
		for _, v := range vs {
			header.Add(k, v)
		}
	}
	for _, h := range hopByHopHeaders {
		header.Del(h)
	}

	// A declared Content-Length lets net/http choose identity framing instead
	// of chunking the response a second time.
	contentLength := int64(-1)
	if cl := header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil && n >= 0 {
			contentLength = n
		}
	}

	return &http.Response{
		Status:        http.StatusText(rh.Status),
		StatusCode:    rh.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          &bodyReader{st: st, sess: s},
		ContentLength: contentLength,
		Request:       req,
	}
}

// pumpRequestBody streams the public request body to the agent.
func (s *session) pumpRequestBody(ctx context.Context, id uint64, st *stream, body io.ReadCloser) {
	defer func() { _ = body.Close() }()

	buf := make([]byte, requestChunkSize)
	for {
		select {
		case <-st.done:
			return
		case <-s.closing:
			return
		default:
		}

		n, readErr := body.Read(buf)
		if n > 0 {
			frame, err := tunnelproto.Encode(tunnelproto.FrameReqBody, id, buf[:n])
			if err != nil {
				st.abort(err)
				return
			}
			if err := s.enqueue(ctx, frame); err != nil {
				st.abort(err)
				return
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			// A body that failed mid-read (client hung up, or exceeded
			// MaxBytesReader) leaves the agent waiting; reset instead.
			s.sendReset(id, tunnelproto.StreamReset{
				Code:    tunnelproto.ResetClientDisconnected,
				Message: "request body ended early",
			})
			st.abort(readErr)
			return
		}
	}

	endFrame, err := tunnelproto.Encode(tunnelproto.FrameReqEnd, id, nil)
	if err != nil {
		st.abort(err)
		return
	}
	_ = s.enqueue(ctx, endFrame)
}

// requestTarget renders path plus query exactly as the origin server should
// see it.
func requestTarget(req *http.Request) string {
	target := req.URL.EscapedPath()
	if target == "" {
		target = "/"
	}
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}
	return target
}

// forwardableHeaders lowercases header names and drops hop-by-hop entries,
// including any header named by the request's own Connection header.
func forwardableHeaders(h http.Header) map[string][]string {
	drop := make(map[string]struct{}, len(hopByHopHeaders))
	for _, name := range hopByHopHeaders {
		drop[http.CanonicalHeaderKey(name)] = struct{}{}
	}
	// Connection lists further headers that are themselves hop-by-hop.
	for _, v := range h.Values("Connection") {
		for _, name := range splitAndTrim(v, ',') {
			if name != "" {
				drop[http.CanonicalHeaderKey(name)] = struct{}{}
			}
		}
	}

	out := make(map[string][]string, len(h))
	for k, vs := range h {
		if _, skip := drop[http.CanonicalHeaderKey(k)]; skip {
			continue
		}
		cp := make([]string, len(vs))
		copy(cp, vs)
		out[lowerASCII(k)] = cp
	}
	return out
}
