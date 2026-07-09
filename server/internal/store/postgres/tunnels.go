package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/siliconcolony/tunl/server/internal/core"
)

const tunnelColumns = `id, subdomain, token_id, protocol, local_port, node_id, client_addr, connected_at, last_seen_at`

// pgUniqueViolation is the SQLSTATE Postgres raises for a unique-constraint
// violation. Detection keys on this code (not the human-readable message text)
// so it stays correct across locales and server versions.
const pgUniqueViolation = "23505"

// subdomainUniqueConstraint is the named UNIQUE constraint on tunnels.subdomain,
// used to distinguish a subdomain collision from any other unique violation.
const subdomainUniqueConstraint = "tunnels_subdomain_key"

type tunnelStore struct {
	pool *pgxpool.Pool
}

func scanTunnel(sc scanner) (*core.Tunnel, error) {
	var (
		t     core.Tunnel
		proto string
	)
	if err := sc.Scan(&t.ID, &t.Subdomain, &t.TokenID, &proto, &t.LocalPort, &t.NodeID, &t.ClientAddr, &t.ConnectedAt, &t.LastSeenAt); err != nil {
		return nil, err
	}
	t.Protocol = core.Protocol(proto)
	return &t, nil
}

func (s *tunnelStore) Claim(ctx context.Context, t *core.Tunnel) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO tunnels (`+tunnelColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		t.ID, t.Subdomain, t.TokenID, t.Protocol.String(), t.LocalPort, t.NodeID, t.ClientAddr, t.ConnectedAt, t.LastSeenAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == subdomainUniqueConstraint {
			return fmt.Errorf("subdomain %s: %w", t.Subdomain, core.ErrSubdomainTaken)
		}
		return fmt.Errorf("claim tunnel: %w", err)
	}
	return nil
}

func (s *tunnelStore) Release(ctx context.Context, id string) error {
	// Releasing a tunnel that was already removed (e.g. reaped) is a no-op, so a
	// zero-row delete is deliberately not an error.
	if _, err := s.pool.Exec(ctx, `DELETE FROM tunnels WHERE id = $1`, id); err != nil {
		return fmt.Errorf("release tunnel: %w", err)
	}
	return nil
}

func (s *tunnelStore) Heartbeat(ctx context.Context, id string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE tunnels SET last_seen_at = $2 WHERE id = $1`, id, at)
	if err != nil {
		return fmt.Errorf("heartbeat tunnel: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("tunnel %s: %w", id, core.ErrNotFound)
	}
	return nil
}

func (s *tunnelStore) GetBySubdomain(ctx context.Context, subdomain string) (*core.Tunnel, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+tunnelColumns+` FROM tunnels WHERE subdomain = $1`, subdomain)
	t, err := scanTunnel(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("tunnel on %s: %w", subdomain, core.ErrNotFound)
		}
		return nil, fmt.Errorf("get tunnel by subdomain: %w", err)
	}
	return t, nil
}

func (s *tunnelStore) CountByToken(ctx context.Context, tokenID string) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM tunnels WHERE token_id = $1`, tokenID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count tunnels by token: %w", err)
	}
	return n, nil
}

func (s *tunnelStore) ListActive(ctx context.Context) ([]core.Tunnel, error) {
	// IDs are time-sortable ULIDs, so descending id order is newest first.
	rows, err := s.pool.Query(ctx, `SELECT `+tunnelColumns+` FROM tunnels ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list active tunnels: %w", err)
	}
	defer rows.Close()

	out, err := collectTunnels(rows)
	if err != nil {
		return nil, fmt.Errorf("list active tunnels: %w", err)
	}
	return out, nil
}

func (s *tunnelStore) DeleteStale(ctx context.Context, cutoff time.Time) ([]core.Tunnel, error) {
	rows, err := s.pool.Query(ctx,
		`DELETE FROM tunnels WHERE last_seen_at < $1 RETURNING `+tunnelColumns, cutoff)
	if err != nil {
		return nil, fmt.Errorf("delete stale tunnels: %w", err)
	}
	defer rows.Close()

	out, err := collectTunnels(rows)
	if err != nil {
		return nil, fmt.Errorf("delete stale tunnels: %w", err)
	}
	return out, nil
}

func (s *tunnelStore) DeleteByNode(ctx context.Context, nodeID string) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM tunnels WHERE node_id = $1`, nodeID)
	if err != nil {
		return 0, fmt.Errorf("delete tunnels by node: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func collectTunnels(rows pgx.Rows) ([]core.Tunnel, error) {
	var out []core.Tunnel
	for rows.Next() {
		t, err := scanTunnel(rows)
		if err != nil {
			return nil, fmt.Errorf("scan tunnel: %w", err)
		}
		out = append(out, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tunnels: %w", err)
	}
	return out, nil
}
