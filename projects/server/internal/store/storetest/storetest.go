// Package storetest is the conformance suite for the core storage ports.
//
// The memory adapter stands in for Postgres in every gateway, ingress and e2e
// test, so any behavioural drift between the two -- a different sentinel
// error, ordering, or edge case -- lets a test pass against a store production
// does not run. Both adapters run this one table: memory always, postgres when
// RIFT_TEST_POSTGRES_DSN is set. A new port method or edge case belongs here,
// not in either adapter's own tests.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/anomalysh/rift/projects/server/internal/core"
)

// Stores is one fresh, empty set of adapters under test.
type Stores struct {
	Tokens       core.TokenStore
	Reservations core.ReservationStore
	Tunnels      core.TunnelStore
	Domains      core.DomainStore
}

// Factory returns empty stores for one subtest.
type Factory func(t *testing.T) Stores

// epoch anchors every timestamp. Microsecond precision is what Postgres keeps,
// so values round-trip exactly.
var epoch = time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)

// at returns epoch plus n milliseconds. ULIDs order by millisecond, so IDs
// minted from distinct at(n) values sort in n order.
func at(n int) time.Time { return epoch.Add(time.Duration(n) * time.Millisecond) }

type testCase struct {
	name string
	run  func(t *testing.T, ctx context.Context, s Stores)
}

// Run executes every conformance case against stores from newStores.
func Run(t *testing.T, newStores Factory) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t, context.Background(), newStores(t))
		})
	}
}

