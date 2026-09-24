package ingress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anomalysh/rift/projects/server/internal/config"
	"github.com/anomalysh/rift/projects/server/internal/core"
	"github.com/anomalysh/rift/projects/server/internal/registry"
	"github.com/anomalysh/rift/projects/server/internal/store/memory"
)

// recordingSession answers 200 and remembers the last request it was sent, so
// a test can inspect exactly what the agent would receive.
type recordingSession struct {
	tunnel core.Tunnel
	seen   **http.Request
}

func (s *recordingSession) Tunnel() core.Tunnel { return s.tunnel }
func (s *recordingSession) Close(string) error  { return nil }
func (s *recordingSession) RoundTrip(r *http.Request) (*http.Response, error) {
	*s.seen = r
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
}

func newForwardedIngress(trusted []string) *Ingress {
	cfg := &config.Config{}
	cfg.Tunnel.PublicScheme = "https"
	cfg.Ingress.TrustedProxyIPs = trusted
	return &Ingress{cfg: cfg, trusted: parseTrusted(trusted)}
}

// The local service behind a tunnel must learn who actually connected. On the
// direct (locally attached) path the gateway is the only proxy, so it fills the
// forwarded headers from the socket.
func TestAnnotateForwarded_DirectClient(t *testing.T) {
	i := newForwardedIngress(nil)
	r, _ := http.NewRequest(http.MethodGet, "http://demo.rift.test/path", nil)
	r.RemoteAddr = "203.0.113.5:44321"
	r.Host = "demo.rift.test"

	i.annotateForwarded(r)

	if got := r.Header.Get(config.HeaderForwardedFor); got != "203.0.113.5" {
		t.Errorf("X-Forwarded-For = %q, want 203.0.113.5", got)
	}
	if got := r.Header.Get(config.HeaderRealIP); got != "203.0.113.5" {
		t.Errorf("X-Real-IP = %q, want 203.0.113.5", got)
	}
	if got := r.Header.Get(config.HeaderForwardedProto); got != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want https", got)
	}
	if got := r.Header.Get(config.HeaderForwardedHost); got != "demo.rift.test" {
		t.Errorf("X-Forwarded-Host = %q, want demo.rift.test", got)
	}
}

// Behind a trusted reverse proxy (Caddy) the real client is in the incoming
// X-Forwarded-For; the gateway keeps that chain, appends the proxy's address
// per convention, and resolves X-Real-IP to the real client, not the proxy.
func TestAnnotateForwarded_TrustedUpstream(t *testing.T) {
	i := newForwardedIngress([]string{"10.0.0.2"})
	r, _ := http.NewRequest(http.MethodGet, "http://demo.rift.test/path", nil)
	r.RemoteAddr = "10.0.0.2:5555" // Caddy, trusted
	r.Host = "demo.rift.test"
	r.Header.Set(config.HeaderForwardedFor, "198.51.100.7")
	r.Header.Set(config.HeaderForwardedProto, "https")

	i.annotateForwarded(r)

	if got := r.Header.Get(config.HeaderForwardedFor); got != "198.51.100.7, 10.0.0.2" {
		t.Errorf("X-Forwarded-For = %q, want the chain with the proxy appended", got)
	}
	if got := r.Header.Get(config.HeaderRealIP); got != "198.51.100.7" {
		t.Errorf("X-Real-IP = %q, want 198.51.100.7", got)
	}
}

// An untrusted immediate peer's X-Forwarded-For must not be believed or passed
// on: the socket address replaces the client-written chain.
func TestAnnotateForwarded_UntrustedSpoof(t *testing.T) {
	i := newForwardedIngress(nil) // trust nobody
	r, _ := http.NewRequest(http.MethodGet, "http://demo.rift.test/path", nil)
	r.RemoteAddr = "192.0.2.9:40000"
	r.Host = "demo.rift.test"
	r.Header.Set(config.HeaderForwardedFor, "1.2.3.4") // spoofed by client

	i.annotateForwarded(r)

	if got := r.Header.Get(config.HeaderRealIP); got != "192.0.2.9" {
		t.Errorf("X-Real-IP = %q, want 192.0.2.9 (socket, not spoofed header)", got)
	}
	if got := r.Header.Values(config.HeaderForwardedFor); len(got) != 1 || got[0] != "192.0.2.9" {
		t.Errorf("X-Forwarded-For = %q, want the spoofed chain replaced by 192.0.2.9", got)
	}
}

