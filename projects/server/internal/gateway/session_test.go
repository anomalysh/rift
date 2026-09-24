package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/anomalysh/rift/projects/server/internal/core"
	"github.com/anomalysh/rift/projects/server/internal/tunnelproto"
)

// maxRequests only reads the tunnel policy, so a bare session literal exercises
// the A4 --once / --max-requests quota mapping without a live connection.
func TestSessionMaxRequests(t *testing.T) {
	cases := []struct {
		name   string
		policy core.Policy
		want   int64
	}{
		{"unbounded", core.Policy{}, 0},
		{"once", core.Policy{Once: true}, 1},
		{"max-requests", core.Policy{MaxRequests: 5}, 5},
		{"once-wins-over-max", core.Policy{Once: true, MaxRequests: 5}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &session{tunnel: core.Tunnel{Policy: tc.policy}}
			if got := s.maxRequests(); got != tc.want {
				t.Fatalf("maxRequests() = %d, want %d", got, tc.want)
			}
		})
	}
}

// The request gate admits exactly maxRequests before it starts rejecting, so
// a --max-requests=3 tunnel serves 3, refuses the 4th, and retires.
func TestSessionRequestQuotaGate(t *testing.T) {
	s := newSession(nil, core.Tunnel{ID: "t", Policy: core.Policy{MaxRequests: 3}},
		testConfig(), &countingTunnels{}, nil, testLogger())
	served := 0
	for n := 1; n <= 5; n++ {
		if err := s.admitRequest(); err == nil {
			served++
		} else if !errors.Is(err, errSessionClosed) {
			t.Fatalf("request %d: err = %v, want errSessionClosed", n, err)
		}
	}
	if served != 3 {
		t.Fatalf("served %d requests, want 3 within the quota", served)
	}
	assertClosedWith(t, s, tunnelproto.ShutdownPolicyExpired)
}

// RES_BODY after RES_END used to send on the closed body channel and panic
// the read loop, killing the whole process. It must now be dropped, and the
// session must keep serving.
func TestBodyAfterEndDoesNotPanic(t *testing.T) {
	s, a := newTestSession(t, testConfig(), core.Policy{}, nil)

	id, res := startRoundTrip(t, s, a)
	a.sendHead(id, http.StatusOK)
	a.send(tunnelproto.FrameResBody, id, []byte("hello"))
	a.send(tunnelproto.FrameResEnd, id, nil)
	a.send(tunnelproto.FrameResEnd, id, nil) // a duplicate end is ignored too
	a.send(tunnelproto.FrameResBody, id, []byte("after end"))
	a.expectReset(id)

	r := awaitResult(t, res)
	if r.err != nil {
		t.Fatalf("RoundTrip: %v", r.err)
	}
	body, err := io.ReadAll(r.resp.Body)
	_ = r.resp.Body.Close()
	if err != nil || string(body) != "hello" {
		t.Fatalf("body = %q, %v; want %q", body, err, "hello")
	}

	// The same after a RESET: the stream is aborted but still registered
	// until its consumer lets go, so RES_END then RES_BODY reach it.
	id2, res2 := startRoundTrip(t, s, a)
	a.send(tunnelproto.FrameReset, id2, []byte(`{"code":"upstream_error"}`))
	a.send(tunnelproto.FrameResEnd, id2, nil)
	a.send(tunnelproto.FrameResBody, id2, []byte("after reset"))
	if r := awaitResult(t, res2); r.err == nil {
		t.Fatal("RoundTrip after RESET succeeded, want an error")
	}

	// And after RES_HEAD, RES_END, RES_BODY with the body still unread.
	id3, res3 := startRoundTrip(t, s, a)
	a.sendHead(id3, http.StatusOK)
	a.send(tunnelproto.FrameResEnd, id3, nil)
	a.send(tunnelproto.FrameResBody, id3, []byte("late"))
	a.expectReset(id3)
	r3 := awaitResult(t, res3)
	if r3.err != nil {
		t.Fatalf("RoundTrip: %v", r3.err)
	}
	_ = r3.resp.Body.Close()

	// The read loop survived (a recovered panic would have closed the session).
	id4, res4 := startRoundTrip(t, s, a)
	a.sendHead(id4, http.StatusNoContent)
	a.send(tunnelproto.FrameResEnd, id4, nil)
	r4 := awaitResult(t, res4)
	if r4.err != nil {
		t.Fatalf("follow-up RoundTrip: %v", r4.err)
	}
	_ = r4.resp.Body.Close()
	assertOpen(t, s)
}

