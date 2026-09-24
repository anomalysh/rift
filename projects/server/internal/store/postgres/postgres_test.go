package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/anomalysh/rift/projects/server/internal/config"
	"github.com/anomalysh/rift/projects/server/internal/core"
	"github.com/anomalysh/rift/projects/server/internal/store/migrations"
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
