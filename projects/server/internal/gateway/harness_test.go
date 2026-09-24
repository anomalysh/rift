package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/anomalysh/rift/projects/server/internal/config"
	"github.com/anomalysh/rift/projects/server/internal/core"
	"github.com/anomalysh/rift/projects/server/internal/tunnelproto"
)

// testLogger discards logs unless RIFT_TEST_DEBUG is set.
func testLogger() *slog.Logger {
	var sink io.Writer = io.Discard
	if os.Getenv("RIFT_TEST_DEBUG") != "" {
		sink = os.Stderr
	}
	return slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// testConfig is a minimal config for driving a session directly. The watchdog
// ticks hourly, so a test never races a heartbeat timeout unless it asks to.
func testConfig() *config.Config {
	return &config.Config{
		Gateway: config.Gateway{
			HandshakeTimeout: 5 * time.Second,
			WriteTimeout:     5 * time.Second,
		},
		Tunnel: config.Tunnel{
			BaseDomain:              "rift.test",
			PublicScheme:            config.SchemeHTTPS,
			HeartbeatInterval:       time.Hour,
			HeartbeatTimeout:        2 * time.Hour,
			TokenRevalidateInterval: time.Hour,
			RequestTimeout:          30 * time.Second,
			StreamBufferSize:        8,
		},
	}
}

// countingTunnels is a TunnelStore that only supports Heartbeat, which it
// counts. The session touches nothing else.
type countingTunnels struct {
	core.TunnelStore
	heartbeats atomic.Int64
}

func (c *countingTunnels) Heartbeat(context.Context, string, time.Time) error {
	c.heartbeats.Add(1)
	return nil
}

// fakeAgent is the agent end of a live session's WebSocket.
type fakeAgent struct {
	t      *testing.T
	conn   *websocket.Conn
	frames chan tunnelproto.Frame
}

// newTestSession runs a real session (all three loops) over a loopback
// WebSocket and returns it with the agent end.
func newTestSession(t *testing.T, cfg *config.Config, pol core.Policy, tunnels core.TunnelStore) (*session, *fakeAgent) {
	t.Helper()
	if tunnels == nil {
		tunnels = &countingTunnels{}
	}
	sessCh := make(chan *session, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{tunnelproto.Subprotocol}})
		if err != nil {
			return
		}
		conn.SetReadLimit(int64(tunnelproto.MaxFrameBytes))
		s := newSession(conn, core.Tunnel{ID: "tun-1", Subdomain: "app", Policy: pol}, cfg, tunnels, nil, testLogger())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s.wg.Add(3)
		go s.writeLoop()
		go s.watchdog(ctx)
		go s.readLoop(ctx)
		sessCh <- s
		<-s.closing
		s.wg.Wait()
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"),
		&websocket.DialOptions{Subprotocols: []string{tunnelproto.Subprotocol}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.SetReadLimit(int64(tunnelproto.MaxFrameBytes))

	var sess *session
	select {
	case sess = <-sessCh:
	case <-ctx.Done():
		t.Fatal("session did not start")
	}

	a := &fakeAgent{t: t, conn: conn, frames: make(chan tunnelproto.Frame, 4096)}
	go func() {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				close(a.frames)
				return
			}
			f, err := tunnelproto.Decode(data)
			if err != nil {
				continue
			}
			a.frames <- f
		}
	}()

	t.Cleanup(func() {
		_ = sess.Close(string(tunnelproto.ShutdownServerShutdown))
		_ = conn.CloseNow()
		srv.Close()
	})
	return sess, a
}

func (a *fakeAgent) send(typ tunnelproto.FrameType, id uint64, payload []byte) {
	a.t.Helper()
	frame, err := tunnelproto.Encode(typ, id, payload)
	if err != nil {
		a.t.Fatalf("encode %s: %v", typ, err)
	}
	if err := a.conn.Write(context.Background(), websocket.MessageBinary, frame); err != nil {
		a.t.Fatalf("write %s: %v", typ, err)
	}
}

func (a *fakeAgent) sendHead(id uint64, status int) {
	a.t.Helper()
	frame, err := tunnelproto.EncodeJSONFrame(tunnelproto.FrameResHead, id, tunnelproto.ResponseHead{Status: status})
	if err != nil {
		a.t.Fatalf("encode head: %v", err)
	}
	if err := a.conn.Write(context.Background(), websocket.MessageBinary, frame); err != nil {
		a.t.Fatalf("write head: %v", err)
	}
}

func (a *fakeAgent) ping() {
	a.t.Helper()
	frame, err := tunnelproto.EncodeControl(tunnelproto.ControlPing, tunnelproto.Heartbeat{TS: time.Now().UnixMilli()})
	if err != nil {
		a.t.Fatalf("encode ping: %v", err)
	}
	if err := a.conn.Write(context.Background(), websocket.MessageBinary, frame); err != nil {
		a.t.Fatalf("write ping: %v", err)
	}
}

// expect returns the next frame of type typ, skipping control frames (pongs)
// and anything else. It fails the test after a timeout.
func (a *fakeAgent) expect(typ tunnelproto.FrameType) tunnelproto.Frame {
	a.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-a.frames:
			if !ok {
				a.t.Fatalf("agent socket closed waiting for %s", typ)
			}
			if f.Type == typ {
				return f
			}
		case <-deadline:
			a.t.Fatalf("timed out waiting for %s", typ)
		}
	}
}

// nextDataFrame returns the next frame that is not a CONTROL frame, so a test
// can assert exactly what the gateway sent next on its streams.
func (a *fakeAgent) nextDataFrame() tunnelproto.Frame {
	a.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-a.frames:
			if !ok {
				a.t.Fatal("agent socket closed waiting for a data frame")
			}
			if f.Type != tunnelproto.FrameControl {
				return f
			}
		case <-deadline:
			a.t.Fatal("timed out waiting for a data frame")
		}
	}
}

// expectReset waits for a RESET on stream id.
func (a *fakeAgent) expectReset(id uint64) {
	a.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-a.frames:
			if !ok {
				a.t.Fatalf("agent socket closed waiting for RESET on %d", id)
			}
			if f.Type == tunnelproto.FrameReset && f.StreamID == id {
				return
			}
		case <-deadline:
			a.t.Fatalf("timed out waiting for RESET on stream %d", id)
		}
	}
}

// roundTripResult is a RoundTrip outcome delivered from a goroutine.
type roundTripResult struct {
	resp *http.Response
	err  error
}

// startRoundTrip issues a GET through the session in the background and
// returns the stream id the agent sees.
func startRoundTrip(t *testing.T, s *session, a *fakeAgent) (uint64, chan roundTripResult) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://app.rift.test/", nil)
	out := make(chan roundTripResult, 1)
	go func() {
		resp, err := s.RoundTrip(req)
		out <- roundTripResult{resp, err}
	}()
	head := a.expect(tunnelproto.FrameReqHead)
	return head.StreamID, out
}

func awaitResult(t *testing.T, ch chan roundTripResult) roundTripResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("RoundTrip did not return")
		return roundTripResult{}
	}
}

// streamCount reports how many streams the session has in flight.
func (s *session) streamCount() int {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	return len(s.streams)
}

func assertOpen(t *testing.T, s *session) {
	t.Helper()
	select {
	case <-s.closing:
		t.Fatalf("session closed unexpectedly: %s", closeReasonOf(s))
	default:
	}
}
