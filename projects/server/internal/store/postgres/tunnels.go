package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anomalysh/rift/projects/server/internal/core"
)

// tunnelColumns deliberately omits core.Tunnel.Policy: the schema has no
// column for it and nothing reads it back from the store. A policy is enforced
// from the live session it arrived on, never from this table.
const tunnelColumns = `id, subdomain, token_id, protocol, local_port, node_id, client_addr, connected_at, last_seen_at`

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

// Claim inserts the tunnel. The UNIQUE constraint on subdomain is what makes
// the claim atomic across nodes; translate maps its violation to
// ErrSubdomainTaken, a duplicate tunnel ID to ErrConflict and an unknown token
// to ErrNotFound.
func (s *tunnelStore) Claim(ctx context.Context, t *core.Tunnel) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO tunnels (`+tunnelColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		t.ID, t.Subdomain, t.TokenID, t.Protocol.String(), t.LocalPort, t.NodeID, t.ClientAddr, t.ConnectedAt, t.LastSeenAt)
	return translate(err, "claim subdomain "+t.Subdomain)
}

func (s *tunnelStore) Release(ctx context.Context, id string) error {
	// Releasing a tunnel that was already removed (e.g. reaped) is a no-op, so a
	// zero-row delete is deliberately not an error.
	_, err := s.pool.Exec(ctx, `DELETE FROM tunnels WHERE id = $1`, id)
	return translate(err, "release tunnel "+id)
}

func (s *tunnelStore) Heartbeat(ctx context.Context, id string, at time.Time) error {
	return execOne(ctx, s.pool, "heartbeat tunnel "+id, `UPDATE tunnels SET last_seen_at = $2 WHERE id = $1`, id, at)
}

func (s *tunnelStore) GetBySubdomain(ctx context.Context, subdomain string) (*core.Tunnel, error) {
	t, err := scanTunnel(s.pool.QueryRow(ctx, `SELECT `+tunnelColumns+` FROM tunnels WHERE subdomain = $1`, subdomain))
	if err != nil {
		return nil, translate(err, "tunnel on "+subdomain)
	}
	return t, nil
}

func (s *tunnelStore) CountByToken(ctx context.Context, tokenID string) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM tunnels WHERE token_id = $1`, tokenID).Scan(&n); err != nil {
		return 0, translate(err, "count tunnels by token")
	}
	return n, nil
}

func (s *tunnelStore) ListActive(ctx context.Context) ([]core.Tunnel, error) {
	// IDs are time-sortable ULIDs, so descending id order is newest first.
	return collect(ctx, s.pool, "list active tunnels", scanTunnel,
		`SELECT `+tunnelColumns+` FROM tunnels ORDER BY id DESC`)
}

func (s *tunnelStore) DeleteStale(ctx context.Context, cutoff time.Time) ([]core.Tunnel, error) {
	return collect(ctx, s.pool, "delete stale tunnels", scanTunnel,
		`DELETE FROM tunnels WHERE last_seen_at < $1 RETURNING `+tunnelColumns, cutoff)
}

func (s *tunnelStore) DeleteByNode(ctx context.Context, nodeID string) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM tunnels WHERE node_id = $1`, nodeID)
	if err != nil {
		return 0, translate(err, "delete tunnels by node")
	}
	return int(tag.RowsAffected()), nil
}
