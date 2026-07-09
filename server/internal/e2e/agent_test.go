package e2e

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/anomalysh/rift/server/internal/tunnelproto"
)

// testAgent is a minimal, correct implementation of the agent side of the
// protocol. It serves an http.Handler through the tunnel, exactly as the Bun
// CLI serves the developer's local process.
type testAgent struct {
	t    *testing.T
	conn *websocket.Conn

	writeMu sync.Mutex

	mu       sync.Mutex
	streams  map[uint64]*agentStream
	shutdown tunnelproto.ShutdownReason

	// declineUpgrade makes the agent answer an upgrade request with an ordinary
	// non-101 response instead of switching protocols, so a test can exercise
	// the gateway's decline path.
	declineUpgrade bool

	// rawBackend, when set, makes raw (tcp/tls) streams proxy to this address
	// instead of echoing, so a test can run a real TLS handshake through them.
	rawBackend string

	hello tunnelproto.HelloOK
	done  chan struct{}
}

type agentStream struct {
	pw     *io.PipeWriter
	cancel context.CancelFunc
	// upgrade streams have no HTTP handler: they echo the raw duplex byte
	// stream, standing in for a WebSocket server.
	upgrade bool
	// raw, when set, is a real backend connection this stream proxies to (used
	// by the tls test); REQ_BODY is written to it and its reads become RES_BODY.
	raw net.Conn
}

// dialAgent completes an http handshake and starts serving h through the tunnel.
func dialAgent(t *testing.T, ctx context.Context, gatewayURL, token, subdomain string, h http.Handler, opts ...func(*testAgent)) (*testAgent, error) {
	return dialAgentProto(t, ctx, gatewayURL, token, subdomain, "http", h, opts...)
}

// dialAgentProto completes the handshake for a given protocol and starts
// serving. Optional opts tune the agent before its loops start.
func dialAgentProto(t *testing.T, ctx context.Context, gatewayURL, token, subdomain, protocol string, h http.Handler, opts ...func(*testAgent)) (*testAgent, error) {
	t.Helper()

	conn, _, err := websocket.Dial(ctx, gatewayURL, &websocket.DialOptions{
		Subprotocols: []string{tunnelproto.Subprotocol},
	})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(int64(tunnelproto.MaxFrameBytes))

	helloFrame, err := tunnelproto.EncodeControl(tunnelproto.ControlHello, tunnelproto.Hello{
		ProtocolVersion: tunnelproto.Version,
		Token:           token,
		Protocol:        protocol,
		Subdomain:       subdomain,
		LocalPort:       3000,
		ClientVersion:   "test",
	})
	if err != nil {
		return nil, err
	}
	if err := conn.Write(ctx, websocket.MessageBinary, helloFrame); err != nil {
		return nil, err
	}

	_, data, err := conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	frame, err := tunnelproto.Decode(data)
	if err != nil {
		return nil, err
	}
	env, err := tunnelproto.DecodeControl(frame.Payload)
	if err != nil {
		return nil, err
	}

	switch env.Type {
	case tunnelproto.ControlHelloOK:
		var ok tunnelproto.HelloOK
		if err := tunnelproto.UnmarshalPayload(env, &ok); err != nil {
			return nil, err
		}
		a := &testAgent{
			t:       t,
			conn:    conn,
			streams: make(map[uint64]*agentStream),
			hello:   ok,
			done:    make(chan struct{}),
		}
		for _, o := range opts {
			o(a)
		}
		go a.readLoop(h)
		go a.heartbeat()
		return a, nil

	case tunnelproto.ControlHelloError:
		var he tunnelproto.HelloError
		if err := tunnelproto.UnmarshalPayload(env, &he); err != nil {
			return nil, err
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
		return nil, &handshakeRejection{code: he.Code, message: he.Message}

	default:
		return nil, errors.New("unexpected control message: " + string(env.Type))
	}
}

type handshakeRejection struct {
	code    tunnelproto.ErrorCode
	message string
}

func (e *handshakeRejection) Error() string { return string(e.code) + ": " + e.message }

func (a *testAgent) close() {
	_ = a.conn.Close(websocket.StatusNormalClosure, "")
}

// shutdownReason returns the reason the gateway gave for closing, if any.
func (a *testAgent) shutdownReason() tunnelproto.ShutdownReason {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.shutdown
}

// heartbeat pings on the interval the gateway asked for, as a real agent does.
func (a *testAgent) heartbeat() {
	interval := time.Duration(a.hello.HeartbeatIntervalMS) * time.Millisecond
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-a.done:
			return
		case <-ticker.C:
			frame, err := tunnelproto.EncodeControl(tunnelproto.ControlPing,
				tunnelproto.Heartbeat{TS: time.Now().UnixMilli()})
			if err != nil {
				return
			}
			if err := a.write(frame); err != nil {
				return
			}
		}
	}
}

func (a *testAgent) write(frame []byte) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return a.conn.Write(ctx, websocket.MessageBinary, frame)
}

