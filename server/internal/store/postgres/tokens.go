package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/siliconcolony/tunl/server/internal/core"
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
	row := s.pool.QueryRow(ctx, `SELECT `+tokenColumns+` FROM tokens WHERE token_hash = $1`, hash)
	t, err := scanToken(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("token by hash: %w", core.ErrNotFound)
		}
		return nil, fmt.Errorf("find token by hash: %w", err)
	}
	return t, nil
}

func (s *tokenStore) FindByID(ctx context.Context, id string) (*core.Token, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+tokenColumns+` FROM tokens WHERE id = $1`, id)
	t, err := scanToken(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("token %s: %w", id, core.ErrNotFound)
		}
		return nil, fmt.Errorf("find token by id: %w", err)
	}
	return t, nil
}

func (s *tokenStore) Create(ctx context.Context, t *core.Token) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO tokens (`+tokenColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		t.ID, t.Name, t.TokenHash, t.MaxTunnels, t.CreatedAt, t.LastUsedAt, t.RevokedAt, t.ExpiresAt)
	if err != nil {
		return fmt.Errorf("create token: %w", err)
	}
	return nil
}

func (s *tokenStore) List(ctx context.Context) ([]core.Token, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+tokenColumns+` FROM tokens ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()

	var out []core.Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, fmt.Errorf("scan token: %w", err)
		}
		out = append(out, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tokens: %w", err)
	}
	return out, nil
}

func (s *tokenStore) Revoke(ctx context.Context, id string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE tokens SET revoked_at = $2 WHERE id = $1`, id, at)
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("token %s: %w", id, core.ErrNotFound)
	}
	return nil
}

func (s *tokenStore) TouchLastUsed(ctx context.Context, id string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE tokens SET last_used_at = $2 WHERE id = $1`, id, at)
	if err != nil {
		return fmt.Errorf("touch token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("token %s: %w", id, core.ErrNotFound)
	}
	return nil
}
