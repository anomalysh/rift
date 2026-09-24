package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anomalysh/rift/projects/server/internal/core"
)

const domainColumns = `domain, subdomain, token_id, created_at`

type domainStore struct {
	pool *pgxpool.Pool
}

func scanDomain(sc scanner) (*core.CustomDomain, error) {
	var d core.CustomDomain
	if err := sc.Scan(&d.Domain, &d.Subdomain, &d.TokenID, &d.CreatedAt); err != nil {
		return nil, err
	}
	return &d, nil
}

// Upsert inserts the mapping, or updates the subdomain when the SAME token
// reconnects. The conflict update is guarded by token_id so a second token
// cannot repoint another token's domain: when the guard fails no row is
// returned, which we translate to ErrDomainOwned.
func (s *domainStore) Upsert(ctx context.Context, d core.CustomDomain) error {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO custom_domains (`+domainColumns+`) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (domain) DO UPDATE SET subdomain = EXCLUDED.subdomain
		 WHERE custom_domains.token_id = EXCLUDED.token_id
		 RETURNING token_id`,
		d.Domain, d.Subdomain, d.TokenID, d.CreatedAt)
	var owner string
	if err := row.Scan(&owner); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("domain %s: %w", d.Domain, core.ErrDomainOwned)
		}
		return translate(err, "upsert custom domain")
	}
	return nil
}

// Transfer moves a domain to d.TokenID provided it is still held by
// fromTokenID (or already by d.TokenID). The guard sits in the conflict
// update's WHERE clause, so the ownership check and the write are a single
// statement: if another token reclaimed the domain first, no row is returned
// and the caller gets ErrDomainOwned instead of silently stealing it back.
// created_at is reset because, for the new owner, the mapping is new.
func (s *domainStore) Transfer(ctx context.Context, d core.CustomDomain, fromTokenID string) error {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO custom_domains (`+domainColumns+`) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (domain) DO UPDATE
		 SET subdomain = EXCLUDED.subdomain, token_id = EXCLUDED.token_id, created_at = EXCLUDED.created_at
		 WHERE custom_domains.token_id = $5 OR custom_domains.token_id = EXCLUDED.token_id
		 RETURNING token_id`,
		d.Domain, d.Subdomain, d.TokenID, d.CreatedAt, fromTokenID)
	var owner string
	if err := row.Scan(&owner); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("domain %s: %w", d.Domain, core.ErrDomainOwned)
		}
		return translate(err, "transfer custom domain")
	}
	return nil
}

func (s *domainStore) Lookup(ctx context.Context, domain string) (*core.CustomDomain, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+domainColumns+` FROM custom_domains WHERE domain = $1`, domain)
	d, err := scanDomain(row)
	if err != nil {
		return nil, translate(err, "domain "+domain)
	}
	return d, nil
}

func (s *domainStore) List(ctx context.Context) ([]core.CustomDomain, error) {
	return collect(ctx, s.pool, "list custom domains", scanDomain,
		`SELECT `+domainColumns+` FROM custom_domains ORDER BY domain`)
}

func (s *domainStore) Delete(ctx context.Context, domain string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM custom_domains WHERE domain = $1`, domain); err != nil {
		return translate(err, "delete custom domain")
	}
	return nil
}
