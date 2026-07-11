// Package db provides a configured pgx connection pool against the
// artist-alley PostgreSQL database.
//
// Every query in the application goes through this package, either by
// using the [*pgxpool.Pool] directly or via sqlc-generated query types
// that accept a pool. We deliberately do not wrap the pool in our own
// interface; pgx is the API.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/config"
)

// Open builds a connection pool from cfg. The pool eagerly verifies one
// connection so callers know within a couple seconds whether the DB is
// reachable.
func Open(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	dsn := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword, cfg.DBName, cfg.DBSSLMode,
	)

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: parse config: %w", err)
	}
	poolCfg.MaxConns = cfg.DBMaxConns
	poolCfg.MinConns = cfg.DBMinConns
	poolCfg.MaxConnLifetime = cfg.DBConnMaxLifetime

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}