// clientIP walks X-Forwarded-For right to left: a trusted proxy APPENDS the
// address it saw, so the left-most entry is whatever the client wrote.
func TestClientIPWalksForwardedForFromTheRight(t *testing.T) {
	i := newForwardedIngress([]string{"10.0.0.0/8"})
	cases := []struct {
		name   string
		remote string
		xff    []string
		realIP string
		want   string
	}{
		{"untrusted socket ignores headers", "203.0.113.5:1", []string{"1.2.3.4"}, "5.6.7.8", "203.0.113.5"},
		{"client spoofs a left-most entry", "10.0.0.2:1", []string{"1.2.3.4, 198.51.100.7"}, "", "198.51.100.7"},
		{"trusted hops are skipped", "10.0.0.2:1", []string{"198.51.100.7, 10.0.0.9, 10.0.0.3"}, "", "198.51.100.7"},
		{"repeated header lines form one chain", "10.0.0.2:1", []string{"1.2.3.4", "198.51.100.7"}, "", "198.51.100.7"},
		{"host:port entries are tolerated", "10.0.0.2:1", []string{"198.51.100.7:5555"}, "", "198.51.100.7"},
		{"bracketed v6 with port", "10.0.0.2:1", []string{"[2001:db8::1]:443"}, "", "2001:db8::1"},
		{"garbage stops at the last trusted hop", "10.0.0.2:1", []string{"198.51.100.7, bogus, 10.0.0.3"}, "", "10.0.0.3"},
		{"all hops trusted", "10.0.0.2:1", []string{"10.0.0.7"}, "", "10.0.0.7"},
		{"X-Real-IP only without X-Forwarded-For", "10.0.0.2:1", nil, "198.51.100.8", "198.51.100.8"},
		{"X-Real-IP ignored when X-Forwarded-For present", "10.0.0.2:1", []string{"198.51.100.7"}, "1.1.1.1", "198.51.100.7"},
		{"invalid X-Real-IP falls back to socket", "10.0.0.2:1", nil, "not-an-ip", "10.0.0.2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://demo.rift.test/", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add(config.HeaderForwardedFor, v)
			}
			if tc.realIP != "" {
				r.Header.Set(config.HeaderRealIP, tc.realIP)
			}
			if got := i.clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// A peer-forwarded request must be judged on the visitor's address, which the
// edge stamps into HeaderRiftClientIP, never on the forwarding node's socket
// address or on a client-supplied X-Forwarded-For preserved across the hop.
func TestInternalProxyUsesEdgeStampedClientIP(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	store := memory.New()
	reg := registry.NewLocal()
	cfg := &config.Config{
		Redis:   config.Redis{Enabled: true},
		Cluster: config.Cluster{PeerSecret: secret},
		Ingress: config.Ingress{TrustedProxyIPs: []string{"10.0.0.0/8"}},
		Tunnel:  config.Tunnel{BaseDomain: "rift.test", PublicScheme: "https", RequestTimeout: 5 * time.Second},
	}
	i := New(cfg, discardLogger(), reg, store.Tunnels(), store.Reservations(), store.Domains())

	tn := core.Tunnel{ID: "t1", Subdomain: "app", TokenID: "tok", Policy: core.Policy{
		AllowIPs: []string{"198.51.100.0/24"},
	}}
	if _, err := reg.Register(context.Background(), &echoSession{tunnel: tn}); err != nil {
		t.Fatal(err)
	}

	hop := func(stamped, xff string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://app.rift.test"+config.RouteInternalProxy, nil)
		r.Host = "app.rift.test"
		r.RemoteAddr = "10.0.0.5:3333" // the forwarding node, a trusted address
		r.Header.Set(config.HeaderRiftPeerToken, secret)
		r.Header.Set(config.HeaderRiftSubdomain, "app")
		r.Header.Set(config.HeaderRiftForwardedURI, "/x")
		if stamped != "" {
			r.Header.Set(config.HeaderRiftClientIP, stamped)
		}
		if xff != "" {
			r.Header.Set(config.HeaderForwardedFor, xff)
		}
		w := httptest.NewRecorder()
		i.Handler().ServeHTTP(w, r)
		return w
	}

	if w := hop("198.51.100.7", ""); w.Code != http.StatusOK {
		t.Fatalf("allowed visitor via peer = %d %q, want 200", w.Code, w.Body.String())
	}
	// The spoofed chain names an allowed address, but the edge resolved the
	// visitor to 203.0.113.9: the stamp wins.
	if w := hop("203.0.113.9", "198.51.100.7"); w.Code != http.StatusForbidden {
		t.Fatalf("blocked visitor with spoofed XFF via peer = %d, want 403", w.Code)
	}
}

// A visitor cannot plant the peer-hop client header: the edge strips it before
// resolving the client, so it never reaches the policy or the agent.
func TestPublicEdgeStripsClientIPHeader(t *testing.T) {
	store := memory.New()
	reg := registry.NewLocal()
	cfg := &config.Config{Tunnel: config.Tunnel{BaseDomain: "rift.test", PublicScheme: "https", RequestTimeout: 5 * time.Second}}
	i := New(cfg, discardLogger(), reg, store.Tunnels(), store.Reservations(), store.Domains())

	var seen *http.Request
	sess := &recordingSession{tunnel: core.Tunnel{ID: "t", Subdomain: "app", TokenID: "tok", Policy: core.Policy{
		AllowIPs: []string{"198.51.100.0/24"},
	}}, seen: &seen}
	if _, err := reg.Register(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "http://app.rift.test/", nil)
	r.Host = "app.rift.test"
	r.RemoteAddr = "203.0.113.9:1"
	r.Header.Set(config.HeaderRiftClientIP, "198.51.100.7")
	w := httptest.NewRecorder()
	i.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("planted client header = %d, want 403 (judged on the socket address)", w.Code)
	}

	sess.tunnel.Policy = core.Policy{}
	sess.tunnel.ID = "t2" // a fresh policy-cache entry
	r = httptest.NewRequest(http.MethodGet, "http://app.rift.test/", nil)
	r.Host = "app.rift.test"
	r.RemoteAddr = "203.0.113.9:1"
	r.Header.Set(config.HeaderRiftClientIP, "198.51.100.7")
	i.Handler().ServeHTTP(httptest.NewRecorder(), r)
	if seen == nil {
		t.Fatal("request never reached the tunnel")
	}
	if v := seen.Header.Get(config.HeaderRiftClientIP); v != "" {
		t.Fatalf("agent received a client-planted %s: %q", config.HeaderRiftClientIP, v)
	}
	if !strings.HasPrefix(seen.RemoteAddr, "203.0.113.9") {
		t.Fatalf("agent RemoteAddr = %q, want the socket address", seen.RemoteAddr)
	}
}

func TestValidPeerURL(t *testing.T) {
	for url, want := range map[string]bool{
		"http://10.0.0.4:8080":    true,
		"https://node-2.internal": true,
		"http://10.0.0.4:8080/":   true,
		"ftp://10.0.0.4":          false,
		"file:///etc/passwd":      false,
		"10.0.0.4:8080":           false,
		"http://user:pw@10.0.0.4": false,
		"http://10.0.0.4?x=1":     false,
		"http://":                 false,
		"javascript:alert(1)":     false,
	} {
		if got := validPeerURL(url); got != want {
			t.Errorf("validPeerURL(%q) = %v, want %v", url, got, want)
		}
	}
}
