package ingress

import (
	"net/http"
	"testing"

	"github.com/anomalysh/rift/server/internal/config"
)

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
// X-Forwarded-For; the gateway must preserve that chain and resolve X-Real-IP
// to the real client, not the proxy.
func TestAnnotateForwarded_TrustedUpstream(t *testing.T) {
	i := newForwardedIngress([]string{"10.0.0.2"})
	r, _ := http.NewRequest(http.MethodGet, "http://demo.rift.test/path", nil)
	r.RemoteAddr = "10.0.0.2:5555" // Caddy, trusted
	r.Host = "demo.rift.test"
	r.Header.Set(config.HeaderForwardedFor, "198.51.100.7")
	r.Header.Set(config.HeaderForwardedProto, "https")

	i.annotateForwarded(r)

	// The upstream chain is preserved, not appended to or replaced.
	if got := r.Header.Get(config.HeaderForwardedFor); got != "198.51.100.7" {
		t.Errorf("X-Forwarded-For = %q, want 198.51.100.7 (preserved)", got)
	}
	// X-Real-IP resolves to the real client because Caddy is trusted.
	if got := r.Header.Get(config.HeaderRealIP); got != "198.51.100.7" {
		t.Errorf("X-Real-IP = %q, want 198.51.100.7", got)
	}
}

// An untrusted immediate peer's X-Forwarded-For must not be believed for
// X-Real-IP: the socket address wins so a client cannot spoof its own IP.
func TestAnnotateForwarded_UntrustedSpoof(t *testing.T) {
	i := newForwardedIngress(nil) // trust nobody
	r, _ := http.NewRequest(http.MethodGet, "http://demo.rift.test/path", nil)
	r.RemoteAddr = "192.0.2.9:40000"
	r.Host = "demo.rift.test"
	r.Header.Set(config.HeaderForwardedFor, "1.2.3.4") // spoofed by client

	i.annotateForwarded(r)

	// The spoofed chain is left as-is (leftmost is still whatever was sent),
	// but X-Real-IP, the value we vouch for, is the real socket address.
	if got := r.Header.Get(config.HeaderRealIP); got != "192.0.2.9" {
		t.Errorf("X-Real-IP = %q, want 192.0.2.9 (socket, not spoofed header)", got)
	}
}
