package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/anomalysh/rift/projects/server/internal/auth"
	"github.com/anomalysh/rift/projects/server/internal/config"
	"github.com/anomalysh/rift/projects/server/internal/core"
	"github.com/anomalysh/rift/projects/server/internal/registry"
	"github.com/anomalysh/rift/projects/server/internal/store/memory"
	"github.com/anomalysh/rift/projects/server/internal/tunnelproto"
)

// spyRegistry records every session registered through it.
type spyRegistry struct {
	*registry.Local
	mu         sync.Mutex
	registered []core.Session
}

func (r *spyRegistry) Register(ctx context.Context, s core.Session) (core.Session, error) {
	r.mu.Lock()
	r.registered = append(r.registered, s)
	r.mu.Unlock()
	return r.Local.Register(ctx, s)
}

func (r *spyRegistry) sessions() []core.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.Session(nil), r.registered...)
}

// handshakeHarness is a whole Gateway behind a test server, with one token.
type handshakeHarness struct {
	gw    *Gateway
	store *memory.Store
	reg   *spyRegistry
	url   string
	token string
}

// newHandshakeHarness builds the gateway. tunnels, when set, wraps the
// in-memory tunnel store the gateway is given.
func newHandshakeHarness(t *testing.T, tunnels func(core.TunnelStore) core.TunnelStore) *handshakeHarness {
	t.Helper()
	rules, err := core.NewSubdomainRules(3, 63, `^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`,
		config.DefaultSubdomainBlocklist, config.DefaultSubdomainGenerator, 10, config.DefaultSubdomainGenAlphabet)
	if err != nil {
		t.Fatalf("subdomain rules: %v", err)
	}
	cfg := testConfig()
	cfg.NodeID = "test-node"
	cfg.Tunnel.MaxTunnelsPerToken = 5
	cfg.SubdomainRules = rules

	store := memory.New()
	plaintext, hash, err := auth.Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := store.Tokens().Create(context.Background(), &core.Token{
		ID: core.MustNewID(time.Now()), Name: "test", TokenHash: hash, MaxTunnels: 5, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create token: %v", err)
	}

	var ts core.TunnelStore = store.Tunnels()
	if tunnels != nil {
		ts = tunnels(ts)
	}
	reg := &spyRegistry{Local: registry.NewLocal()}
	gw := New(cfg, testLogger(), store.Tokens(), store.Reservations(), ts, store.Domains(), reg)
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return &handshakeHarness{gw: gw, store: store, reg: reg, url: "ws" + strings.TrimPrefix(srv.URL, "http"), token: plaintext}
}

// hello connects an agent, sends hello (with the harness token), and returns
// the socket and the gateway's reply.
func (h *handshakeHarness) hello(t *testing.T, hello tunnelproto.Hello) (*websocket.Conn, tunnelproto.ControlEnvelope) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, h.url, &websocket.DialOptions{Subprotocols: []string{tunnelproto.Subprotocol}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	hello.ProtocolVersion = tunnelproto.Version
	hello.Token = h.token
	frame, err := tunnelproto.EncodeControl(tunnelproto.ControlHello, hello)
	if err != nil {
		t.Fatalf("encode hello: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	return conn, readControl(t, conn)
}

// readControl reads the next frame from an agent socket, which must be a
// CONTROL frame.
func readControl(t *testing.T, conn *websocket.Conn) tunnelproto.ControlEnvelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	f, err := tunnelproto.Decode(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	env, err := tunnelproto.DecodeControl(f.Payload)
	if err != nil {
		t.Fatalf("decode control: %v", err)
	}
	return env
}

// A session rejected after it was registered (here: a tcp tunnel on a server
// with tcp disabled) never starts its loops. It must still be closed, or the
// agent-chosen TTL timer keeps it alive and requests routed to it hang.
func TestRejectAfterRegisterClosesSession(t *testing.T) {
	h := newHandshakeHarness(t, nil) // tcp is disabled: rejected after registration

	_, env := h.hello(t, tunnelproto.Hello{
		Protocol:  string(core.ProtocolTCP),
		Subdomain: "rawapp",
		// An hour-long TTL: the timer would pin the session for that long.
		Policy: &core.Policy{TTLSeconds: 3600},
	})
	if env.Type != tunnelproto.ControlHelloError {
		t.Fatalf("reply = %+v; want hello_error", env)
	}

	regs := h.reg.sessions()
	if len(regs) != 1 {
		t.Fatalf("registered %d sessions, want 1", len(regs))
	}
	sess := regs[0].(*session)
	// The session is closed before the rejection is written, so by the time
	// the agent has read it the session must already be closed.
	assertClosedWith(t, sess, tunnelproto.ShutdownServerShutdown)
	if sess.ttlTimer == nil {
		t.Fatal("expected a ttl timer to have been armed")
	}
	if sess.ttlTimer.Stop() {
		t.Fatal("the rejected session's ttl timer was still armed")
	}

	// A request routed to the dead session fails fast instead of hanging.
	req := httptest.NewRequest(http.MethodGet, "http://rawapp.rift.test/", nil)
	if _, err := sess.RoundTrip(req); err == nil {
		t.Fatal("RoundTrip on a rejected session succeeded")
	}
}

// gatedTunnels holds every Release until gate is closed, so a test can
// observe a tunnel mid-teardown.
type gatedTunnels struct {
	core.TunnelStore
	gate chan struct{}
}

func (g *gatedTunnels) Release(ctx context.Context, id string) error {
	select {
	case <-g.gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	return g.TunnelStore.Release(ctx, id)
}

// Shutdown must wait for each tunnel's teardown (its shutdown frame and the
// release of its row), not just request it, or the process exits with rows
// still claiming subdomains. A tunnel that connects once shutdown has begun
// is told to go away rather than left running.
func TestShutdownWaitsForTunnelTeardown(t *testing.T) {
	gated := &gatedTunnels{gate: make(chan struct{})}
	h := newHandshakeHarness(t, func(ts core.TunnelStore) core.TunnelStore {
		gated.TunnelStore = ts
		return gated
	})
	conn, env := h.hello(t, tunnelproto.Hello{Protocol: string(core.ProtocolHTTP), Subdomain: "shutapp"})
	if env.Type != tunnelproto.ControlHelloOK {
		t.Fatalf("reply = %+v; want hello_ok", env)
	}

	// Teardown is stuck releasing the row: Shutdown must still be waiting
	// when its context ends.
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := h.gw.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown with a tunnel mid-teardown = %v, want DeadlineExceeded", err)
	}
	if env := readControl(t, conn); env.Type != tunnelproto.ControlShutdown {
		t.Fatalf("agent got %s, want shutdown", env.Type)
	}

	close(gated.gate)
	if err := h.gw.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// This agent stopped reading after the shutdown frame, as a real one does,
	// so it never answers the close handshake. Teardown gives it
	// closeHandshakeGrace, then drops the socket; it used to wait out
	// coder/websocket's own 5s handshake timeout instead.
	if elapsed := time.Since(start); elapsed > closeHandshakeGrace+2*time.Second {
		t.Fatalf("teardown took %v against an agent that stopped reading", elapsed)
	}
	if _, err := h.store.Tunnels().GetBySubdomain(context.Background(), "shutapp"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("tunnel row after Shutdown: err = %v, want ErrNotFound", err)
	}

	// A late arrival is shut down as soon as it is established.
	late, env := h.hello(t, tunnelproto.Hello{Protocol: string(core.ProtocolHTTP), Subdomain: "latecomer"})
	if env.Type != tunnelproto.ControlHelloOK {
		t.Fatalf("late reply = %+v; want hello_ok", env)
	}
	if env := readControl(t, late); env.Type != tunnelproto.ControlShutdown {
		t.Fatalf("late agent got %s, want shutdown", env.Type)
	}
}