var cases = []testCase{
	// --- tokens -----------------------------------------------------------
	{"TokenRoundTrip", func(t *testing.T, ctx context.Context, s Stores) {
		exp := at(9_000)
		want := core.Token{ID: core.MustNewID(at(1)), Name: "ci", TokenHash: "h1", MaxTunnels: 3, CreatedAt: at(1), ExpiresAt: &exp}
		mustNoErr(t, "create", s.Tokens.Create(ctx, &want))

		got, err := s.Tokens.FindByID(ctx, want.ID)
		mustNoErr(t, "find by id", err)
		assertToken(t, got, want)
		got, err = s.Tokens.FindByHash(ctx, "h1")
		mustNoErr(t, "find by hash", err)
		assertToken(t, got, want)
	}},
	{"TokenMissingIsNotFound", func(t *testing.T, ctx context.Context, s Stores) {
		_, err := s.Tokens.FindByID(ctx, "nope")
		mustBe(t, "find by id", err, core.ErrNotFound)
		_, err = s.Tokens.FindByHash(ctx, "nope")
		mustBe(t, "find by hash", err, core.ErrNotFound)
		mustBe(t, "revoke", s.Tokens.Revoke(ctx, "nope", at(1)), core.ErrNotFound)
		mustBe(t, "touch", s.Tokens.TouchLastUsed(ctx, "nope", at(1)), core.ErrNotFound)
	}},
	{"TokenDuplicateIsConflict", func(t *testing.T, ctx context.Context, s Stores) {
		first := seedToken(t, ctx, s, 1)
		dupID := core.Token{ID: first.ID, TokenHash: "other", CreatedAt: at(2)}
		mustBe(t, "duplicate id", s.Tokens.Create(ctx, &dupID), core.ErrConflict)
		dupHash := core.Token{ID: core.MustNewID(at(3)), TokenHash: first.TokenHash, CreatedAt: at(3)}
		mustBe(t, "duplicate hash", s.Tokens.Create(ctx, &dupHash), core.ErrConflict)

		// The original survives both attempts untouched.
		got, err := s.Tokens.FindByID(ctx, first.ID)
		mustNoErr(t, "find", err)
		assertToken(t, got, first)
	}},
	{"TokenRevokeAndTouch", func(t *testing.T, ctx context.Context, s Stores) {
		tok := seedToken(t, ctx, s, 1)
		mustNoErr(t, "revoke", s.Tokens.Revoke(ctx, tok.ID, at(5)))
		mustNoErr(t, "touch", s.Tokens.TouchLastUsed(ctx, tok.ID, at(6)))
		got, err := s.Tokens.FindByID(ctx, tok.ID)
		mustNoErr(t, "find", err)
		if got.RevokedAt == nil || !got.RevokedAt.Equal(at(5)) {
			t.Fatalf("RevokedAt = %v, want %v", got.RevokedAt, at(5))
		}
		if got.LastUsedAt == nil || !got.LastUsedAt.Equal(at(6)) {
			t.Fatalf("LastUsedAt = %v, want %v", got.LastUsedAt, at(6))
		}
		if got.Active(at(7)) {
			t.Fatal("revoked token reports active")
		}
	}},
	{"TokenListOldestFirst", func(t *testing.T, ctx context.Context, s Stores) {
		list, err := s.Tokens.List(ctx)
		mustNoErr(t, "list empty", err)
		mustEmpty(t, len(list), list == nil)

		// Created out of order; listed by ID ascending.
		c := seedToken(t, ctx, s, 3)
		a := seedToken(t, ctx, s, 1)
		b := seedToken(t, ctx, s, 2)
		list, err = s.Tokens.List(ctx)
		mustNoErr(t, "list", err)
		assertOrder(t, tokenIDs(list), a.ID, b.ID, c.ID)
	}},

	// --- reservations -----------------------------------------------------
	{"ReservationRoundTrip", func(t *testing.T, ctx context.Context, s Stores) {
		tok := seedToken(t, ctx, s, 1)
		want := core.Reservation{Subdomain: "myapp", TokenID: tok.ID, Note: "demo", CreatedAt: at(2)}
		mustNoErr(t, "create", s.Reservations.Create(ctx, &want))
		got, err := s.Reservations.Get(ctx, "myapp")
		mustNoErr(t, "get", err)
		if got.Subdomain != want.Subdomain || got.TokenID != want.TokenID || got.Note != want.Note || !got.CreatedAt.Equal(want.CreatedAt) {
			t.Fatalf("got %+v, want %+v", *got, want)
		}
	}},
	{"ReservationErrors", func(t *testing.T, ctx context.Context, s Stores) {
		tok := seedToken(t, ctx, s, 1)
		_, err := s.Reservations.Get(ctx, "nope")
		mustBe(t, "get missing", err, core.ErrNotFound)
		mustBe(t, "delete missing", s.Reservations.Delete(ctx, "nope"), core.ErrNotFound)

		mustNoErr(t, "create", s.Reservations.Create(ctx, &core.Reservation{Subdomain: "taken", TokenID: tok.ID, CreatedAt: at(2)}))
		// The admin API answers 409 for this only if every adapter says ErrConflict.
		mustBe(t, "duplicate", s.Reservations.Create(ctx, &core.Reservation{Subdomain: "taken", TokenID: tok.ID, CreatedAt: at(3)}), core.ErrConflict)
		mustBe(t, "unknown token", s.Reservations.Create(ctx, &core.Reservation{Subdomain: "orphan", TokenID: "ghost", CreatedAt: at(3)}), core.ErrNotFound)

		mustNoErr(t, "delete", s.Reservations.Delete(ctx, "taken"))
		_, err = s.Reservations.Get(ctx, "taken")
		mustBe(t, "get deleted", err, core.ErrNotFound)
	}},
	{"ReservationListBySubdomain", func(t *testing.T, ctx context.Context, s Stores) {
		list, err := s.Reservations.List(ctx)
		mustNoErr(t, "list empty", err)
		mustEmpty(t, len(list), list == nil)

		tok := seedToken(t, ctx, s, 1)
		for _, sub := range []string{"charlie", "alpha", "bravo"} {
			mustNoErr(t, "create "+sub, s.Reservations.Create(ctx, &core.Reservation{Subdomain: sub, TokenID: tok.ID, CreatedAt: at(2)}))
		}
		list, err = s.Reservations.List(ctx)
		mustNoErr(t, "list", err)
		subs := make([]string, len(list))
		for i, r := range list {
			subs[i] = r.Subdomain
		}
		assertOrder(t, subs, "alpha", "bravo", "charlie")
	}},

	// --- tunnels ----------------------------------------------------------
	{"TunnelRoundTrip", func(t *testing.T, ctx context.Context, s Stores) {
		tok := seedToken(t, ctx, s, 1)
		want := newTunnel(tok.ID, "alpha", 2)
		mustNoErr(t, "claim", s.Tunnels.Claim(ctx, &want))
		got, err := s.Tunnels.GetBySubdomain(ctx, "alpha")
		mustNoErr(t, "get", err)
		assertTunnel(t, got, want)
	}},
	{"TunnelClaimConflicts", func(t *testing.T, ctx context.Context, s Stores) {
		tok := seedToken(t, ctx, s, 1)
		first := newTunnel(tok.ID, "alpha", 2)
		mustNoErr(t, "claim", s.Tunnels.Claim(ctx, &first))

		rival := newTunnel(tok.ID, "alpha", 3)
		mustBe(t, "same subdomain", s.Tunnels.Claim(ctx, &rival), core.ErrSubdomainTaken)

		moved := first
		moved.Subdomain = "beta"
		mustBe(t, "same id, other subdomain", s.Tunnels.Claim(ctx, &moved), core.ErrConflict)
		if _, err := s.Tunnels.GetBySubdomain(ctx, "beta"); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("rejected claim still occupies beta: %v", err)
		}
		got, err := s.Tunnels.GetBySubdomain(ctx, "alpha")
		mustNoErr(t, "get original", err)
		assertTunnel(t, got, first)
	}},
	{"TunnelReleaseIsIdempotentAndFreesSubdomain", func(t *testing.T, ctx context.Context, s Stores) {
		tok := seedToken(t, ctx, s, 1)
		tn := newTunnel(tok.ID, "alpha", 2)
		mustNoErr(t, "claim", s.Tunnels.Claim(ctx, &tn))
		mustNoErr(t, "release", s.Tunnels.Release(ctx, tn.ID))
		mustNoErr(t, "release again", s.Tunnels.Release(ctx, tn.ID))
		mustNoErr(t, "release unknown", s.Tunnels.Release(ctx, "never-existed"))

		next := newTunnel(tok.ID, "alpha", 3)
		mustNoErr(t, "reclaim", s.Tunnels.Claim(ctx, &next))
	}},
	{"TunnelHeartbeat", func(t *testing.T, ctx context.Context, s Stores) {
		tok := seedToken(t, ctx, s, 1)
		tn := newTunnel(tok.ID, "alpha", 2)
		mustNoErr(t, "claim", s.Tunnels.Claim(ctx, &tn))
		mustNoErr(t, "heartbeat", s.Tunnels.Heartbeat(ctx, tn.ID, at(50)))
		got, err := s.Tunnels.GetBySubdomain(ctx, "alpha")
		mustNoErr(t, "get", err)
		if !got.LastSeenAt.Equal(at(50)) {
			t.Fatalf("LastSeenAt = %v, want %v", got.LastSeenAt, at(50))
		}
		// A reaped tunnel's heartbeat must fail so its gateway stops serving.
		mustBe(t, "heartbeat missing", s.Tunnels.Heartbeat(ctx, "gone", at(51)), core.ErrNotFound)
		_, err = s.Tunnels.GetBySubdomain(ctx, "nope")
		mustBe(t, "get missing", err, core.ErrNotFound)
	}},
	{"TunnelCountAndList", func(t *testing.T, ctx context.Context, s Stores) {
		list, err := s.Tunnels.ListActive(ctx)
		mustNoErr(t, "list empty", err)
		mustEmpty(t, len(list), list == nil)
		n, err := s.Tunnels.CountByToken(ctx, "nobody")
		mustNoErr(t, "count empty", err)
		if n != 0 {
			t.Fatalf("count for unknown token = %d, want 0", n)
		}

		a := seedToken(t, ctx, s, 1)
		b := seedToken(t, ctx, s, 2)
		var ids []string
		for i, tokID := range []string{a.ID, b.ID, a.ID} {
			tn := newTunnel(tokID, fmt.Sprintf("sub%d", i), 10+i)
			mustNoErr(t, "claim", s.Tunnels.Claim(ctx, &tn))
			ids = append(ids, tn.ID)
		}
		if n, _ := s.Tunnels.CountByToken(ctx, a.ID); n != 2 {
			t.Fatalf("count for a = %d, want 2", n)
		}
		list, err = s.Tunnels.ListActive(ctx)
		mustNoErr(t, "list", err)
		got := make([]string, len(list))
		for i, tn := range list {
			got[i] = tn.ID
		}
		assertOrder(t, got, ids[2], ids[1], ids[0]) // newest first
	}},
	{"TunnelDeleteStaleIsStrictlyBeforeCutoff", func(t *testing.T, ctx context.Context, s Stores) {
		reaped, err := s.Tunnels.DeleteStale(ctx, at(100))
		mustNoErr(t, "delete stale empty", err)
		mustEmpty(t, len(reaped), reaped == nil)

		tok := seedToken(t, ctx, s, 1)
		stale := newTunnel(tok.ID, "stale", 2)
		stale.LastSeenAt = at(99)
		edge := newTunnel(tok.ID, "edge", 3)
		edge.LastSeenAt = at(100) // exactly at the cutoff: survives
		for _, tn := range []*core.Tunnel{&stale, &edge} {
			mustNoErr(t, "claim "+tn.Subdomain, s.Tunnels.Claim(ctx, tn))
		}
		reaped, err = s.Tunnels.DeleteStale(ctx, at(100))
		mustNoErr(t, "delete stale", err)
		if len(reaped) != 1 {
			t.Fatalf("reaped %d tunnels, want 1", len(reaped))
		}
		assertTunnel(t, &reaped[0], stale)
		if _, err := s.Tunnels.GetBySubdomain(ctx, "edge"); err != nil {
			t.Fatalf("tunnel at the cutoff was reaped: %v", err)
		}
	}},
	{"TunnelDeleteByNode", func(t *testing.T, ctx context.Context, s Stores) {
		tok := seedToken(t, ctx, s, 1)
		for i, node := range []string{"n1", "n2", "n1"} {
			tn := newTunnel(tok.ID, fmt.Sprintf("sub%d", i), 10+i)
			tn.NodeID = node
			mustNoErr(t, "claim", s.Tunnels.Claim(ctx, &tn))
		}
		n, err := s.Tunnels.DeleteByNode(ctx, "n1")
		mustNoErr(t, "delete by node", err)
		if n != 2 {
			t.Fatalf("deleted %d, want 2", n)
		}
		if n, _ := s.Tunnels.DeleteByNode(ctx, "n1"); n != 0 {
			t.Fatalf("second delete removed %d, want 0", n)
		}
		list, _ := s.Tunnels.ListActive(ctx)
		if len(list) != 1 || list[0].NodeID != "n2" {
			t.Fatalf("survivors = %+v, want only n2's tunnel", list)
		}
	}},

	// --- custom domains ---------------------------------------------------
	{"DomainOwnershipAndRefresh", func(t *testing.T, ctx context.Context, s Stores) {
		owner := seedToken(t, ctx, s, 1)
		other := seedToken(t, ctx, s, 2)
		mustNoErr(t, "upsert", s.Domains.Upsert(ctx, core.CustomDomain{Domain: "app.acme.com", Subdomain: "abc", TokenID: owner.ID, CreatedAt: at(3)}))
		// The same token reconnecting on a new subdomain moves the mapping.
		mustNoErr(t, "refresh", s.Domains.Upsert(ctx, core.CustomDomain{Domain: "app.acme.com", Subdomain: "xyz", TokenID: owner.ID, CreatedAt: at(4)}))
		// Another token may not repoint it.
		mustBe(t, "foreign upsert", s.Domains.Upsert(ctx, core.CustomDomain{Domain: "app.acme.com", Subdomain: "evil", TokenID: other.ID, CreatedAt: at(5)}), core.ErrDomainOwned)

		list, err := s.Domains.List(ctx)
		mustNoErr(t, "list", err)
		if len(list) != 1 || list[0].Subdomain != "xyz" || list[0].TokenID != owner.ID {
			t.Fatalf("mappings = %+v, want app.acme.com -> xyz owned by %s", list, owner.ID)
		}
	}},
	{"DomainLookupAndTransfer", func(t *testing.T, ctx context.Context, s Stores) {
		owner := seedToken(t, ctx, s, 1)
		heir := seedToken(t, ctx, s, 2)
		third := seedToken(t, ctx, s, 3)

		_, err := s.Domains.Lookup(ctx, "app.acme.com")
		mustBe(t, "lookup absent", err, core.ErrNotFound)

		mustNoErr(t, "upsert", s.Domains.Upsert(ctx, core.CustomDomain{Domain: "app.acme.com", Subdomain: "abc", TokenID: owner.ID, CreatedAt: at(4)}))
		got, err := s.Domains.Lookup(ctx, "app.acme.com")
		mustNoErr(t, "lookup", err)
		if got.Subdomain != "abc" || got.TokenID != owner.ID {
			t.Fatalf("lookup = %+v, want abc owned by %s", got, owner.ID)
		}

		// A transfer names the holder it expects; it succeeds only while that
		// token still holds the domain, so two claimants cannot both win.
		mustNoErr(t, "transfer", s.Domains.Transfer(ctx, core.CustomDomain{Domain: "app.acme.com", Subdomain: "def", TokenID: heir.ID, CreatedAt: at(5)}, owner.ID))
		got, err = s.Domains.Lookup(ctx, "app.acme.com")
		mustNoErr(t, "lookup after transfer", err)
		if got.Subdomain != "def" || got.TokenID != heir.ID {
			t.Fatalf("after transfer = %+v, want def owned by %s", got, heir.ID)
		}
		mustBe(t, "stale transfer", s.Domains.Transfer(ctx, core.CustomDomain{Domain: "app.acme.com", Subdomain: "ghi", TokenID: third.ID, CreatedAt: at(6)}, owner.ID), core.ErrDomainOwned)
		// The current holder re-transferring to itself is a refresh, not a theft.
		mustNoErr(t, "self transfer", s.Domains.Transfer(ctx, core.CustomDomain{Domain: "app.acme.com", Subdomain: "jkl", TokenID: heir.ID, CreatedAt: at(7)}, owner.ID))
		// A transfer of an unmapped domain simply creates it.
		mustNoErr(t, "transfer absent", s.Domains.Transfer(ctx, core.CustomDomain{Domain: "new.acme.com", Subdomain: "mno", TokenID: third.ID, CreatedAt: at(8)}, owner.ID))
		got, err = s.Domains.Lookup(ctx, "new.acme.com")
		mustNoErr(t, "lookup created", err)
		if got.TokenID != third.ID {
			t.Fatalf("created mapping owned by %s, want %s", got.TokenID, third.ID)
		}
	}},
	{"DomainListAndDelete", func(t *testing.T, ctx context.Context, s Stores) {
		list, err := s.Domains.List(ctx)
		mustNoErr(t, "list empty", err)
		if len(list) != 0 {
			t.Fatalf("empty store listed %d domains", len(list))
		}
		tok := seedToken(t, ctx, s, 1)
		for _, d := range []string{"c.example.com", "a.example.com", "b.example.com"} {
			mustNoErr(t, "upsert "+d, s.Domains.Upsert(ctx, core.CustomDomain{Domain: d, Subdomain: "sub", TokenID: tok.ID, CreatedAt: at(2)}))
		}
		list, err = s.Domains.List(ctx)
		mustNoErr(t, "list", err)
		names := make([]string, len(list))
		for i, d := range list {
			names[i] = d.Domain
		}
		assertOrder(t, names, "a.example.com", "b.example.com", "c.example.com")

		mustNoErr(t, "delete", s.Domains.Delete(ctx, "a.example.com"))
		mustNoErr(t, "delete again", s.Domains.Delete(ctx, "a.example.com"))
		list, _ = s.Domains.List(ctx)
		if len(list) != 2 {
			t.Fatalf("after delete listed %d domains, want 2", len(list))
		}
	}},
}