func (a *testAgent) readLoop(h http.Handler) {
	defer close(a.done)
	ctx := context.Background()

	for {
		_, data, err := a.conn.Read(ctx)
		if err != nil {
			return
		}
		frame, err := tunnelproto.Decode(data)
		if err != nil {
			return
		}

		switch frame.Type {
		case tunnelproto.FrameControl:
			env, err := tunnelproto.DecodeControl(frame.Payload)
			if err != nil {
				return
			}
			if env.Type == tunnelproto.ControlShutdown {
				var sd tunnelproto.Shutdown
				if err := tunnelproto.UnmarshalPayload(env, &sd); err == nil {
					a.mu.Lock()
					a.shutdown = sd.Reason
					a.mu.Unlock()
				}
				return
			}

		case tunnelproto.FrameReqHead:
			var head tunnelproto.RequestHead
			if err := unmarshal(frame.Payload, &head); err != nil {
				return
			}
			switch {
			case head.Raw:
				a.startRaw(frame.StreamID)
			case head.Upgrade:
				a.startUpgrade(frame.StreamID, head)
			default:
				a.startRequest(frame.StreamID, head, h)
			}

		case tunnelproto.FrameReqBody:
			if st := a.stream(frame.StreamID); st != nil {
				chunk := make([]byte, len(frame.Payload))
				copy(chunk, frame.Payload)
				switch {
				case st.raw != nil:
					_, _ = st.raw.Write(chunk)
				case st.upgrade:
					// Echo the upgraded byte stream straight back.
					if bodyFrame, err := tunnelproto.Encode(tunnelproto.FrameResBody, frame.StreamID, chunk); err == nil {
						_ = a.write(bodyFrame)
					}
				default:
					_, _ = st.pw.Write(chunk)
				}
			}

		case tunnelproto.FrameReqEnd:
			if st := a.stream(frame.StreamID); st != nil {
				switch {
				case st.raw != nil:
					if cw, ok := st.raw.(interface{ CloseWrite() error }); ok {
						_ = cw.CloseWrite()
					}
				case st.upgrade:
					if endFrame, err := tunnelproto.Encode(tunnelproto.FrameResEnd, frame.StreamID, nil); err == nil {
						_ = a.write(endFrame)
					}
					a.forget(frame.StreamID)
				default:
					_ = st.pw.Close()
				}
			}

		case tunnelproto.FrameReset:
			if st := a.stream(frame.StreamID); st != nil {
				st.cancel()
				if st.pw != nil {
					_ = st.pw.CloseWithError(errors.New("reset by gateway"))
				}
				if st.raw != nil {
					_ = st.raw.Close()
				}
				a.forget(frame.StreamID)
			}
		}
	}
}

func (a *testAgent) stream(id uint64) *agentStream {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.streams[id]
}

func (a *testAgent) forget(id uint64) {
	a.mu.Lock()
	delete(a.streams, id)
	a.mu.Unlock()
}

func (a *testAgent) startRequest(id uint64, head tunnelproto.RequestHead, h http.Handler) {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())

	a.mu.Lock()
	a.streams[id] = &agentStream{pw: pw, cancel: cancel}
	a.mu.Unlock()

	if !head.HasBody {
		_ = pw.Close()
	}

	go func() {
		defer cancel()
		defer a.forget(id)

		req, err := http.NewRequestWithContext(ctx, head.Method, "http://"+head.Host+head.Path, pr)
		if err != nil {
			a.sendReset(id, tunnelproto.ResetUpstreamError, err.Error())
			return
		}
		for k, vs := range head.Headers {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		req.Host = head.Host
		req.RemoteAddr = head.RemoteAddr

		w := &agentResponseWriter{agent: a, id: id, header: make(http.Header)}
		h.ServeHTTP(w, req)

		// A reset already terminated the stream; sending a head or an end
		// frame after it would violate the protocol.
		if w.reset {
			return
		}
		if !w.wroteHeader {
			w.WriteHeader(http.StatusOK)
		}
		endFrame, err := tunnelproto.Encode(tunnelproto.FrameResEnd, id, nil)
		if err == nil {
			_ = a.write(endFrame)
		}
	}()
}

