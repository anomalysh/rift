package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anomalysh/rift/projects/server/internal/core"
)

const tokenColumns = `id, name, token_hash, max_tunnels, created_at, last_used_at, revoked_at, expires_at`

type tokenStore struct {
	pool *pgxpool.Pool
}

func scanToken(sc scanner) (*core.Token, error) {
	var t core.Token
	if err := sc.Scan(&t.ID, &t.Name, &t.TokenHash, &t.MaxTunnels, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt, &t.ExpiresAt); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *tokenStore) FindByHash(ctx context.Context, hash string) (*core.Token, error) {
	t, err := scanToken(s.pool.QueryRow(ctx, `SELECT `+tokenColumns+` FROM tokens WHERE token_hash = $1`, hash))
	if err != nil {
		// The hash identifies a credential; it stays out of the error text.
		return nil, translate(err, "find token by hash")
	}
	return t, nil
}

func (s *tokenStore) FindByID(ctx context.Context, id string) (*core.Token, error) {
	t, err := scanToken(s.pool.QueryRow(ctx, `SELECT `+tokenColumns+` FROM tokens WHERE id = $1`, id))
	if err != nil {
		return nil, translate(err, "find token "+id)
	}
	return t, nil
}

// Create inserts a token. A duplicate ID or hash is ErrConflict.
func (s *tokenStore) Create(ctx context.Context, t *core.Token) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO tokens (`+tokenColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		t.ID, t.Name, t.TokenHash, t.MaxTunnels, t.CreatedAt, t.LastUsedAt, t.RevokedAt, t.ExpiresAt)
	return translate(err, "create token "+t.ID)
}

// List returns every token oldest first (IDs are time-sortable ULIDs).
func (s *tokenStore) List(ctx context.Context) ([]core.Token, error) {
	return collect(ctx, s.pool, "list tokens", scanToken, `SELECT `+tokenColumns+` FROM tokens ORDER BY id`)
}

func (s *tokenStore) Revoke(ctx context.Context, id string, at time.Time) error {
	return execOne(ctx, s.pool, "revoke token "+id, `UPDATE tokens SET revoked_at = $2 WHERE id = $1`, id, at)
}

func (s *tokenStore) TouchLastUsed(ctx context.Context, id string, at time.Time) error {
	return execOne(ctx, s.pool, "touch token "+id, `UPDATE tokens SET last_used_at = $2 WHERE id = $1`, id, at)
}