// seedToken creates a token whose ID is minted at at(n), so tokens seeded with
// increasing n sort in that order.
func seedToken(t *testing.T, ctx context.Context, s Stores, n int) core.Token {
	t.Helper()
	tok := core.Token{
		ID:         core.MustNewID(at(n)),
		Name:       fmt.Sprintf("tok%d", n),
		TokenHash:  fmt.Sprintf("hash-%d", n),
		MaxTunnels: 5,
		CreatedAt:  at(n),
	}
	mustNoErr(t, "seed token", s.Tokens.Create(ctx, &tok))
	return tok
}

// newTunnel builds a tunnel whose ID is minted at at(n).
func newTunnel(tokenID, sub string, n int) core.Tunnel {
	return core.Tunnel{
		ID:          core.MustNewID(at(n)),
		Subdomain:   sub,
		TokenID:     tokenID,
		Protocol:    core.ProtocolTCP,
		LocalPort:   5432,
		NodeID:      "node-1",
		ClientAddr:  "198.51.100.7:40000",
		ConnectedAt: at(n),
		LastSeenAt:  at(n),
	}
}

func tokenIDs(list []core.Token) []string {
	out := make([]string, len(list))
	for i, t := range list {
		out[i] = t.ID
	}
	return out
}

func mustNoErr(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func mustBe(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: err = %v, want %v", what, err, want)
	}
}

