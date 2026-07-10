package ingress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anomalysh/rift/projects/server/internal/config"
	"github.com/anomalysh/rift/projects/server/internal/core"
	"github.com/anomalysh/rift/projects/server/internal/store/memory"
)

func domainTestIngress(t *testing.T) (*Ingress, core.DomainStore) {
	t.Helper()
	store := memory.New()
	i := &Ingress{
		cfg: &config.Config{Tunnel: config.Tunnel{
			BaseDomain: "rift.test", PublicScheme: "https",
		}},
		logger:  discardLogger(),
		domains: store.Domains(),
	}
	return i, store.Domains()
}

func TestTLSAskAuthorizesRegisteredCustomDomain(t *testing.T) {
	i, domains := domainTestIngress(t)
	if err := domains.Upsert(context.Background(), core.CustomDomain{
		Domain: "app.acme.com", Subdomain: "abc", TokenID: "tok", CreatedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}

	// A registered custom domain is authorized for a certificate.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/internal/tls-ask?domain=app.acme.com", nil)
	i.handleTLSAsk(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("registered custom domain tls-ask = %d, want 200", w.Code)
	}

	// An unregistered foreign domain is refused (not an open issuance relay).
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/internal/tls-ask?domain=evil.example.com", nil)
	i.handleTLSAsk(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unregistered domain tls-ask = %d, want 403", w.Code)
	}
}

func TestSubdomainForCustomDomain(t *testing.T) {
	i, domains := domainTestIngress(t)
	if err := domains.Upsert(context.Background(), core.CustomDomain{
		Domain: "app.acme.com", Subdomain: "abc", TokenID: "tok", CreatedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}

	// The Host header (with a port, mixed case) resolves to the subdomain.
	r := httptest.NewRequest(http.MethodGet, "http://x/y", nil)
	r.Host = "App.Acme.com:443"
	if sub, ok := i.subdomainForCustomDomain(r); !ok || sub != "abc" {
		t.Fatalf("subdomainForCustomDomain = %q, %v; want abc, true", sub, ok)
	}

	// An unregistered host does not resolve.
	r.Host = "unknown.example.com"
	if _, ok := i.subdomainForCustomDomain(r); ok {
		t.Fatal("an unregistered host must not resolve")
	}
}
