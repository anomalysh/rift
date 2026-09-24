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
	got, err := d.Lookup(ctx, "app.acme.com")
	if err != nil || got.Subdomain != "abc" || got.TokenID != "tok1" {
		t.Fatalf("Lookup = %+v, %v; want abc owned by tok1", got, err)
	}

	// The same token reconnecting with a new subdomain refreshes the mapping.
	if err := d.Upsert(ctx, core.CustomDomain{
		Domain: "app.acme.com", Subdomain: "xyz", TokenID: "tok1", CreatedAt: time.Unix(2, 0),
	}); err != nil {
		t.Fatalf("refresh upsert: %v", err)
	}
	if got, _ := d.Lookup(ctx, "app.acme.com"); got.Subdomain != "xyz" {
		t.Fatalf("subdomain after refresh = %q, want xyz", got.Subdomain)
	}

	// A different token cannot claim the same domain.
	err = d.Upsert(ctx, core.CustomDomain{
		Domain: "app.acme.com", Subdomain: "evil", TokenID: "tok2", CreatedAt: time.Unix(3, 0),
	})
	if !errors.Is(err, core.ErrDomainOwned) {
		t.Fatalf("foreign claim err = %v, want ErrDomainOwned", err)
	}

	// A miss is ErrNotFound; delete is idempotent.
	if _, err := d.Lookup(ctx, "nope.example.com"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing domain err = %v, want ErrNotFound", err)
	}
	if err := d.Delete(ctx, "app.acme.com"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := d.Delete(ctx, "app.acme.com"); err != nil {
		t.Fatalf("second delete should be a no-op, got %v", err)
	}
}

// Transfer reassigns a domain only from the owner the caller named, so two
// tokens racing to reclaim the same abandoned domain cannot both win.
func TestDomainStoreTransferIsGuardedByPreviousOwner(t *testing.T) {
	ctx := context.Background()
	d := New().Domains()

	if err := d.Upsert(ctx, core.CustomDomain{Domain: "app.acme.com", Subdomain: "old", TokenID: "dead"}); err != nil {
		t.Fatal(err)
	}

	// tok2 takes it over from the (inactive) owner it observed.
	if err := d.Transfer(ctx, core.CustomDomain{Domain: "app.acme.com", Subdomain: "new", TokenID: "tok2"}, "dead"); err != nil {
		t.Fatalf("transfer from observed owner: %v", err)
	}
	if got, _ := d.Lookup(ctx, "app.acme.com"); got.TokenID != "tok2" || got.Subdomain != "new" {
		t.Fatalf("after transfer = %+v, want tok2/new", got)
	}

	// tok3 also observed "dead" as the owner, but tok2 won the race.
	err := d.Transfer(ctx, core.CustomDomain{Domain: "app.acme.com", Subdomain: "x", TokenID: "tok3"}, "dead")
	if !errors.Is(err, core.ErrDomainOwned) {
		t.Fatalf("stale transfer err = %v, want ErrDomainOwned", err)
	}

	// Transferring an unmapped domain simply creates it.
	if err := d.Transfer(ctx, core.CustomDomain{Domain: "fresh.acme.com", Subdomain: "f", TokenID: "tok3"}, "whoever"); err != nil {
		t.Fatalf("transfer of an unmapped domain: %v", err)
	}
}
