package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/anomalysh/rift/server/internal/config"
	"github.com/anomalysh/rift/server/internal/core"
	"github.com/anomalysh/rift/server/internal/store/migrations"
)

// testDB opens a pool against RIFT_TEST_POSTGRES_DSN, migrates, and truncates so
// each test starts from a clean, deterministic state. It skips when the DSN is
// unset so `go test ./...` stays green without a database.
func testDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("RIFT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set RIFT_TEST_POSTGRES_DSN to run postgres store tests")
	}

	ctx := context.Background()
	db, err := Open(ctx, config.Postgres{
		DSN:            dsn,
		MaxConns:       4,
		MinConns:       1,
		ConnectTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(db.Close)

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.pool.Exec(ctx, `TRUNCATE tunnels, reservations, tokens CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return db
}

func seedToken(t *testing.T, db *DB) *core.Token {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	tok := &core.Token{
		ID:         core.MustNewID(now),
		Name:       "test",
		TokenHash:  core.MustNewID(now), // any unique value satisfies the UNIQUE column
		MaxTunnels: 5,
		CreatedAt:  now,
	}
	if err := db.Tokens().Create(context.Background(), tok); err != nil {
		t.Fatalf("create token: %v", err)
	}
	return tok
}

func newTunnel(tokenID, subdomain string, lastSeen time.Time) *core.Tunnel {
	return &core.Tunnel{
		ID:          core.MustNewID(lastSeen),
		Subdomain:   subdomain,
		TokenID:     tokenID,
		Protocol:    core.ProtocolHTTP,
		LocalPort:   3000,
		NodeID:      "node-1",
		ClientAddr:  "1.2.3.4:5678",
		ConnectedAt: lastSeen,
		LastSeenAt:  lastSeen,
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := testDB(t) // already migrated once inside testDB
	ctx := context.Background()

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	var applied, files int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	migs, err := parseMigrations(migrations.FS)
	if err != nil {
		t.Fatalf("parse migrations: %v", err)
	}
	files = len(migs)
	if applied != files {
		t.Fatalf("want %d applied migrations, got %d", files, applied)
	}
}

func TestClaimConflictReturnsSubdomainTaken(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tok := seedToken(t, db)
	now := time.Now().UTC()

	if err := db.Tunnels().Claim(ctx, newTunnel(tok.ID, "alpha", now)); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	err := db.Tunnels().Claim(ctx, newTunnel(tok.ID, "alpha", now))
	if !errors.Is(err, core.ErrSubdomainTaken) {
		t.Fatalf("want ErrSubdomainTaken, got %v", err)
	}
}

func TestHeartbeatMissingTunnelReturnsNotFound(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	err := db.Tunnels().Heartbeat(ctx, core.MustNewID(now), now)
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tok := seedToken(t, db)
	now := time.Now().UTC()

	tun := newTunnel(tok.ID, "beta", now)
	if err := db.Tunnels().Claim(ctx, tun); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := db.Tunnels().Release(ctx, tun.ID); err != nil {
		t.Fatalf("first release: %v", err)
	}
	if err := db.Tunnels().Release(ctx, tun.ID); err != nil {
		t.Fatalf("release of already-released tunnel should be nil, got %v", err)
	}
}

func TestDeleteStaleReturnsReapedRows(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tok := seedToken(t, db)
	now := time.Now().UTC()

	stale := newTunnel(tok.ID, "stale", now.Add(-time.Hour))
	fresh := newTunnel(tok.ID, "fresh", now)
	if err := db.Tunnels().Claim(ctx, stale); err != nil {
		t.Fatalf("claim stale: %v", err)
	}
	if err := db.Tunnels().Claim(ctx, fresh); err != nil {
		t.Fatalf("claim fresh: %v", err)
	}

	reaped, err := db.Tunnels().DeleteStale(ctx, now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("delete stale: %v", err)
	}
	if len(reaped) != 1 {
		t.Fatalf("want 1 reaped tunnel, got %d", len(reaped))
	}
	if reaped[0].ID != stale.ID || reaped[0].Subdomain != "stale" {
		t.Fatalf("reaped wrong tunnel: %+v", reaped[0])
	}

	// The fresh tunnel must survive.
	if _, err := db.Tunnels().GetBySubdomain(ctx, "fresh"); err != nil {
		t.Fatalf("fresh tunnel should still exist: %v", err)
	}
	if _, err := db.Tunnels().GetBySubdomain(ctx, "stale"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("stale tunnel should be gone, got %v", err)
	}
}

func TestCountByToken(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tokA := seedToken(t, db)
	tokB := seedToken(t, db)
	now := time.Now().UTC()

	for _, sd := range []string{"a1", "a2", "a3"} {
		if err := db.Tunnels().Claim(ctx, newTunnel(tokA.ID, sd, now)); err != nil {
			t.Fatalf("claim %s: %v", sd, err)
		}
	}
	if err := db.Tunnels().Claim(ctx, newTunnel(tokB.ID, "b1", now)); err != nil {
		t.Fatalf("claim b1: %v", err)
	}

	n, err := db.Tunnels().CountByToken(ctx, tokA.ID)
	if err != nil {
		t.Fatalf("count token A: %v", err)
	}
	if n != 3 {
		t.Fatalf("want 3 tunnels for token A, got %d", n)
	}

	n, err = db.Tunnels().CountByToken(ctx, tokB.ID)
	if err != nil {
		t.Fatalf("count token B: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 tunnel for token B, got %d", n)
	}
}

func TestTokenLookupAndNotFound(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tok := seedToken(t, db)

	got, err := db.Tokens().FindByHash(ctx, tok.TokenHash)
	if err != nil {
		t.Fatalf("find by hash: %v", err)
	}
	if got.ID != tok.ID {
		t.Fatalf("want id %s, got %s", tok.ID, got.ID)
	}

	if _, err := db.Tokens().FindByID(ctx, "does-not-exist"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
