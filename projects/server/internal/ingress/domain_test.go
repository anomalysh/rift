package ingress

import (
	"context"
	"io"
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

// echoSession answers every request with its own token ID, so a test can see
// which tunnel a request reached.
type echoSession struct{ tunnel core.Tunnel }

func (e *echoSession) Tunnel() core.Tunnel { return e.tunnel }
func (e *echoSession) Close(string) error  { return nil }
func (e *echoSession) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("served-by:" + e.tunnel.TokenID + " path:" + r.URL.Path)),
		Request:    r,
	}, nil
}

type domainFixture struct {
	i     *Ingress
	store *memory.Store
	reg   *registry.Local
}

func newDomainFixture(t *testing.T) *domainFixture {
	t.Helper()
	store := memory.New()
	reg := registry.NewLocal()
	cfg := &config.Config{
		Gateway: config.Gateway{Hostname: "gateway.rift.test"},
		Tunnel: config.Tunnel{
			BaseDomain: "rift.test", PublicScheme: "https", RequestTimeout: 5 * time.Second,
		},
	}
	i := New(cfg, discardLogger(), reg, store.Tunnels(), store.Reservations(), store.Domains())
	return &domainFixture{i: i, store: store, reg: reg}
}

// attach makes tokenID's tunnel live on sub, both in the store (the cluster
// authority) and in this node's registry.
func (f *domainFixture) attach(t *testing.T, sub, tokenID string) *echoSession {
	t.Helper()
	tn := core.Tunnel{ID: core.MustNewID(time.Now()), Subdomain: sub, TokenID: tokenID, Protocol: core.ProtocolHTTP}
	if err := f.store.Tunnels().Claim(context.Background(), &tn); err != nil {
		t.Fatal(err)
	}
	sess := &echoSession{tunnel: tn}
	if _, err := f.reg.Register(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	return sess
}

func (f *domainFixture) detach(t *testing.T, sess *echoSession) {
	t.Helper()
	_ = f.store.Tunnels().Release(context.Background(), sess.tunnel.ID)
	_ = f.reg.Unregister(context.Background(), sess)
}

func (f *domainFixture) mapDomain(t *testing.T, domain, sub, tokenID string) {
	t.Helper()
	if err := f.store.Domains().Upsert(context.Background(), core.CustomDomain{
		Domain: domain, Subdomain: sub, TokenID: tokenID, CreatedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *domainFixture) ask(domain string) int {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, config.RouteTLSAsk+"?"+config.QueryParamDomain+"="+domain, nil)
	f.i.handleTLSAsk(w, r)
	return w.Code
}

func (f *domainFixture) visit(host string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://placeholder/page", nil)
	r.Host = host
	r.RemoteAddr = "203.0.113.5:1234"
	f.i.Handler().ServeHTTP(w, r)
	return w
}

func TestTLSAskAuthorizesCustomDomainOnlyForItsOwner(t *testing.T) {
	f := newDomainFixture(t)
	f.mapDomain(t, "app.acme.com", "abc", "owner")

	// Mapped but nobody holds the subdomain: no certificate.
	if got := f.ask("app.acme.com"); got != http.StatusNotFound {
		t.Fatalf("custom domain with no tunnel tls-ask = %d, want 404", got)
	}

	// Another token holding the subdomain does not earn the owner's domain a
	// certificate.
	squatter := f.attach(t, "abc", "squatter")
	if got := f.ask("app.acme.com"); got != http.StatusNotFound {
		t.Fatalf("custom domain held by another token tls-ask = %d, want 404", got)
	}
	f.detach(t, squatter)

	// The owner's live tunnel does.
	owner := f.attach(t, "abc", "owner")
	if got := f.ask("app.acme.com"); got != http.StatusOK {
		t.Fatalf("custom domain with owner's tunnel tls-ask = %d, want 200", got)
	}
	f.detach(t, owner)

	// A reservation by the owner pre-authorizes, one by anyone else does not.
	for _, id := range []string{"owner", "someone"} {
		if err := f.store.Tokens().Create(context.Background(), &core.Token{ID: id, TokenHash: "hash-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.Reservations().Create(context.Background(), &core.Reservation{Subdomain: "abc", TokenID: "owner"}); err != nil {
		t.Fatal(err)
	}
	if got := f.ask("app.acme.com"); got != http.StatusOK {
		t.Fatalf("custom domain reserved by owner tls-ask = %d, want 200", got)
	}
	f.mapDomain(t, "other.acme.com", "xyz", "owner")
	if err := f.store.Reservations().Create(context.Background(), &core.Reservation{Subdomain: "xyz", TokenID: "someone"}); err != nil {
		t.Fatal(err)
	}
	if got := f.ask("other.acme.com"); got != http.StatusNotFound {
		t.Fatalf("custom domain reserved by another token tls-ask = %d, want 404", got)
	}

	// An unregistered foreign domain is refused (not an open issuance relay).
	if got := f.ask("evil.example.com"); got != http.StatusForbidden {
		t.Fatalf("unregistered domain tls-ask = %d, want 403", got)
	}
}

// Names under the base domain are never looked up as custom domains, even if a
// row for one exists (a legacy registration, or a hand-inserted one).
func TestTLSAskNeverConsultsCustomDomainsUnderBase(t *testing.T) {
	f := newDomainFixture(t)
	f.attach(t, "abc", "tok")
	f.mapDomain(t, "a.b.rift.test", "abc", "tok")

	if got := f.ask("a.b.rift.test"); got != http.StatusForbidden {
		t.Fatalf("multi-label name under the base tls-ask = %d, want 403", got)
	}
}

func TestCustomDomainRoutesOnlyToOwningToken(t *testing.T) {
	f := newDomainFixture(t)
	f.mapDomain(t, "app.acme.com", "abc", "owner")

	// Owner live: the domain reaches the owner's tunnel (Host with port and
	// mixed case normalizes).
	owner := f.attach(t, "abc", "owner")
	w := f.visit("App.Acme.com:443")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "served-by:owner") {
		t.Fatalf("owner's domain = %d %q, want 200 from owner", w.Code, w.Body.String())
	}
	f.detach(t, owner)

	// The owner leaves and another token claims the same subdomain: the
	// domain must not follow the label to the squatter.
	f.attach(t, "abc", "squatter")
	w = f.visit("app.acme.com")
	if w.Code != http.StatusNotFound {
		t.Fatalf("domain after squatter took the subdomain = %d %q, want 404", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "abc") {
		t.Fatalf("404 for a custom domain leaked the backing subdomain: %q", w.Body.String())
	}

	// The squatter's own subdomain URL still works.
	if w := f.visit("abc.rift.test"); w.Code != http.StatusOK {
		t.Fatalf("squatter's own subdomain = %d, want 200", w.Code)
	}
}

// The apex and the gateway hostname answer "not a tunnel" even when a custom
// domain row names them: the ingress never consults the table for a
// server-owned name.
func TestServerHostnamesIgnoreCustomDomainTable(t *testing.T) {
	f := newDomainFixture(t)
	f.attach(t, "evil", "attacker")
	for _, host := range []string{"rift.test", "gateway.rift.test", "a.b.rift.test"} {
		f.mapDomain(t, host, "evil", "attacker")
		w := f.visit(host)
		if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "served-by") {
			t.Errorf("%s = %d %q, want a 404 that never reaches the tunnel", host, w.Code, w.Body.String())
		}
	}
}
