package e2e

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/anomaly-sh/rift/server/internal/tunnelproto"
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

	hello tunnelproto.HelloOK
	done  chan struct{}
}

type agentStream struct {
	pw     *io.PipeWriter
	cancel context.CancelFunc
}

// dialAgent completes the handshake and starts serving h through the tunnel.
func dialAgent(t *testing.T, ctx context.Context, gatewayURL, token, subdomain string, h http.Handler) (*testAgent, error) {
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
		Protocol:        "http",
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
			a.startRequest(frame.StreamID, head, h)

		case tunnelproto.FrameReqBody:
			if st := a.stream(frame.StreamID); st != nil {
				chunk := make([]byte, len(frame.Payload))
				copy(chunk, frame.Payload)
				_, _ = st.pw.Write(chunk)
			}

		case tunnelproto.FrameReqEnd:
			if st := a.stream(frame.StreamID); st != nil {
				_ = st.pw.Close()
			}

		case tunnelproto.FrameReset:
			if st := a.stream(frame.StreamID); st != nil {
				st.cancel()
				_ = st.pw.CloseWithError(errors.New("reset by gateway"))
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
