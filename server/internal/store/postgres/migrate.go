package postgres

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anomaly-sh/rift/server/internal/store/migrations"
)

// migrationLockKey scopes the session-level advisory lock the runner holds
// while migrating. Its value is arbitrary but must never change, or two
// releases built at different times would use different keys and fail to
// exclude each other.
const migrationLockKey int64 = 0x74756e6c6d696700 // "riftmig\0"

// migration is one parsed SQL file: its numeric version, source filename and body.
type migration struct {
	version int64
	name    string
	sql     string
}

// parseMigrations reads every *.sql file from fsys, derives each version from
// the leading digits of its filename, and returns them ordered by version. It
// takes an fs.FS and touches no database so the ordering and validation rules
// are unit-testable in isolation.
func parseMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, err := versionFromName(e.Name())
		if err != nil {
			return nil, err
		}
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: e.Name(), sql: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("duplicate migration version %d in %s and %s", out[i].version, out[i-1].name, out[i].name)
		}
	}
	return out, nil
}

// versionFromName reads the numeric prefix before the first underscore, e.g.
// "0007_add_index.sql" -> 7.
func versionFromName(name string) (int64, error) {
	digits := name
	if i := strings.IndexByte(name, '_'); i >= 0 {
		digits = name[:i]
	} else {
		digits = strings.TrimSuffix(name, ".sql")
	}
	if digits == "" {
		return 0, fmt.Errorf("migration %q has no numeric version prefix", name)
	}
	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("migration %q has a non-numeric version prefix %q", name, digits)
	}
	return v, nil
}

// runMigrations applies pending migrations idempotently. It serialises
// concurrent riftd instances with a Postgres advisory lock so two processes
// starting together cannot both try to apply the same file.
func runMigrations(ctx context.Context, pool *pgxpool.Pool) (err error) {
	migs, err := parseMigrations(migrations.FS)
	if err != nil {
		return err
	}

	// The lock is session-level, so it must live on one connection for the whole
	// run rather than any pooled connection.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration conn: %w", err)
	}

	locked := false
	defer func() {
		if locked {
			// Unlock on a fresh context so a cancelled ctx cannot orphan the
			// session lock. If the unlock itself fails the connection is broken,
			// and Release then discards it, ending the session and dropping the
			// lock server-side either way.
			if _, uerr := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey); uerr != nil && err == nil {
				err = fmt.Errorf("release advisory lock: %w", uerr)
			}
		}
		conn.Release()
	}()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}
	locked = true

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    bigint      PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := make(map[int64]bool)
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("load applied migrations: %w", err)
	}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("scan applied version: %w", err)
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate applied migrations: %w", err)
	}

	for _, m := range migs {
		if applied[m.version] {
			continue
		}
		if err := applyMigration(ctx, conn, m); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration runs one migration and records its version in a single
// transaction, so a mid-file failure leaves neither partial schema nor a false
// applied marker.
func applyMigration(ctx context.Context, conn *pgxpool.Conn, m migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", m.name, err)
	}
	defer tx.Rollback(ctx)

	// No args, so pgx uses the simple protocol and a file with several
	// statements executes as one.
	if _, err := tx.Exec(ctx, m.sql); err != nil {
		return fmt.Errorf("apply migration %s: %w", m.name, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.version); err != nil {
		return fmt.Errorf("record migration %s: %w", m.name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %s: %w", m.name, err)
	}
	return nil
}
