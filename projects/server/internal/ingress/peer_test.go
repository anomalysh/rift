package ingress

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anomalysh/rift/projects/server/internal/config"
	"github.com/anomalysh/rift/projects/server/internal/registry"
	"github.com/anomalysh/rift/projects/server/internal/store/memory"
)

// A non-idempotent method must never be silently retried against another node:
// repeating a POST could submit an order twice.
func TestCanRetryForward(t *testing.T) {
	cases := []struct {
		method string
		body   bool
		want   bool
	}{
		{http.MethodGet, false, true},
		{http.MethodHead, false, true},
		{http.MethodOptions, false, true},
		{http.MethodPost, false, false},
		{http.MethodPut, false, false},
		{http.MethodDelete, false, false},
		{http.MethodPatch, false, false},
		// A body cannot be replayed even for an otherwise-idempotent method.
		{http.MethodGet, true, false},
	}
	for _, tc := range cases {
		var body *http.Request
		if tc.body {
			r, _ := http.NewRequest(tc.method, "http://x/", strings.NewReader("payload"))
			body = r
		} else {
			r, _ := http.NewRequest(tc.method, "http://x/", nil)
			body = r
		}
		if got := canRetryForward(body); got != tc.want {
			t.Errorf("canRetryForward(%s body=%v) = %v, want %v", tc.method, tc.body, got, tc.want)
		}
	}
}

func peerTestIngress(t *testing.T) *Ingress {
	t.Helper()
	store := memory.New()
	cfg := &config.Config{
		Redis:   config.Redis{Enabled: true},
		Cluster: config.Cluster{PeerSecret: "0123456789abcdef0123456789abcdef"},
		Tunnel:  config.Tunnel{BaseDomain: "rift.test", PublicScheme: "https", RequestTimeout: 5 * time.Second},
	}
	return New(cfg, discardLogger(), registry.NewLocal(), store.Tunnels(), store.Reservations(), store.Domains())
}

// The peer hop stamps the edge-resolved client address, so the receiving node
// can judge the visitor rather than the forwarding node.
func TestPeerForwardStampsClientIP(t *testing.T) {
	i := peerTestIngress(t)
	var got http.Header
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}))
	defer peer.Close()

	r := httptest.NewRequest(http.MethodGet, "http://app.rift.test/x", nil)
	r = withClientIP(r, "198.51.100.7")
	resp, err := i.doPeerForward(r, peer.URL, "app")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if v := got.Get(config.HeaderRiftClientIP); v != "198.51.100.7" {
		t.Fatalf("%s = %q, want 198.51.100.7", config.HeaderRiftClientIP, v)
	}
}

// A lease naming something other than an http(s) URL is never dialled, so the
// peer secret cannot be sent to a corrupted Redis entry's choice of target.
func TestPeerForwardRefusesNonHTTPLease(t *testing.T) {
	i := peerTestIngress(t)
	r := httptest.NewRequest(http.MethodGet, "http://app.rift.test/x", nil)
	if _, err := i.doPeerForward(r, "file:///etc/passwd", "app"); !errors.Is(err, errInvalidPeerURL) {
		t.Fatalf("err = %v, want errInvalidPeerURL", err)
	}
}

// Node-to-node traffic carries the peer secret; it must never be routed via an
// HTTP proxy inherited from the environment.
func TestPeerClientIgnoresEnvironmentProxy(t *testing.T) {
	i := peerTestIngress(t)
	tr, ok := i.peers.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("peer transport is %T, want *http.Transport", i.peers.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("peer transport consults a proxy; node-to-node hops must go direct")
	}
}