// mustEmpty checks an empty list is empty and non-nil, so both adapters
// serialize it identically ([] rather than null).
func mustEmpty(t *testing.T, n int, isNil bool) {
	t.Helper()
	if n != 0 || isNil {
		t.Fatalf("empty store returned len=%d nil=%v, want an empty non-nil slice", n, isNil)
	}
}

func assertOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func equalTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func assertToken(t *testing.T, got *core.Token, want core.Token) {
	t.Helper()
	if got.ID != want.ID || got.Name != want.Name || got.TokenHash != want.TokenHash ||
		got.MaxTunnels != want.MaxTunnels || !got.CreatedAt.Equal(want.CreatedAt) ||
		!equalTimePtr(got.LastUsedAt, want.LastUsedAt) || !equalTimePtr(got.RevokedAt, want.RevokedAt) ||
		!equalTimePtr(got.ExpiresAt, want.ExpiresAt) {
		t.Fatalf("token = %+v, want %+v", *got, want)
	}
}

// assertTunnel compares every persisted field. Policy is not one: see the
// postgres adapter's tunnelColumns.
func assertTunnel(t *testing.T, got *core.Tunnel, want core.Tunnel) {
	t.Helper()
	if got.ID != want.ID || got.Subdomain != want.Subdomain || got.TokenID != want.TokenID ||
		got.Protocol != want.Protocol || got.LocalPort != want.LocalPort || got.NodeID != want.NodeID ||
		got.ClientAddr != want.ClientAddr || !got.ConnectedAt.Equal(want.ConnectedAt) ||
		!got.LastSeenAt.Equal(want.LastSeenAt) {
		t.Fatalf("tunnel = %+v, want %+v", *got, want)
	}
}
