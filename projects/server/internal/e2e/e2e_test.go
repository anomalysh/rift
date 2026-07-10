// Package e2e wires the real gateway, ingress and registry together with a
// real WebSocket agent and proves a public HTTP request reaches the agent's
// handler and streams back.
package e2e

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/anomalysh/rift/projects/server/internal/auth"
	"github.com/anomalysh/rift/projects/server/internal/config"
	"github.com/anomalysh/rift/projects/server/internal/core"
	"github.com/anomalysh/rift/projects/server/internal/gateway"
	"github.com/anomalysh/rift/projects/server/internal/ingress"
	"github.com/anomalysh/rift/projects/server/internal/registry"
	"github.com/anomalysh/rift/projects/server/internal/store/memory"
	"github.com/anomalysh/rift/projects/server/internal/tunnelproto"
)

const testBaseDomain = "rift.example.test"

type stack struct {
	cfg        *config.Config
	store      *memory.Store
	ingress    *ingress.Ingress
	gw         *gateway.Gateway
	tokens     *flakyTokens
	gatewayWS  string
	ingressURL string
	client     *http.Client
	token      string
	tokenID    string
}

// flakyTokens wraps a TokenStore so a test can make FindByID fail, simulating
// a database outage during token revalidation.
type flakyTokens struct {
	core.TokenStore

	mu  sync.Mutex
	err error
}

func (f *flakyTokens) failFindByID(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *flakyTokens) FindByID(ctx context.Context, id string) (*core.Token, error) {
	f.mu.Lock()
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.TokenStore.FindByID(ctx, id)
}

func unmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func newStack(t *testing.T, tune func(*config.Config)) *stack {
	t.Helper()

	rules, err := core.NewSubdomainRules(3, 63, `^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`,
		config.DefaultSubdomainBlocklist, config.DefaultSubdomainGenerator, 10, config.DefaultSubdomainGenAlphabet)
	if err != nil {
		t.Fatalf("subdomain rules: %v", err)
	}

	cfg := &config.Config{
		Env:    config.EnvDevelopment,
		NodeID: "test-node",
		Gateway: config.Gateway{
			Hostname:         "gateway." + testBaseDomain,
			Path:             "/tunnel",
			HandshakeTimeout: 5 * time.Second,
			WriteTimeout:     5 * time.Second,
		},
		Tunnel: config.Tunnel{
			BaseDomain:              testBaseDomain,
			PublicScheme:            config.SchemeHTTPS,
			HeartbeatInterval:       50 * time.Millisecond,
			HeartbeatTimeout:        2 * time.Second,
			TokenRevalidateInterval: 50 * time.Millisecond,
			RequestTimeout:          5 * time.Second,
			MaxRequestBodyBytes:     1 << 20,
			MaxTunnelsPerToken:      5,
			StreamBufferSize:        8,
		},
		SubdomainRules: rules,
	}
	if tune != nil {
		tune(cfg)
	}

	// RIFT_TEST_DEBUG=1 surfaces server logs when a test is being diagnosed.
	var logSink io.Writer = io.Discard
	if os.Getenv("RIFT_TEST_DEBUG") != "" {
		logSink = os.Stderr
	}
	logger := slog.New(slog.NewTextHandler(logSink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store := memory.New()

	plaintext, hash, err := auth.Mint()
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	tokenID := core.MustNewID(time.Now())
	if err := store.Tokens().Create(context.Background(), &core.Token{
		ID: tokenID, Name: "test", TokenHash: hash,
		MaxTunnels: cfg.Tunnel.MaxTunnelsPerToken, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create token: %v", err)
	}

	tokens := &flakyTokens{TokenStore: store.Tokens()}

	reg := registry.NewLocal()
	gw := gateway.New(cfg, logger, tokens, store.Reservations(), store.Tunnels(), reg)
	ing := ingress.New(cfg, logger, reg, store.Tunnels(), store.Reservations())

	gwMux := http.NewServeMux()
	gwMux.Handle(cfg.Gateway.Path, gw.Handler())

	gwSrv := httptest.NewServer(gwMux)
	ingSrv := httptest.NewServer(ing.Handler())
	t.Cleanup(func() {
		_ = gw.Shutdown(context.Background())
		gwSrv.Close()
		ingSrv.Close()
	})

	return &stack{
		cfg:        cfg,
		store:      store,
		ingress:    ing,
		gw:         gw,
		tokens:     tokens,
		gatewayWS:  "ws" + strings.TrimPrefix(gwSrv.URL, "http") + cfg.Gateway.Path,
		ingressURL: ingSrv.URL,
		client:     ingSrv.Client(),
		token:      plaintext,
		tokenID:    tokenID,
	}
}

// get issues a request to the ingress with the Host header of a tunnel.
func (s *stack) do(t *testing.T, method, subdomain, path string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, s.ingressURL+path, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Host = core.Hostname(subdomain, testBaseDomain)
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return resp
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func TestPublicRequestReachesAgent(t *testing.T) {
	s := newStack(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a, err := dialAgent(t, ctx, s.gatewayWS, s.token, "demo", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "yes")
		fmt.Fprintf(w, "hello from %s %s", r.Method, r.URL.Path)
	}))
	if err != nil {
		t.Fatalf("dial agent: %v", err)
	}
	defer a.close()

	if a.hello.Subdomain != "demo" {
		t.Fatalf("got subdomain %q, want demo", a.hello.Subdomain)
	}
	if want := "https://demo." + testBaseDomain; a.hello.URL != want {
		t.Fatalf("got url %q, want %q", a.hello.URL, want)
	}

	resp := s.do(t, http.MethodGet, "demo", "/greet", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Upstream"); got != "yes" {
		t.Fatalf("X-Upstream = %q, want yes", got)
	}
	if got, want := readAll(t, resp.Body), "hello from GET /greet"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestRequestBodyAndQueryReachAgent(t *testing.T) {
	s := newStack(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a, err := dialAgent(t, ctx, s.gatewayWS, s.token, "echo", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "q=%s body=%s ct=%s", r.URL.Query().Get("n"), body, r.Header.Get("Content-Type"))
	}))
	if err != nil {
		t.Fatalf("dial agent: %v", err)
	}
	defer a.close()

	req, err := http.NewRequest(http.MethodPost, s.ingressURL+"/submit?n=7", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = core.Hostname("echo", testBaseDomain)
	req.Header.Set("Content-Type", "text/plain")

	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if got, want := readAll(t, resp.Body), "q=7 body=payload ct=text/plain"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// A dead local service must surface as 502, not as a hung request.
func TestUpstreamResetBecomesBadGateway(t *testing.T) {
	s := newStack(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a, err := dialAgent(t, ctx, s.gatewayWS, s.token, "dead", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arw, ok := w.(*agentResponseWriter)
		if !ok {
			t.Errorf("expected agentResponseWriter, got %T", w)
			return
		}
		arw.resetStream("upstream_error", "ECONNREFUSED 127.0.0.1:3000")
	}))
	if err != nil {
		t.Fatalf("dial agent: %v", err)
	}
	defer a.close()

	resp := s.do(t, http.MethodGet, "dead", "/", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestUnknownSubdomainIsNotFound(t *testing.T) {
	s := newStack(t, nil)

	resp := s.do(t, http.MethodGet, "nobody", "/", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestForeignHostIsNotFound(t *testing.T) {
	s := newStack(t, nil)

	req, err := http.NewRequest(http.MethodGet, s.ingressURL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "demo.evil.example"

	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// Responses must reach the client incrementally; a buffered proxy would break
// server-sent events and long-polling.
func TestResponseStreamsIncrementally(t *testing.T) {
	s := newStack(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	release := make(chan struct{})
	a, err := dialAgent(t, ctx, s.gatewayWS, s.token, "stream", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "first\n")
		<-release
		_, _ = io.WriteString(w, "second\n")
	}))
	if err != nil {
		t.Fatalf("dial agent: %v", err)
	}
	defer a.close()

	resp := s.do(t, http.MethodGet, "stream", "/events", nil)
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)

	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	if line != "first\n" {
		t.Fatalf("first chunk = %q", line)
	}

	// The handler is still blocked, so receiving "first" proves the gateway
	// did not wait for the whole response before forwarding it.
	close(release)

	line, err = br.ReadString('\n')
	if err != nil {
		t.Fatalf("read second chunk: %v", err)
	}
	if line != "second\n" {
		t.Fatalf("second chunk = %q", line)
	}
}

func TestBlocklistedSubdomainIsRejected(t *testing.T) {
	s := newStack(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := dialAgent(t, ctx, s.gatewayWS, s.token, "api", nopHandler())
	var rej *handshakeRejection
	if !asRejection(err, &rej) {
		t.Fatalf("got %v, want a handshake rejection", err)
	}
	if rej.code != "subdomain_reserved" {
		t.Fatalf("code = %q, want subdomain_reserved", rej.code)
	}
}

func TestInvalidTokenIsRejected(t *testing.T) {
	s := newStack(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := dialAgent(t, ctx, s.gatewayWS, "rift_notarealtoken", "demo", nopHandler())
	var rej *handshakeRejection
	if !asRejection(err, &rej) {
		t.Fatalf("got %v, want a handshake rejection", err)
	}
	if rej.code != "unauthorized" {
		t.Fatalf("code = %q, want unauthorized", rej.code)
	}
}

// A subdomain reserved by another token may not be claimed.
func TestReservationBlocksOtherTokens(t *testing.T) {
	s := newStack(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	otherID := core.MustNewID(time.Now())
	_, hash, _ := auth.Mint()
	if err := s.store.Tokens().Create(ctx, &core.Token{ID: otherID, Name: "other", TokenHash: hash, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.Reservations().Create(ctx, &core.Reservation{
		Subdomain: "mine", TokenID: otherID, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	_, err := dialAgent(t, ctx, s.gatewayWS, s.token, "mine", nopHandler())
	var rej *handshakeRejection
	if !asRejection(err, &rej) {
		t.Fatalf("got %v, want a handshake rejection", err)
	}
	if rej.code != "subdomain_reserved" {
		t.Fatalf("code = %q, want subdomain_reserved", rej.code)
	}
}

// An agent whose socket dropped still owns a tunnel row until the reaper runs.
// Reconnecting must take that row over rather than trip the per-token limit,
// which would otherwise lock a max_tunnels=1 token out of its own subdomain.
func TestReconnectTakesOverOwnTunnelWithinLimit(t *testing.T) {
	s := newStack(t, func(c *config.Config) { c.Tunnel.MaxTunnelsPerToken = 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	first, err := dialAgent(t, ctx, s.gatewayWS, s.token, "sticky", textHandler("first"))
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}

	if resp := s.do(t, http.MethodGet, "sticky", "/", nil); readAll(t, resp.Body) != "first" {
		resp.Body.Close()
		t.Fatal("first agent did not serve")
	} else {
		resp.Body.Close()
	}

	// Reconnect while the first tunnel's row is still present.
	second, err := dialAgent(t, ctx, s.gatewayWS, s.token, "sticky", textHandler("second"))
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer second.close()

	// The displaced agent is told it was replaced and its read loop ends.
	select {
	case <-first.done:
	case <-time.After(3 * time.Second):
		t.Fatal("displaced agent was not shut down")
	}

	resp := s.do(t, http.MethodGet, "sticky", "/", nil)
	defer resp.Body.Close()
	if got := readAll(t, resp.Body); got != "second" {
		t.Fatalf("body = %q, want %q from the reconnected agent", got, "second")
	}
}

func TestTunnelLimitIsEnforcedAcrossSubdomains(t *testing.T) {
	s := newStack(t, func(c *config.Config) { c.Tunnel.MaxTunnelsPerToken = 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The token's stored MaxTunnels was set from the original config, so
	// override it to exercise the limit path.
	if err := s.store.Tokens().Create(ctx, &core.Token{
		ID: "limited", Name: "limited", TokenHash: auth.HashToken("rift_limitedtoken"), MaxTunnels: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	first, err := dialAgent(t, ctx, s.gatewayWS, "rift_limitedtoken", "one", nopHandler())
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	defer first.close()

	_, err = dialAgent(t, ctx, s.gatewayWS, "rift_limitedtoken", "two", nopHandler())
	var rej *handshakeRejection
	if !asRejection(err, &rej) {
		t.Fatalf("got %v, want a handshake rejection", err)
	}
	if rej.code != "tunnel_limit" {
		t.Fatalf("code = %q, want tunnel_limit", rej.code)
	}
}

func TestGeneratedSubdomainWhenNoneRequested(t *testing.T) {
	s := newStack(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a, err := dialAgent(t, ctx, s.gatewayWS, s.token, "", textHandler("generated"))
	if err != nil {
		t.Fatalf("dial agent: %v", err)
	}
	defer a.close()

	if err := s.cfg.SubdomainRules.Validate(a.hello.Subdomain); err != nil {
		t.Fatalf("generated subdomain %q is not valid: %v", a.hello.Subdomain, err)
	}

	resp := s.do(t, http.MethodGet, a.hello.Subdomain, "/", nil)
	defer resp.Body.Close()
	if got := readAll(t, resp.Body); got != "generated" {
		t.Fatalf("body = %q", got)
	}
}

// Caddy asks before issuing a certificate. Approving an unknown host would
// make the server an open certificate-issuance relay.
func TestTLSAskAuthorizesOnlyLiveOrReservedSubdomains(t *testing.T) {
	s := newStack(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a, err := dialAgent(t, ctx, s.gatewayWS, s.token, "live", nopHandler())
	if err != nil {
		t.Fatalf("dial agent: %v", err)
	}
	defer a.close()

	if err := s.store.Reservations().Create(ctx, &core.Reservation{
		Subdomain: "held", TokenID: s.tokenID, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		domain string
		want   int
	}{
		{"live." + testBaseDomain, http.StatusOK},
		{"held." + testBaseDomain, http.StatusOK},
		{"absent." + testBaseDomain, http.StatusNotFound},
		{"attacker.example.com", http.StatusForbidden},
		{"", http.StatusBadRequest},
		// The gateway's own hostname is not a tunnel, but agents dial it over
		// TLS. Refusing it leaves the gateway with no certificate at all.
		{"gateway." + testBaseDomain, http.StatusOK},
		{"GATEWAY." + strings.ToUpper(testBaseDomain), http.StatusOK},
		// A wildcard certificate does not cover the apex, so it needs its own.
		{testBaseDomain, http.StatusOK},
		// A sibling of the base domain is still somebody else's.
		{"evil" + testBaseDomain, http.StatusForbidden},
	}
	for _, tc := range cases {
		resp, err := s.client.Get(s.ingressURL + config.RouteTLSAsk + "?" + config.QueryParamDomain + "=" + tc.domain)
		if err != nil {
			t.Fatalf("tls-ask %q: %v", tc.domain, err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("tls-ask %q = %d, want %d", tc.domain, resp.StatusCode, tc.want)
		}
	}
}

// Revoking a token must terminate the tunnels it already opened. Blocking
// only new connections would leave a compromised token serving traffic until
// its agent happened to disconnect.
func TestRevokingATokenClosesItsLiveTunnels(t *testing.T) {
	s := newStack(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a, err := dialAgent(t, ctx, s.gatewayWS, s.token, "doomed", textHandler("alive"))
	if err != nil {
		t.Fatalf("dial agent: %v", err)
	}
	defer a.close()

	resp := s.do(t, http.MethodGet, "doomed", "/", nil)
	body := readAll(t, resp.Body)
	resp.Body.Close()
	if body != "alive" {
		t.Fatalf("tunnel did not serve before revocation: %q", body)
	}

	if err := s.store.Tokens().Revoke(ctx, s.tokenID, time.Now()); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	select {
	case <-a.done:
	case <-time.After(5 * time.Second):
		t.Fatal("tunnel kept serving after its token was revoked")
	}
	if got := a.shutdownReason(); got != tunnelproto.ShutdownTokenRevoked {
		t.Fatalf("shutdown reason = %q, want %q", got, tunnelproto.ShutdownTokenRevoked)
	}

	// Routing must drop the tunnel too, not just close the socket.
	deadline := time.Now().Add(3 * time.Second)
	for {
		r := s.do(t, http.MethodGet, "doomed", "/", nil)
		code := r.StatusCode
		r.Body.Close()
		if code == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("subdomain still routable after revocation (status %d)", code)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// An expired token is as dead as a revoked one.
func TestExpiredTokenClosesItsLiveTunnels(t *testing.T) {
	s := newStack(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	expiring := core.MustNewID(time.Now())
	const secret = "rift_expiringtokensecretvalue00000"
	future := time.Now().Add(700 * time.Millisecond)
	if err := s.store.Tokens().Create(ctx, &core.Token{
		ID: expiring, Name: "expiring", TokenHash: auth.HashToken(secret),
		MaxTunnels: 2, CreatedAt: time.Now(), ExpiresAt: &future,
	}); err != nil {
		t.Fatal(err)
	}

	a, err := dialAgent(t, ctx, s.gatewayWS, secret, "ttl", textHandler("alive"))
	if err != nil {
		t.Fatalf("dial agent: %v", err)
	}
	defer a.close()

	select {
	case <-a.done:
	case <-time.After(5 * time.Second):
		t.Fatal("tunnel kept serving after its token expired")
	}
	if got := a.shutdownReason(); got != tunnelproto.ShutdownTokenRevoked {
		t.Fatalf("shutdown reason = %q, want %q", got, tunnelproto.ShutdownTokenRevoked)
	}
}

// A store failure must not disconnect every live tunnel at once.
func TestTokenRevalidationToleratesStoreFailure(t *testing.T) {
	s := newStack(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a, err := dialAgent(t, ctx, s.gatewayWS, s.token, "resilient", textHandler("alive"))
	if err != nil {
		t.Fatalf("dial agent: %v", err)
	}
	defer a.close()

	s.tokens.failFindByID(errors.New("database is on fire"))

	// Several revalidation ticks must pass without the tunnel dying.
	select {
	case <-a.done:
		t.Fatal("a transient store failure tore down a healthy tunnel")
	case <-time.After(500 * time.Millisecond):
	}

	resp := s.do(t, http.MethodGet, "resilient", "/", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 while the store is failing", resp.StatusCode)
	}
}

// Liveness must not depend on the database; readiness must. Conflating them
// makes an orchestrator kill a healthy process during a database blip.
func TestHealthAndReadinessDifferOnDependencyFailure(t *testing.T) {
	s := newStack(t, nil)

	resp, err := s.client.Get(s.ingressURL + config.RouteHealth)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", resp.StatusCode)
	}

	// No check installed: ready by default.
	resp, err = s.client.Get(s.ingressURL + config.RouteReady)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz with no check = %d, want 200", resp.StatusCode)
	}

	s.ingress.SetReadyCheck(func(context.Context) error {
		return errors.New("database is on fire")
	})

	resp, err = s.client.Get(s.ingressURL + config.RouteReady)
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz with a failing dependency = %d, want 503", resp.StatusCode)
	}
	// The probe must not describe internal topology to whoever can reach it.
	if strings.Contains(body, "fire") {
		t.Fatalf("readiness body leaked the underlying error: %q", body)
	}

	// Liveness stays green: the process is fine, its dependency is not.
	resp, err = s.client.Get(s.ingressURL + config.RouteHealth)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz during a dependency failure = %d, want 200", resp.StatusCode)
	}
}

// The internal peer route must be closed when this node is not clustered.
func TestInternalProxyIsClosedWithoutRedis(t *testing.T) {
	s := newStack(t, nil)

	resp, err := s.client.Get(s.ingressURL + config.RouteInternalProxy)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when redis is disabled", resp.StatusCode)
	}
}

// A WebSocket (or any Upgrade) request must switch protocols end to end and
// then carry raw bytes both ways through the tunnel.
func TestWebSocketUpgradeIsProxied(t *testing.T) {
	s := newStack(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The HTTP handler must never run for an upgrade; the agent echoes raw bytes.
	a, err := dialAgent(t, ctx, s.gatewayWS, s.token, "wsapp", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Errorf("upgrade request must not reach the HTTP handler")
	}))
	if err != nil {
		t.Fatalf("dial agent: %v", err)
	}
	defer a.close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(s.ingressURL, "http://"))
	if err != nil {
		t.Fatalf("dial ingress: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	req := "GET /socket HTTP/1.1\r\n" +
		"Host: " + core.Hostname("wsapp", testBaseDomain) + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("status line = %q, want 101 Switching Protocols", statusLine)
	}

	// The 101 must carry a valid handshake, then the header block ends.
	sawAccept := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		if strings.HasPrefix(strings.ToLower(line), "sec-websocket-accept:") {
			sawAccept = true
		}
		if line == "\r\n" {
			break
		}
	}
	if !sawAccept {
		t.Fatal("101 response missing Sec-WebSocket-Accept")
	}

	// The connection is now a raw pipe: send bytes, expect the echo.
	for _, msg := range []string{"first-frame", "second-frame"} {
		if _, err := conn.Write([]byte(msg)); err != nil {
			t.Fatalf("write %q: %v", msg, err)
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(br, buf); err != nil {
			t.Fatalf("read echo of %q: %v", msg, err)
		}
		if string(buf) != msg {
			t.Fatalf("echo = %q, want %q", string(buf), msg)
		}
	}
}

// When the local service does not switch protocols, the gateway must relay its
// ordinary response rather than hijack the connection into a broken upgrade.
func TestDeclinedUpgradeRelaysNormalResponse(t *testing.T) {
	s := newStack(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// This agent answers an upgrade request with a plain 426 + body, standing
	// in for a service reached at a path that does not speak WebSocket.
	a, err := dialAgent(t, ctx, s.gatewayWS, s.token, "declineapp", nil,
		func(a *testAgent) { a.declineUpgrade = true })
	if err != nil {
		t.Fatalf("dial agent: %v", err)
	}
	defer a.close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(s.ingressURL, "http://"))
	if err != nil {
		t.Fatalf("dial ingress: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	req := "GET /socket HTTP/1.1\r\n" +
		"Host: " + core.Hostname("declineapp", testBaseDomain) + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "no websocket here" {
		t.Fatalf("body = %q, want %q", body, "no websocket here")
	}
}

// A tcp tunnel must be reachable on the gateway-allocated port and carry raw
// bytes both ways.
func TestTCPTunnelPipesBytes(t *testing.T) {
	s := newStack(t, func(c *config.Config) {
		c.TCP.Enabled = true
		c.TCP.ListenHost = "127.0.0.1"
		c.TCP.AdvertiseHost = "127.0.0.1"
		c.TCP.PortMin = 21000
		c.TCP.PortMax = 21050
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A tcp tunnel needs no HTTP handler; the agent echoes the raw stream.
	a, err := dialAgentProto(t, ctx, s.gatewayWS, s.token, "tcpapp", "tcp", nil)
	if err != nil {
		t.Fatalf("dial tcp agent: %v", err)
	}
	defer a.close()

	if !strings.HasPrefix(a.hello.BindAddr, "127.0.0.1:") {
		t.Fatalf("hello_ok bind_addr = %q, want 127.0.0.1:<port>", a.hello.BindAddr)
	}

	conn, err := net.Dial("tcp", a.hello.BindAddr)
	if err != nil {
		t.Fatalf("dial tcp tunnel at %s: %v", a.hello.BindAddr, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	for _, msg := range []string{"tcp-hello", "and-again"} {
		if _, err := conn.Write([]byte(msg)); err != nil {
			t.Fatalf("write %q: %v", msg, err)
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("read echo of %q: %v", msg, err)
		}
		if string(buf) != msg {
			t.Fatalf("echo = %q, want %q", string(buf), msg)
		}
	}
}

// A tcp handshake must be refused when tcp tunnels are disabled.
func TestTCPRejectedWhenDisabled(t *testing.T) {
	s := newStack(t, nil) // TCP.Enabled defaults to false
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := dialAgentProto(t, ctx, s.gatewayWS, s.token, "tcpapp", "tcp", nil)
	if err == nil {
		t.Fatal("expected tcp handshake to be rejected when disabled")
	}
	var rej *handshakeRejection
	if !asRejection(err, &rej) {
		t.Fatalf("error = %v, want a handshake rejection", err)
	}
	if rej.code != tunnelproto.ErrCodeUnsupportedProtocol {
		t.Fatalf("rejection code = %q, want %q", rej.code, tunnelproto.ErrCodeUnsupportedProtocol)
	}
}

// A tls tunnel must route by ClientHello SNI and pass the encrypted bytes
// through so a real TLS handshake completes against the agent's local service.
func TestTLSPassthroughCompletesHandshake(t *testing.T) {
	const host = "tlsapp." + testBaseDomain
	cert := selfSignedCert(t, host)

	// The agent's "local service": a TLS server that echoes plaintext.
	backend, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("tls backend listen: %v", err)
	}
	defer func() { _ = backend.Close() }()
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { defer func() { _ = c.Close() }(); _, _ = io.Copy(c, c) }(c)
		}
	}()

	s := newStack(t, func(c *config.Config) {
		c.TLSTunnel.Enabled = true
		c.TLSTunnel.ListenAddr = "127.0.0.1:0"
		c.TLSTunnel.AdvertisePort = 8443
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Serve passthrough on a listener we own, so we know its address.
	passLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("passthrough listen: %v", err)
	}
	go func() { _ = s.gw.ServeTLSTunnelsListener(ctx, passLn) }()

	// The agent proxies raw streams to the local TLS echo server.
	a, err := dialAgentProto(t, ctx, s.gatewayWS, s.token, "tlsapp", "tls", nil,
		func(a *testAgent) { a.rawBackend = backend.Addr().String() })
	if err != nil {
		t.Fatalf("dial tls agent: %v", err)
	}
	defer a.close()

	if want := host + ":8443"; a.hello.BindAddr != want {
		t.Fatalf("hello_ok bind_addr = %q, want %q", a.hello.BindAddr, want)
	}

	// A real TLS client dials the passthrough with the tunnel host as SNI.
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	client, err := tls.Dial("tcp", passLn.Addr().String(), &tls.Config{
		ServerName: host,
		RootCAs:    pool,
	})
	if err != nil {
		t.Fatalf("tls dial through passthrough: %v", err)
	}
	defer func() { _ = client.Close() }()
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))

	// The handshake already proved SNI routing reached the right agent and its
	// certificate; now prove application bytes flow both ways, end to end.
	if got := client.ConnectionState().PeerCertificates; len(got) == 0 || got[0].DNSNames[0] != host {
		t.Fatalf("peer certificate did not match the tls tunnel service")
	}
	msg := []byte("secure-hello")
	if _, err := client.Write(msg); err != nil {
		t.Fatalf("write over tls: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("read echo over tls: %v", err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("echo = %q, want %q", buf, msg)
	}
}

func TestTLSTunnelRejectedWhenDisabled(t *testing.T) {
	s := newStack(t, nil) // TLSTunnel.Enabled defaults to false
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := dialAgentProto(t, ctx, s.gatewayWS, s.token, "tlsapp", "tls", nil)
	var rej *handshakeRejection
	if !asRejection(err, &rej) {
		t.Fatalf("error = %v, want a handshake rejection", err)
	}
	if rej.code != tunnelproto.ErrCodeUnsupportedProtocol {
		t.Fatalf("rejection code = %q, want %q", rej.code, tunnelproto.ErrCodeUnsupportedProtocol)
	}
}

// selfSignedCert mints an in-memory certificate valid for host.
func selfSignedCert(t *testing.T, host string) tls.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}
}

// An agent offering a protocol version outside the supported range must be
// rejected with unsupported_version and an actionable message.
func TestUnsupportedProtocolVersionIsRejected(t *testing.T) {
	s := newStack(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, s.gatewayWS, &websocket.DialOptions{
		Subprotocols: []string{tunnelproto.Subprotocol},
	})
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	// A version far above the current one: the client is newer than the gateway.
	hello, err := tunnelproto.EncodeControl(tunnelproto.ControlHello, tunnelproto.Hello{
		ProtocolVersion: tunnelproto.Version + 99,
		Token:           s.token,
		Protocol:        "http",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageBinary, hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	frame, err := tunnelproto.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	env, err := tunnelproto.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != tunnelproto.ControlHelloError {
		t.Fatalf("got %s, want hello_error", env.Type)
	}
	var he tunnelproto.HelloError
	if err := tunnelproto.UnmarshalPayload(env, &he); err != nil {
		t.Fatal(err)
	}
	if he.Code != tunnelproto.ErrCodeUnsupportedVersion {
		t.Fatalf("code = %q, want %q", he.Code, tunnelproto.ErrCodeUnsupportedVersion)
	}
	if !strings.Contains(he.Message, "upgrade the gateway") {
		t.Fatalf("message is not actionable for a newer client: %q", he.Message)
	}
}

// hello_ok must advertise the gateway's protocol version so an agent can tell
// it is behind.
func TestHelloOKAdvertisesProtocolVersion(t *testing.T) {
	s := newStack(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a, err := dialAgent(t, ctx, s.gatewayWS, s.token, "vers", nopHandler())
	if err != nil {
		t.Fatalf("dial agent: %v", err)
	}
	defer a.close()

	if a.hello.ProtocolVersion != tunnelproto.Version {
		t.Fatalf("hello_ok protocol_version = %d, want %d", a.hello.ProtocolVersion, tunnelproto.Version)
	}
}

func nopHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
}

func textHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
}

func asRejection(err error, target **handshakeRejection) bool {
	if err == nil {
		return false
	}
	rej, ok := err.(*handshakeRejection)
	if !ok {
		return false
	}
	*target = rej
	return true
}

// TestOversizedRequestBodyIsRejectedWith413 pins the public status for a body
// that exceeds RIFT_MAX_REQUEST_BODY_BYTES. The cap is enforced by a
// MaxBytesReader read inside the goroutine pumping the body to the agent, so
// the failure reaches the ingress as a read error, not as an agent reset --
// which is why it once surfaced as a bare 502.
func TestOversizedRequestBodyIsRejectedWith413(t *testing.T) {
	const limit = 64 << 10

	s := newStack(t, func(c *config.Config) {
		c.Tunnel.MaxRequestBodyBytes = limit
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Drain the body so the agent behaves like a real upstream: it must not
	// answer early, or the response head would win the race against the cap.
	a, err := dialAgent(t, ctx, s.gatewayWS, s.token, "bigbody", http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			n, _ := io.Copy(io.Discard, r.Body)
			fmt.Fprintf(w, "read %d", n)
		}))
	if err != nil {
		t.Fatalf("dial agent: %v", err)
	}
	defer a.close()

	t.Run("under the cap round-trips", func(t *testing.T) {
		body := strings.Repeat("a", limit-1)
		resp := s.do(t, http.MethodPost, "bigbody", "/upload", strings.NewReader(body))
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
		if got, want := readAll(t, resp.Body), fmt.Sprintf("read %d", limit-1); got != want {
			t.Fatalf("body = %q, want %q", got, want)
		}
	})

	t.Run("over the cap is 413 payload_too_large", func(t *testing.T) {
		body := strings.Repeat("a", limit+(1<<10))
		req, err := http.NewRequest(http.MethodPost, s.ingressURL+"/upload", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = core.Hostname("bigbody", testBaseDomain)
		// Ask for JSON so the error code, not just the status, is assertable.
		req.Header.Set("Accept", "application/json")

		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want %d (body: %s)",
				resp.StatusCode, http.StatusRequestEntityTooLarge, readAll(t, resp.Body))
		}
		if got := readAll(t, resp.Body); !strings.Contains(got, "payload_too_large") {
			t.Fatalf("body = %q, want it to contain %q", got, "payload_too_large")
		}
	})
}
