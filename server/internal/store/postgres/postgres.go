// Package postgres is the PostgreSQL adapter implementing the core storage
// ports (TokenStore, ReservationStore, TunnelStore) on top of pgxpool.
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anomalysh/rift/server/internal/config"
	"github.com/anomalysh/rift/server/internal/core"
)

// DB owns the connection pool and hands out the storage-port implementations.
type DB struct {
	pool         *pgxpool.Pool
	tokens       *tokenStore
	reservations *reservationStore
	tunnels      *tunnelStore
}

// scanner is the common Scan surface of pgx.Row and pgx.Rows, letting one
// helper decode a struct from either a single-row query or a row cursor.
type scanner interface {
	Scan(dest ...any) error
}

// Open builds a pool from the DSN, overrides pool sizing and connect timeout
// with the configured values, and verifies connectivity with a ping.
func Open(ctx context.Context, cfg config.Postgres) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse dsn: %w", err)
	}
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = int32(cfg.MaxConns)
	}
	if cfg.MinConns > 0 {
		poolCfg.MinConns = int32(cfg.MinConns)
	}
	if cfg.ConnectTimeout > 0 {
		poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: new pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}

	db := &DB{pool: pool}
	db.tokens = &tokenStore{pool: pool}
	db.reservations = &reservationStore{pool: pool}
	db.tunnels = &tunnelStore{pool: pool}
	return db, nil
}

// Migrate applies every pending schema migration.
func (db *DB) Migrate(ctx context.Context) error { return runMigrations(ctx, db.pool) }

// Ping verifies the pool can reach the database.
func (db *DB) Ping(ctx context.Context) error { return db.pool.Ping(ctx) }

// Tokens returns the TokenStore backed by this pool.
func (db *DB) Tokens() core.TokenStore { return db.tokens }

// Reservations returns the ReservationStore backed by this pool.
func (db *DB) Reservations() core.ReservationStore { return db.reservations }

// Tunnels returns the TunnelStore backed by this pool.
func (db *DB) Tunnels() core.TunnelStore { return db.tunnels }

// Close drains and closes the pool.
func (db *DB) Close() { db.pool.Close() }
