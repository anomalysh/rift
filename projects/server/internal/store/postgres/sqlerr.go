package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anomalysh/rift/projects/server/internal/core"
)

// SQLSTATE codes the adapter maps onto core sentinels. Detection keys on the
// code (not the human-readable message text) so it stays correct across
// locales and server versions.
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
)

// subdomainUniqueConstraint is the named UNIQUE constraint on tunnels.subdomain,
// used to distinguish a subdomain collision from any other unique violation.
const subdomainUniqueConstraint = "tunnels_subdomain_key"

// translate is the single place a pgx error becomes a core sentinel, so every
// query maps the same database outcome to the same port error -- and to the
// same error the memory adapter returns for it:
//
//   - no row                         -> core.ErrNotFound
//   - tunnels.subdomain collision    -> core.ErrSubdomainTaken
//   - any other unique violation     -> core.ErrConflict (duplicate id or hash)
//   - foreign-key violation          -> core.ErrNotFound (the referenced token
//     does not exist)
//
// Before this existed each query hand-rolled the no-rows check and nothing
// mapped the constraint violations, so a duplicate reservation surfaced as an
// opaque error (a 500 from the admin API) on Postgres while the memory store
// returned ErrConflict (a 409).
//
// what names the operation or record for the message. The driver error stays
// in the chain so the constraint detail still reaches a log.
func translate(err error, what string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", what, core.ErrNotFound)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgUniqueViolation:
			if pgErr.ConstraintName == subdomainUniqueConstraint {
				return fmt.Errorf("%s: %w: %w", what, core.ErrSubdomainTaken, err)
			}
			return fmt.Errorf("%s: %w: %w", what, core.ErrConflict, err)
		case pgForeignKeyViolation:
			return fmt.Errorf("%s: %w: %w", what, core.ErrNotFound, err)
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

// execOne runs a statement that must touch exactly one existing row (an
// UPDATE or DELETE by key). Zero rows affected means the key is absent, which
// the ports report as core.ErrNotFound.
func execOne(ctx context.Context, pool *pgxpool.Pool, what, sql string, args ...any) error {
	tag, err := pool.Exec(ctx, sql, args...)
	if err != nil {
		return translate(err, what)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: %w", what, core.ErrNotFound)
	}
	return nil
}

// collect runs a multi-row query and decodes every row with scan. The result
// is never nil, matching the memory adapter, so an empty table encodes as []
// rather than null wherever a caller serializes it directly.
func collect[T any](ctx context.Context, pool *pgxpool.Pool, what string, scan func(scanner) (*T, error), sql string, args ...any) ([]T, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, translate(err, what)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (T, error) {
		v, err := scan(r)
		if err != nil {
			var zero T
			return zero, err
		}
		return *v, nil
	})
	if err != nil {
		return nil, translate(err, what)
	}
	return out, nil
}
