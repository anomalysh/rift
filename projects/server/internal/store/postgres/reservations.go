package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anomalysh/rift/projects/server/internal/core"
)

const reservationColumns = `subdomain, token_id, note, created_at`

type reservationStore struct {
	pool *pgxpool.Pool
}

func scanReservation(sc scanner) (*core.Reservation, error) {
	var r core.Reservation
	if err := sc.Scan(&r.Subdomain, &r.TokenID, &r.Note, &r.CreatedAt); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *reservationStore) Get(ctx context.Context, subdomain string) (*core.Reservation, error) {
	r, err := scanReservation(s.pool.QueryRow(ctx,
		`SELECT `+reservationColumns+` FROM reservations WHERE subdomain = $1`, subdomain))
	if err != nil {
		return nil, translate(err, "get reservation "+subdomain)
	}
	return r, nil
}

// Create inserts a reservation. An already-reserved subdomain is ErrConflict
// and an unknown token is ErrNotFound, as in the memory adapter; the admin API
// relies on the former to answer 409 rather than 500.
func (s *reservationStore) Create(ctx context.Context, r *core.Reservation) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO reservations (`+reservationColumns+`) VALUES ($1, $2, $3, $4)`,
		r.Subdomain, r.TokenID, r.Note, r.CreatedAt)
	return translate(err, "create reservation "+r.Subdomain)
}

func (s *reservationStore) List(ctx context.Context) ([]core.Reservation, error) {
	return collect(ctx, s.pool, "list reservations", scanReservation,
		`SELECT `+reservationColumns+` FROM reservations ORDER BY subdomain`)
}

func (s *reservationStore) Delete(ctx context.Context, subdomain string) error {
	return execOne(ctx, s.pool, "delete reservation "+subdomain,
		`DELETE FROM reservations WHERE subdomain = $1`, subdomain)
}
