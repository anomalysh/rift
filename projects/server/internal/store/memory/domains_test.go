package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anomalysh/rift/projects/server/internal/core"
)

func TestDomainStoreOwnershipAndRefresh(t *testing.T) {
	ctx := context.Background()
	s := New()
	d := s.Domains()

	// First registration.
	if err := d.Upsert(ctx, core.CustomDomain{
		Domain: "app.acme.com", Subdomain: "abc", TokenID: "tok1", CreatedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	sub, err := d.SubdomainFor(ctx, "app.acme.com")
	if err != nil || sub != "abc" {
		t.Fatalf("SubdomainFor = %q, %v; want abc", sub, err)
	}

	// The same token reconnecting with a new subdomain refreshes the mapping.
	if err := d.Upsert(ctx, core.CustomDomain{
		Domain: "app.acme.com", Subdomain: "xyz", TokenID: "tok1", CreatedAt: time.Unix(2, 0),
	}); err != nil {
		t.Fatalf("refresh upsert: %v", err)
	}
	if sub, _ := d.SubdomainFor(ctx, "app.acme.com"); sub != "xyz" {
		t.Fatalf("subdomain after refresh = %q, want xyz", sub)
	}

	// A different token cannot claim the same domain.
	err = d.Upsert(ctx, core.CustomDomain{
		Domain: "app.acme.com", Subdomain: "evil", TokenID: "tok2", CreatedAt: time.Unix(3, 0),
	})
	if !errors.Is(err, core.ErrDomainOwned) {
		t.Fatalf("foreign claim err = %v, want ErrDomainOwned", err)
	}

	// A miss is ErrNotFound; delete is idempotent.
	if _, err := d.SubdomainFor(ctx, "nope.example.com"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing domain err = %v, want ErrNotFound", err)
	}
	if err := d.Delete(ctx, "app.acme.com"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := d.Delete(ctx, "app.acme.com"); err != nil {
		t.Fatalf("second delete should be a no-op, got %v", err)
	}
}
