// Package e2e wires the real gateway, ingress and registry together with a
// real WebSocket agent and proves a public HTTP request reaches the agent's
// handler and streams back.
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/siliconcolony/tunl/server/internal/auth"
	"github.com/siliconcolony/tunl/server/internal/config"
	"github.com/siliconcolony/tunl/server/internal/core"
	"github.com/siliconcolony/tunl/server/internal/gateway"
	"github.com/siliconcolony/tunl/server/internal/ingress"
	"github.com/siliconcolony/tunl/server/internal/registry"
	"github.com/siliconcolony/tunl/server/internal/store/memory"
)

const testBaseDomain = "tunl.example.test"

type stack struct {
	cfg        *config.Config
	store      *memory.Store
	gatewayWS  string
	ingressURL string
	client     *http.Client
	token      string
	tokenID    string
}

func unmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func newStack(t *testing.T, tune func(*config.Config)) *stack {
	t.Helper()

	rules, err := core.NewSubdomainRules(3, 63, `^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`,
		config.DefaultSubdomainBlocklist, 10, config.DefaultSubdomainGenAlphabet)
	if err != nil {
		t.Fatalf("subdomain rules: %v", err)
	}

	cfg := &config.Config{
		Env:    config.EnvDevelopment,
		NodeID: "test-node",
		Gateway: config.Gateway{
			Path:             "/tunnel",
			HandshakeTimeout: 5 * time.Second,
			WriteTimeout:     5 * time.Second,
		},
		Tunnel: config.Tunnel{
			BaseDomain:          testBaseDomain,
			PublicScheme:        config.SchemeHTTPS,
			HeartbeatInterval:   50 * time.Millisecond,
			HeartbeatTimeout:    2 * time.Second,
			RequestTimeout:      5 * time.Second,
			MaxRequestBodyBytes: 1 << 20,
			MaxTunnelsPerToken:  5,
			StreamBufferSize:    8,
		},
		SubdomainRules: rules,
	}
	if tune != nil {
		tune(cfg)
	}

	// TUNL_TEST_DEBUG=1 surfaces server logs when a test is being diagnosed.
	var logSink io.Writer = io.Discard
	if os.Getenv("TUNL_TEST_DEBUG") != "" {
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

	reg := registry.NewLocal()
	gw := gateway.New(cfg, logger, store.Tokens(), store.Reservations(), store.Tunnels(), reg)
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

	_, err := dialAgent(t, ctx, s.gatewayWS, "tunl_notarealtoken", "demo", nopHandler())
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
		ID: "limited", Name: "limited", TokenHash: auth.HashToken("tunl_limitedtoken"), MaxTunnels: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	first, err := dialAgent(t, ctx, s.gatewayWS, "tunl_limitedtoken", "one", nopHandler())
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	defer first.close()

	_, err = dialAgent(t, ctx, s.gatewayWS, "tunl_limitedtoken", "two", nopHandler())
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
