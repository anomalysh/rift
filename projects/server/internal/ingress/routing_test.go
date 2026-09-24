package ingress

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anomalysh/rift/projects/server/internal/config"
)

// Internal routes answer only on internal names; on a tunnel host every path,
// including /healthz and /internal/tls-ask, belongs to the tunnel.
func TestInternalRoutesDoNotShadowTunnels(t *testing.T) {
	f := newDomainFixture(t)
	f.attach(t, "app", "tok")
	f.mapDomain(t, "app.acme.com", "app", "tok")

	serve := func(host, target string, hdr http.Header) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://placeholder"+target, nil)
		r.Host = host
		r.RemoteAddr = "203.0.113.5:1234"
		for k, vs := range hdr {
			for _, v := range vs {
				r.Header.Add(k, v)
			}
		}
		w := httptest.NewRecorder()
		f.i.Handler().ServeHTTP(w, r)
		return w
	}
	fromAgent := func(w *httptest.ResponseRecorder) bool {
		return strings.Contains(w.Body.String(), "served-by:")
	}
	ask := config.RouteTLSAsk + "?" + config.QueryParamDomain + "=app.rift.test"

	cases := []struct {
		name, host, target string
		wantAgent          bool
		wantCode           int
	}{
		// Internal names: what health checks, Caddy's ask URL and operators use.
		{"health on loopback IP", "127.0.0.1:8080", config.RouteHealth, false, http.StatusOK},
		{"ready on IPv6 loopback", "[::1]:8080", config.RouteReady, false, http.StatusOK},
		{"tls-ask on container name", "riftd:8080", ask, false, http.StatusOK},
		{"health on localhost", "localhost", config.RouteHealth, false, http.StatusOK},
		{"health on unregistered domain", "riftd.internal.example", config.RouteHealth, false, http.StatusOK},
		{"other path on internal name", "127.0.0.1:8080", "/", false, http.StatusNotFound},

		// Tunnel hosts: the app owns every path.
		{"tunnel serves its own /healthz", "app.rift.test", config.RouteHealth, true, http.StatusOK},
		{"tunnel serves its own /readyz", "app.rift.test", config.RouteReady, true, http.StatusOK},
		{"tls-ask unreachable via tunnel host", "app.rift.test", ask, true, http.StatusOK},
		{"custom domain serves its own /healthz", "app.acme.com", config.RouteHealth, true, http.StatusOK},
		{"internal proxy path is the app's without Redis", "app.rift.test", config.RouteInternalProxy, true, http.StatusOK},

		// The deployment's own public names are not tunnels and expose nothing.
		{"apex tls-ask", "rift.test", ask, false, http.StatusNotFound},
		{"gateway hostname readiness", "gateway.rift.test", config.RouteReady, false, http.StatusNotFound},
		{"unknown subdomain health", "nobody.rift.test", config.RouteHealth, false, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serve(tc.host, tc.target, nil)
			if w.Code != tc.wantCode || fromAgent(w) != tc.wantAgent {
				t.Fatalf("%s%s = %d %q; want %d, reached agent = %v",
					tc.host, tc.target, w.Code, w.Body.String(), tc.wantCode, tc.wantAgent)
			}
		})
	}

	// The agent receives the path the visitor asked for.
	if w := serve("app.rift.test", config.RouteHealth, nil); !strings.Contains(w.Body.String(), "path:"+config.RouteHealth) {
		t.Fatalf("agent saw %q, want path %s", w.Body.String(), config.RouteHealth)
	}
}

// With Redis on, a peer hop keeps the visitor's Host, so RouteInternalProxy
// carrying a peer token is dispatched to the internal proxy on any Host (and
// authenticated there). Without the token it is just the app's path.
func TestPeerHopDispatchOnTunnelHost(t *testing.T) {
	f := newDomainFixture(t)
	f.i.cfg.Redis.Enabled = true
	f.i.cfg.Cluster.PeerSecret = "0123456789abcdef0123456789abcdef"
	f.attach(t, "app", "tok")

	r := httptest.NewRequest(http.MethodGet, "http://app.rift.test"+config.RouteInternalProxy, nil)
	r.Host = "app.rift.test"
	r.Header.Set(config.HeaderRiftPeerToken, "wrong")
	r.Header.Set(config.HeaderRiftSubdomain, "app")
	w := httptest.NewRecorder()
	f.i.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("peer hop with a wrong token = %d, want 403", w.Code)
	}

	r = httptest.NewRequest(http.MethodGet, "http://app.rift.test"+config.RouteInternalProxy, nil)
	r.Host = "app.rift.test"
	w = httptest.NewRecorder()
	f.i.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "served-by:tok") {
		t.Fatalf("app path without a peer token = %d %q, want the agent", w.Code, w.Body.String())
	}
}
