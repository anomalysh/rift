package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anomalysh/rift/projects/server/internal/auth"
	"github.com/anomalysh/rift/projects/server/internal/config"
	"github.com/anomalysh/rift/projects/server/internal/core"
	"github.com/anomalysh/rift/projects/server/internal/registry"
	"github.com/anomalysh/rift/projects/server/internal/store/memory"
	"github.com/anomalysh/rift/projects/server/internal/tunnelproto"
)

func newDomainTestGateway(t *testing.T) (*Gateway, *memory.Store) {
	t.Helper()
	rules, err := core.NewSubdomainRules(3, 63, config.DefaultSubdomainPattern,
		config.DefaultSubdomainBlocklist, config.DefaultSubdomainGenerator, 10, config.DefaultSubdomainGenAlphabet)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		NodeID:  "node",
		Gateway: config.Gateway{Hostname: "agents.example.org"},
		Tunnel: config.Tunnel{
			BaseDomain: "rift.test", PublicScheme: config.SchemeHTTPS, MaxTunnelsPerToken: 5,
		},
		SubdomainRules: rules,
	}
	store := memory.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := New(cfg, logger, store.Tokens(), store.Reservations(), store.Tunnels(), store.Domains(), registry.NewLocal())
	return g, store
}

func seedGatewayToken(t *testing.T, store *memory.Store, id string) {
	t.Helper()
	if err := store.Tokens().Create(context.Background(), &core.Token{
		ID: id, TokenHash: "hash-" + id, MaxTunnels: 5, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

// A custom domain may not be one of the server's own names: the apex, any name
// under the base (including multi-label ones, which are not valid subdomains
// and so were never caught by the subdomain check), or the gateway hostname.
func TestRegisterDomainsRejectsServerOwnedNames(t *testing.T) {
	g, store := newDomainTestGateway(t)
	seedGatewayToken(t, store, "tok")

	for _, d := range []string{"rift.test", "RIFT.test.", "app.rift.test", "a.b.rift.test", "agents.example.org"} {
		herr := g.registerDomains(context.Background(), []string{d}, "mine", "tok")
		if herr == nil || herr.code != tunnelproto.ErrCodeInvalidDomain {
			t.Errorf("registerDomains(%q) = %v, want %s", d, herr, tunnelproto.ErrCodeInvalidDomain)
		}
	}
	if list, _ := store.Domains().List(context.Background()); len(list) != 0 {
		t.Fatalf("a rejected hello left mappings behind: %+v", list)
	}
}

func TestRegisterDomainsCapsCountAndWritesNothingOnBadEntry(t *testing.T) {
	g, store := newDomainTestGateway(t)
	seedGatewayToken(t, store, "tok")

	many := make([]string, config.DefaultMaxCustomDomainsPerTunnel+1)
	for n := range many {
		many[n] = fmt.Sprintf("d%d.acme.com", n)
	}
	if herr := g.registerDomains(context.Background(), many, "mine", "tok"); herr == nil || herr.code != tunnelproto.ErrCodeInvalidDomain {
		t.Fatalf("over-cap hello = %v, want %s", herr, tunnelproto.ErrCodeInvalidDomain)
	}

	// One good and one bad entry: validation precedes every write.
	if herr := g.registerDomains(context.Background(), []string{"ok.acme.com", "bad_domain"}, "mine", "tok"); herr == nil {
		t.Fatal("a hello with an invalid domain was accepted")
	}
	if list, _ := store.Domains().List(context.Background()); len(list) != 0 {
		t.Fatalf("a rejected hello left mappings behind: %+v", list)
	}

	if herr := g.registerDomains(context.Background(), many[:config.DefaultMaxCustomDomainsPerTunnel], "mine", "tok"); herr != nil {
		t.Fatalf("hello at the cap = %v, want success", herr)
	}
}

// A domain held by an active token stays with it; one whose owner was revoked,
// expired, or deleted can be reclaimed, so a mapping cannot squat forever.
func TestRegisterDomainsReclaimsFromInactiveOwnerOnly(t *testing.T) {
	ctx := context.Background()
	g, store := newDomainTestGateway(t)
	for _, id := range []string{"active", "revoked", "claimant"} {
		seedGatewayToken(t, store, id)
	}
	if err := store.Tokens().Revoke(ctx, "revoked", time.Now()); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := store.Tokens().Create(ctx, &core.Token{
		ID: "expired", TokenHash: "hash-expired", CreatedAt: past, ExpiresAt: &past,
	}); err != nil {
		t.Fatal(err)
	}

	owners := map[string]string{
		"active.acme.com":  "active",
		"revoked.acme.com": "revoked",
		"expired.acme.com": "expired",
		"deleted.acme.com": "ghost", // no such token any more
	}
	for d, owner := range owners {
		if err := store.Domains().Upsert(ctx, core.CustomDomain{Domain: d, Subdomain: "old", TokenID: owner}); err != nil {
			t.Fatal(err)
		}
	}

	herr := g.registerDomains(ctx, []string{"active.acme.com"}, "new", "claimant")
	if herr == nil || herr.code != tunnelproto.ErrCodeDomainOwned {
		t.Fatalf("claiming an active token's domain = %v, want %s", herr, tunnelproto.ErrCodeDomainOwned)
	}
	for _, d := range []string{"revoked.acme.com", "expired.acme.com", "deleted.acme.com"} {
		if herr := g.registerDomains(ctx, []string{d}, "new", "claimant"); herr != nil {
			t.Fatalf("reclaiming %s = %v, want success", d, herr)
		}
		got, err := store.Domains().Lookup(ctx, d)
		if err != nil || got.TokenID != "claimant" || got.Subdomain != "new" {
			t.Fatalf("%s after reclaim = %+v, %v; want claimant/new", d, got, err)
		}
	}
}

// When domain registration fails after the subdomain was claimed, authorize
// must release the claim instead of leaving the label occupied until reaped.
func TestAuthorizeReleasesSubdomainWhenDomainsRejected(t *testing.T) {
	ctx := context.Background()
	g, store := newDomainTestGateway(t)
	plaintext, hash, err := auth.Mint()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Tokens().Create(ctx, &core.Token{ID: "tok", TokenHash: hash, MaxTunnels: 5, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	hello := &tunnelproto.Hello{
		ProtocolVersion: tunnelproto.Version,
		Token:           plaintext,
		Protocol:        string(core.ProtocolHTTP),
		Subdomain:       "myapp",
		Domains:         []string{"rift.test"}, // the apex: refused
	}
	_, _, herr := g.authorize(ctx, httptest.NewRequest("GET", "/tunnel", nil), hello)
	if herr == nil || herr.code != tunnelproto.ErrCodeInvalidDomain {
		t.Fatalf("authorize = %v, want %s", herr, tunnelproto.ErrCodeInvalidDomain)
	}
	if _, err := store.Tunnels().GetBySubdomain(ctx, "myapp"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("subdomain still claimed after a rejected handshake: %v", err)
	}
}