// A public client that stops reading must cost only its own stream: the read
// loop gives it a short stall budget, resets it, and keeps serving everything
// else, pings included, so the tunnel does not die of a heartbeat timeout.
func TestSlowReaderDoesNotStallTunnel(t *testing.T) {
	cfg := testConfig()
	cfg.Tunnel.StreamBufferSize = 2
	cfg.Tunnel.HeartbeatInterval = 50 * time.Millisecond
	cfg.Tunnel.HeartbeatTimeout = 600 * time.Millisecond // stall budget: 150ms
	s, a := newTestSession(t, cfg, core.Policy{}, nil)
	if s.bodyStallBudget != cfg.Tunnel.HeartbeatTimeout/4 {
		t.Fatalf("stall budget = %v, want HeartbeatTimeout/4", s.bodyStallBudget)
	}

	// Keep the agent visibly alive for the watchdog.
	stopPings := make(chan struct{})
	pingsDone := make(chan struct{})
	go func() {
		defer close(pingsDone)
		tick := time.NewTicker(40 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopPings:
				return
			case <-tick.C:
				frame, _ := tunnelproto.EncodeControl(tunnelproto.ControlPing, tunnelproto.Heartbeat{TS: 1})
				if err := a.conn.Write(context.Background(), websocket.MessageBinary, frame); err != nil {
					return
				}
			}
		}
	}()
	defer func() { close(stopPings); <-pingsDone }()

	// Stream A: the public side never reads its body.
	idA, resA := startRoundTrip(t, s, a)
	a.sendHead(idA, http.StatusOK)
	rA := awaitResult(t, resA)
	if rA.err != nil {
		t.Fatalf("RoundTrip A: %v", rA.err)
	}
	defer func() { _ = rA.resp.Body.Close() }()
	chunk := make([]byte, 1024)
	for i := 0; i < 10; i++ {
		a.send(tunnelproto.FrameResBody, idA, chunk)
	}

	// Stream B must be served promptly, long before RequestTimeout.
	start := time.Now()
	idB, resB := startRoundTrip(t, s, a)
	a.sendHead(idB, http.StatusOK)
	a.send(tunnelproto.FrameResBody, idB, []byte("b"))
	a.send(tunnelproto.FrameResEnd, idB, nil)
	rB := awaitResult(t, resB)
	if rB.err != nil {
		t.Fatalf("RoundTrip B: %v", rB.err)
	}
	body, err := io.ReadAll(rB.resp.Body)
	_ = rB.resp.Body.Close()
	if err != nil || string(body) != "b" {
		t.Fatalf("body B = %q, %v", body, err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("stream B took %v behind a stalled stream", elapsed)
	}

	// A was reset, on both sides.
	a.expectReset(idA)
	if _, err := io.ReadAll(rA.resp.Body); err == nil {
		t.Fatal("reading the stalled stream succeeded, want a reset error")
	} else if code, ok := tunnelproto.ResetCodeOf(err); !ok || code != tunnelproto.ResetUpstreamTimeout {
		t.Fatalf("stalled stream error = %v, want an upstream_timeout reset", err)
	}

	// Outlive the heartbeat timeout: the tunnel must still be up.
	time.Sleep(cfg.Tunnel.HeartbeatTimeout + 200*time.Millisecond)
	assertOpen(t, s)
}

// Every ping is answered, but a flood of them reaches the store only once per
// half heartbeat interval.
func TestPingFloodIsNotPersistedPerPing(t *testing.T) {
	tunnels := &countingTunnels{}
	s, a := newTestSession(t, testConfig(), core.Policy{}, tunnels)

	const pings = 100
	for i := 0; i < pings; i++ {
		a.ping()
	}
	for i := 0; i < pings; i++ {
		f := a.expect(tunnelproto.FrameControl)
		env, err := tunnelproto.DecodeControl(f.Payload)
		if err != nil || env.Type != tunnelproto.ControlPong {
			t.Fatalf("control frame %d = %+v, %v; want a pong", i, env, err)
		}
	}
	// The write runs off the read loop, so it may land after the pongs. Every
	// ping has been through the throttle by now, though, so the count can only
	// grow to 1.
	deadline := time.Now().Add(5 * time.Second)
	for tunnels.heartbeats.Load() == 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if n := tunnels.heartbeats.Load(); n != 1 {
		t.Fatalf("persisted %d heartbeats for %d pings, want 1", n, pings)
	}
	assertOpen(t, s)
}