// startUpgrade answers an upgrade request with a 101 and registers an echo
// stream. It mirrors the WebSocket handshake (computing Sec-WebSocket-Accept)
// so the 101 is valid to a real client, then echoes REQ_BODY as RES_BODY.
func (a *testAgent) startUpgrade(id uint64, head tunnelproto.RequestHead) {
	_, cancel := context.WithCancel(context.Background())

	if a.declineUpgrade {
		// Answer as an ordinary service that does not speak the protocol.
		cancel()
		body := "no websocket here"
		headFrame, _ := tunnelproto.EncodeJSONFrame(tunnelproto.FrameResHead, id, tunnelproto.ResponseHead{
			Status:  http.StatusUpgradeRequired,
			Headers: map[string][]string{"Content-Length": {strconv.Itoa(len(body))}},
		})
		_ = a.write(headFrame)
		if bodyFrame, err := tunnelproto.Encode(tunnelproto.FrameResBody, id, []byte(body)); err == nil {
			_ = a.write(bodyFrame)
		}
		if endFrame, err := tunnelproto.Encode(tunnelproto.FrameResEnd, id, nil); err == nil {
			_ = a.write(endFrame)
		}
		return
	}

	a.mu.Lock()
	a.streams[id] = &agentStream{cancel: cancel, upgrade: true}
	a.mu.Unlock()

	headers := map[string][]string{
		"Upgrade":    {"websocket"},
		"Connection": {"Upgrade"},
	}
	if key := firstHeaderValue(head.Headers, "sec-websocket-key"); key != "" {
		headers["Sec-Websocket-Accept"] = []string{wsAccept(key)}
	}
	frame, err := tunnelproto.EncodeJSONFrame(tunnelproto.FrameResHead, id, tunnelproto.ResponseHead{
		Status:  http.StatusSwitchingProtocols,
		Headers: headers,
	})
	if err == nil {
		_ = a.write(frame)
	}
}

// startRaw handles a raw (tcp/tls) stream. With no rawBackend it echoes
// REQ_BODY back as RES_BODY; with one it proxies to that backend, so a real TLS
// handshake can complete through the tunnel.
func (a *testAgent) startRaw(id uint64) {
	ctx, cancel := context.WithCancel(context.Background())

	if a.rawBackend == "" {
		a.mu.Lock()
		a.streams[id] = &agentStream{cancel: cancel, upgrade: true}
		a.mu.Unlock()
		return
	}

	backend, err := net.Dial("tcp", a.rawBackend)
	if err != nil {
		cancel()
		a.sendReset(id, tunnelproto.ResetUpstreamError, err.Error())
		return
	}
	a.mu.Lock()
	a.streams[id] = &agentStream{cancel: cancel, raw: backend}
	a.mu.Unlock()

	// Pump backend -> RES_BODY until the backend closes.
	go func() {
		defer a.forget(id)
		buf := make([]byte, 32*1024)
		for {
			n, readErr := backend.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				if frame, err := tunnelproto.Encode(tunnelproto.FrameResBody, id, chunk); err == nil {
					_ = a.write(frame)
				}
			}
			if readErr != nil {
				break
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
		if endFrame, err := tunnelproto.Encode(tunnelproto.FrameResEnd, id, nil); err == nil {
			_ = a.write(endFrame)
		}
	}()
}

// wsAccept computes the Sec-WebSocket-Accept value for a Sec-WebSocket-Key,
// per RFC 6455 section 4.2.2.
func wsAccept(key string) string {
	const magic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	sum := sha1.Sum([]byte(key + magic))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func firstHeaderValue(h map[string][]string, name string) string {
	for k, vs := range h {
		if strings.EqualFold(k, name) && len(vs) > 0 {
			return vs[0]
		}
	}
	return ""
}

func (a *testAgent) sendReset(id uint64, code tunnelproto.ResetCode, msg string) {
	frame, err := tunnelproto.EncodeJSONFrame(tunnelproto.FrameReset, id, tunnelproto.StreamReset{
		Code: code, Message: msg,
	})
	if err == nil {
		_ = a.write(frame)
	}
}

// agentResponseWriter turns handler writes into RES_HEAD / RES_BODY frames.
type agentResponseWriter struct {
	agent       *testAgent
	id          uint64
	header      http.Header
	wroteHeader bool
	reset       bool
}

func (w *agentResponseWriter) Header() http.Header { return w.header }

func (w *agentResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true

	headers := make(map[string][]string, len(w.header))
	for k, vs := range w.header {
		cp := make([]string, len(vs))
		copy(cp, vs)
		headers[k] = cp
	}
	frame, err := tunnelproto.EncodeJSONFrame(tunnelproto.FrameResHead, w.id, tunnelproto.ResponseHead{
		Status: status, Headers: headers,
	})
	if err != nil {
		return
	}
	_ = w.agent.write(frame)
}

func (w *agentResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	total := len(b)
	for len(b) > 0 {
		n := len(b)
		if n > tunnelproto.MaxPayloadBytes {
			n = tunnelproto.MaxPayloadBytes
		}
		frame, err := tunnelproto.Encode(tunnelproto.FrameResBody, w.id, b[:n])
		if err != nil {
			return total - len(b), err
		}
		if err := w.agent.write(frame); err != nil {
			return total - len(b), err
		}
		b = b[n:]
	}
	return total, nil
}

// Flush is a no-op: every Write already emits its own frame.
func (w *agentResponseWriter) Flush() {}

// resetStream lets a test simulate a dead local service.
func (w *agentResponseWriter) resetStream(code tunnelproto.ResetCode, msg string) {
	w.reset = true
	w.agent.sendReset(w.id, code, msg)
}
