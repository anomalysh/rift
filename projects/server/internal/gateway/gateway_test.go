package gateway

import (
	"context"
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

// A session rejected after it was registered (here: a tcp tunnel on a server
// with tcp disabled) never starts its loops. It must still be closed, or the
// agent-chosen TTL timer keeps it alive and requests routed to it hang.
func TestRejectAfterRegisterClosesSession(t *testing.T) {
	rules, err := core.NewSubdomainRules(3, 63, `^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`,
		config.DefaultSubdomainBlocklist, config.DefaultSubdomainGenerator, 10, config.DefaultSubdomainGenAlphabet)
	if err != nil {
		t.Fatalf("subdomain rules: %v", err)
	}
	cfg := testConfig()
	cfg.NodeID = "test-node"
	cfg.Tunnel.MaxTunnelsPerToken = 5
	cfg.SubdomainRules = rules
	// cfg.TCP.Enabled is false: a tcp hello is rejected after registration.

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

	reg := &spyRegistry{Local: registry.NewLocal()}
	gw := New(cfg, testLogger(), store.Tokens(), store.Reservations(), store.Tunnels(), store.Domains(), reg)
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"),
		&websocket.DialOptions{Subprotocols: []string{tunnelproto.Subprotocol}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	hello, err := tunnelproto.EncodeControl(tunnelproto.ControlHello, tunnelproto.Hello{
		ProtocolVersion: tunnelproto.Version,
		Token:           plaintext,
		Protocol:        string(core.ProtocolTCP),
		Subdomain:       "rawapp",
		// An hour-long TTL: the timer would pin the session for that long.
		Policy: &core.Policy{TTLSeconds: 3600},
	})
	if err != nil {
		t.Fatalf("encode hello: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageBinary, hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	f, err := tunnelproto.Decode(data)
	if err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	env, err := tunnelproto.DecodeControl(f.Payload)
	if err != nil || env.Type != tunnelproto.ControlHelloError {
		t.Fatalf("reply = %+v, %v; want hello_error", env, err)
	}

	regs := reg.sessions()
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