// blockingTunnels is a TunnelStore whose Heartbeat hangs until released or
// its context ends, as a stuck database would.
type blockingTunnels struct {
	core.TunnelStore
	release chan struct{}
}

func (b *blockingTunnels) Heartbeat(ctx context.Context, _ string, _ time.Time) error {
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// A hung tunnel store must not freeze the tunnel. The heartbeat write used
// to run on the read loop with no deadline, so one stuck write stopped every
// stream, and every pong, on the tunnel for as long as the store hung.
func TestHungHeartbeatWriteDoesNotStallReadLoop(t *testing.T) {
	tunnels := &blockingTunnels{release: make(chan struct{})}
	defer close(tunnels.release)
	s, a := newTestSession(t, testConfig(), core.Policy{}, tunnels)

	a.ping() // persisted: the write hangs
	a.expect(tunnelproto.FrameControl)
	a.ping() // throttled: answered straight away
	a.expect(tunnelproto.FrameControl)

	id, res := startRoundTrip(t, s, a)
	a.sendHead(id, http.StatusNoContent)
	a.send(tunnelproto.FrameResEnd, id, nil)
	r := awaitResult(t, res)
	if r.err != nil {
		t.Fatalf("RoundTrip behind a hung heartbeat write: %v", r.err)
	}
	_ = r.resp.Body.Close()
	assertOpen(t, s)
}

// blockingTokens is a TokenStore whose FindByID hangs until its context ends.
type blockingTokens struct{ core.TokenStore }

func (blockingTokens) FindByID(ctx context.Context, _ string) (*core.Token, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// The watchdog enforces the heartbeat timeout and revalidates the token on
// the same goroutine. A token lookup with no deadline against a hung store
// used to suspend it for good, so a dead agent's tunnel was never closed.
func TestHungTokenStoreDoesNotSuspendWatchdog(t *testing.T) {
	cfg := testConfig()
	cfg.Gateway.WriteTimeout = 100 * time.Millisecond
	cfg.Tunnel.HeartbeatInterval = 20 * time.Millisecond
	cfg.Tunnel.HeartbeatTimeout = 300 * time.Millisecond
	cfg.Tunnel.TokenRevalidateInterval = time.Nanosecond // every tick
	s, _ := newTestSessionWithTokens(t, cfg, core.Policy{}, nil, blockingTokens{})

	// The agent never sends a frame.
	select {
	case <-s.closing:
	case <-time.After(5 * time.Second):
		t.Fatal("a silent agent's tunnel was never closed")
	}
	assertClosedWith(t, s, tunnelproto.ShutdownHeartbeatTimeout)
}

// --once must also bound Upgrade requests; otherwise adding an Upgrade header
// to each request bypasses the quota.
func TestUpgradeCountsAgainstRequestQuota(t *testing.T) {
	s, a := newTestSession(t, testConfig(), core.Policy{Once: true}, nil)

	upgrade := func() chan roundTripResult {
		req := httptest.NewRequest(http.MethodGet, "http://app.rift.test/ws", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "x")
		out := make(chan roundTripResult, 1)
		go func() {
			resp, tconn, err := s.Upgrade(req)
			if tconn != nil {
				_ = tconn.Close()
			}
			out <- roundTripResult{resp, err}
		}()
		return out
	}

	// The first upgrade is served (the service declines to switch).
	first := upgrade()
	head := a.expect(tunnelproto.FrameReqHead)
	a.sendHead(head.StreamID, http.StatusOK)
	a.send(tunnelproto.FrameResEnd, head.StreamID, nil)
	r := awaitResult(t, first)
	if r.err != nil {
		t.Fatalf("first upgrade: %v", r.err)
	}
	_ = r.resp.Body.Close()

	// The second is over quota: refused, and the tunnel retires.
	if r := awaitResult(t, upgrade()); r.err == nil {
		t.Fatal("second upgrade on a --once tunnel was served")
	}
	// The refusal retires the tunnel before Upgrade returns.
	assertClosedWith(t, s, tunnelproto.ShutdownPolicyExpired)
}

// A status net/http cannot write (it panics in WriteHeader) or a 101 on an
// ordinary request must fail the stream, not reach the ingress.
func TestInvalidResponseStatusIsRejected(t *testing.T) {
	s, a := newTestSession(t, testConfig(), core.Policy{}, nil)

	for _, status := range []int{0, -1, 99, 600, 1000, http.StatusSwitchingProtocols} {
		id, res := startRoundTrip(t, s, a)
		a.sendHead(id, status)
		r := awaitResult(t, res)
		if !errors.Is(r.err, errInvalidResponseStatus) {
			if r.resp != nil {
				_ = r.resp.Body.Close()
			}
			t.Fatalf("status %d: err = %v, want errInvalidResponseStatus", status, r.err)
		}
		a.expectReset(id)
		if _, ok := s.lookupStream(id); ok {
			t.Fatalf("status %d: rejected stream is still registered", status)
		}
	}

	// Legitimate statuses still pass, including an informational one.
	for _, status := range []int{http.StatusOK, http.StatusNotFound, 599} {
		id, res := startRoundTrip(t, s, a)
		a.sendHead(id, status)
		a.send(tunnelproto.FrameResEnd, id, nil)
		r := awaitResult(t, res)
		if r.err != nil {
			t.Fatalf("status %d rejected: %v", status, r.err)
		}
		_ = r.resp.Body.Close()
	}
	assertOpen(t, s)
}

// Close must not wait on a Write that is blocked on a full send queue: that
// Write holds writeMu, and only Close's cancel can release it.
func TestTunnelConnCloseUnblocksPendingWrite(t *testing.T) {
	cfg := testConfig()
	cfg.Tunnel.StreamBufferSize = 1
	// No loops run, so nothing drains the send queue.
	s := newSession(nil, core.Tunnel{ID: "t"}, cfg, &countingTunnels{}, nil, testLogger())
	defer func() { _ = s.Close("test") }()

	tc, err := s.OpenRaw(context.Background()) // its REQ_HEAD fills the queue
	if err != nil {
		t.Fatalf("OpenRaw: %v", err)
	}

	writeErr := make(chan error, 1)
	go func() {
		_, err := tc.Write([]byte("stuck"))
		writeErr <- err
	}()
	// Wait until the Write holds writeMu. The queue is full, so from there it
	// can only be blocked in enqueue (or about to be): the case under test.
	conn := tc.(*tunnelConn)
	for conn.writeMu.TryLock() {
		conn.writeMu.Unlock()
		runtime.Gosched()
	}

	closed := make(chan struct{})
	go func() {
		_ = tc.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked behind a pending Write")
	}
	select {
	case err := <-writeErr:
		if err == nil {
			t.Fatal("the blocked Write reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the blocked Write never returned")
	}
}

// Streams beyond the per-session cap fail fast on every entry point.
func TestStreamCapPerSession(t *testing.T) {
	s := newSession(nil, core.Tunnel{ID: "t"}, testConfig(), &countingTunnels{}, nil, testLogger())
	defer func() { _ = s.Close("test") }()

	s.streamsMu.Lock()
	for i := 0; i < maxStreamsPerSession; i++ {
		id := s.nextID.Add(1)
		s.streams[id] = newStream(id, 1)
	}
	s.streamsMu.Unlock()

	if _, err := s.OpenRaw(context.Background()); !errors.Is(err, errTooManyStreams) {
		t.Fatalf("OpenRaw at the cap: err = %v, want errTooManyStreams", err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://app.rift.test/", nil)
	if _, err := s.RoundTrip(req); !errors.Is(err, errTooManyStreams) {
		t.Fatalf("RoundTrip at the cap: err = %v, want errTooManyStreams", err)
	}
	if _, _, err := s.Upgrade(req); !errors.Is(err, errTooManyStreams) {
		t.Fatalf("Upgrade at the cap: err = %v, want errTooManyStreams", err)
	}
	if !errors.Is(errTooManyStreams, core.ErrTunnelUnavailable) {
		t.Fatal("errTooManyStreams must map to ErrTunnelUnavailable for the ingress")
	}

	// Releasing one stream makes room again.
	s.forgetStream(1)
	if _, err := s.OpenRaw(context.Background()); err != nil {
		t.Fatalf("OpenRaw after a stream ended: %v", err)
	}
}

// When the agent's half of a raw stream ends first (RES_END) and the public
// side then closes without half-closing, Close must end the gateway's half
// with REQ_END: the agent holds its local connection until both halves end.
// Nothing for the stream may follow Close, not even from a late Write or
// CloseWrite racing it.
func TestTunnelConnCloseEndsOpenWriteHalf(t *testing.T) {
	s, a := newTestSession(t, testConfig(), core.Policy{}, nil)

	tc, err := s.OpenRaw(context.Background())
	if err != nil {
		t.Fatalf("OpenRaw: %v", err)
	}
	id := a.expect(tunnelproto.FrameReqHead).StreamID
	a.send(tunnelproto.FrameResBody, id, []byte("bye"))
	a.send(tunnelproto.FrameResEnd, id, nil)
	if got, err := io.ReadAll(tc); err != nil || string(got) != "bye" {
		t.Fatalf("read = %q, %v; want %q", got, err, "bye")
	}

	if err := tc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if f := a.nextDataFrame(); f.StreamID != id || f.Type != tunnelproto.FrameReqEnd {
		t.Fatalf("after Close the agent got %s on stream %d, want REQ_END on %d", f.Type, f.StreamID, id)
	}

	if _, err := tc.Write([]byte("late")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Write after Close: err = %v, want io.ErrClosedPipe", err)
	}
	if err := tc.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite after Close: %v", err)
	}
	// A fresh stream's REQ_HEAD is queued behind anything the late calls sent.
	if _, err := s.OpenRaw(context.Background()); err != nil {
		t.Fatalf("second OpenRaw: %v", err)
	}
	if f := a.nextDataFrame(); f.StreamID == id {
		t.Fatalf("the closed stream sent %s after Close", f.Type)
	}
}

// A raw stream the public side already half-closed is not ended twice, and
// one the agent never finished is reset rather than ended.
func TestTunnelConnCloseAfterHalfClose(t *testing.T) {
	s, a := newTestSession(t, testConfig(), core.Policy{}, nil)

	tc, err := s.OpenRaw(context.Background())
	if err != nil {
		t.Fatalf("OpenRaw: %v", err)
	}
	id := a.expect(tunnelproto.FrameReqHead).StreamID
	if err := tc.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	if f := a.nextDataFrame(); f.StreamID != id || f.Type != tunnelproto.FrameReqEnd {
		t.Fatalf("CloseWrite sent %s on stream %d, want REQ_END on %d", f.Type, f.StreamID, id)
	}
	_ = tc.Close()
	if f := a.nextDataFrame(); f.StreamID != id || f.Type != tunnelproto.FrameReset {
		t.Fatalf("Close of an unfinished stream sent %s on stream %d, want RESET on %d", f.Type, f.StreamID, id)
	}
}

// A request the public client abandons before the response head must stop
// its body pump: the stream is aborted, not merely forgotten, so bytes the
// client sends afterwards are dropped rather than following the RESET as
// REQ_BODY frames, and the body is closed instead of read to the end.
func TestCanceledRequestStopsBodyPump(t *testing.T) {
	s, a := newTestSession(t, testConfig(), core.Policy{}, nil)

	body, client := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "http://app.rift.test/upload", body).WithContext(ctx)
	req.ContentLength = -1 // streamed, length unknown
	res := make(chan roundTripResult, 1)
	go func() {
		resp, err := s.RoundTrip(req)
		res <- roundTripResult{resp, err}
	}()
	id := a.expect(tunnelproto.FrameReqHead).StreamID

	cancel()
	if r := awaitResult(t, res); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("RoundTrip err = %v, want context.Canceled", r.err)
	}
	if f := a.nextDataFrame(); f.StreamID != id || f.Type != tunnelproto.FrameReset {
		t.Fatalf("after cancel the agent got %s on stream %d, want RESET on %d", f.Type, f.StreamID, id)
	}

	// The pump was blocked reading when the request was abandoned. It takes
	// this chunk, drops it, and closes the body.
	if _, err := client.Write([]byte("late")); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if _, err := client.Write([]byte("more")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("second body write: err = %v, want io.ErrClosedPipe (body still being read)", err)
	}
	// A fresh stream's REQ_HEAD is queued behind anything the pump sent.
	if _, err := s.OpenRaw(context.Background()); err != nil {
		t.Fatalf("OpenRaw: %v", err)
	}
	if f := a.nextDataFrame(); f.StreamID == id {
		t.Fatalf("the abandoned stream sent %s after its RESET", f.Type)
	}
}
