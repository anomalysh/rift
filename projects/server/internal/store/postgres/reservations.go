package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
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
	row := s.pool.QueryRow(ctx, `SELECT `+reservationColumns+` FROM reservations WHERE subdomain = $1`, subdomain)
	r, err := scanReservation(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("reservation %s: %w", subdomain, core.ErrNotFound)
		}
		return nil, fmt.Errorf("get reservation: %w", err)
	}
	return r, nil
}

func (s *reservationStore) Create(ctx context.Context, r *core.Reservation) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO reservations (`+reservationColumns+`) VALUES ($1, $2, $3, $4)`,
		r.Subdomain, r.TokenID, r.Note, r.CreatedAt)
	if err != nil {
		return fmt.Errorf("create reservation: %w", err)
	}
	return nil
}

func (s *reservationStore) List(ctx context.Context) ([]core.Reservation, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+reservationColumns+` FROM reservations ORDER BY subdomain`)
	if err != nil {
		return nil, fmt.Errorf("list reservations: %w", err)
	}
	defer rows.Close()

	var out []core.Reservation
	for rows.Next() {
		r, err := scanReservation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan reservation: %w", err)
		}
		out = append(out, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reservations: %w", err)
	}
	return out, nil
}

func (s *reservationStore) Delete(ctx context.Context, subdomain string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM reservations WHERE subdomain = $1`, subdomain)
	if err != nil {
		return fmt.Errorf("delete reservation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("reservation %s: %w", subdomain, core.ErrNotFound)
	}
	return nil
}
