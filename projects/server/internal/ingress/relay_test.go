package ingress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anomalysh/rift/projects/server/internal/config"
	"github.com/anomalysh/rift/projects/server/internal/core"
	"github.com/anomalysh/rift/projects/server/internal/store/memory"
)

// remoteRegistry attaches nothing locally and places every subdomain on one
// peer node, so a public request takes the forwardToPeer path.
type remoteRegistry struct{ nodeURL string }

func (remoteRegistry) Register(context.Context, core.Session) (core.Session, error) { return nil, nil }
func (remoteRegistry) Unregister(context.Context, core.Session) error               { return nil }
func (remoteRegistry) Lookup(context.Context, string) (core.Session, bool)          { return nil, false }
func (r remoteRegistry) LocatePeer(context.Context, string) (string, bool, error) {
	return r.nodeURL, true, nil
}
func (remoteRegistry) InvalidatePeer(context.Context, string, string) error { return nil }
func (remoteRegistry) Close() error                                         { return nil }

// A peer node's response reaches the public client intact: status, every
// header value, and the streamed body.
func TestPeerForwardRelaysResponse(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != config.RouteInternalProxy ||
			r.Header.Get(config.HeaderRiftPeerToken) != secret ||
			r.Header.Get(config.HeaderRiftSubdomain) != "app" ||
			r.Header.Get(config.HeaderRiftForwardedURI) != "/page?q=1" {
			http.Error(w, "unexpected forward", http.StatusTeapot)
			return
		}
		w.Header().Add("X-Upstream", "a")
		w.Header().Add("X-Upstream", "b")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "from the peer")
	}))
	defer peer.Close()

	store := memory.New()
	cfg := &config.Config{
		Redis:   config.Redis{Enabled: true},
		Cluster: config.Cluster{PeerSecret: secret},
		Tunnel:  config.Tunnel{BaseDomain: "rift.test", PublicScheme: "https", RequestTimeout: 5 * time.Second},
	}
	i := New(cfg, discardLogger(), remoteRegistry{nodeURL: peer.URL}, store.Tunnels(), store.Reservations(), store.Domains())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://app.rift.test/page?q=1", nil)
	r.Host = "app.rift.test"
	r.RemoteAddr = "203.0.113.5:1234"
	i.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", w.Code, w.Body.String())
	}
	if got := w.Header().Values("X-Upstream"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("X-Upstream = %v, want [a b]", got)
	}
	if got := w.Body.String(); got != "from the peer" {
		t.Fatalf("body = %q", got)
	}
}
