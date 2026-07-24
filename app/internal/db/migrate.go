// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx as a database/sql driver, needed by goose
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/mscrnt/artist-alley/app/internal/config"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Note: //go:embed cannot reach outside the embedding package, so the
// migration .sql files live under app/internal/db/migrations/. This is
// fine; the .sql files are still hand-authored, just neighbours of
// migrate.go.

// Migrate runs every pending Up migration against the configured
// database, in lexical filename order. Safe to call on every boot —
// goose tracks applied versions in a goose_db_version table it
// creates on first run.
//
// The connection used here is a short-lived *sql.DB on top of pgx;
// the rest of the app uses the pgxpool. Goose's API requires
// database/sql.
//
// CONCURRENCY (#574): callers can race. The serving process migrates
// at boot, and `aa seed` now migrates too — in CI both run against the
// same database within seconds of each other, and a multi-replica
// deployment has the same shape. Two unsynchronised goose runs on a
// fresh database both see version 0 pending, both try to apply 00001,
// and the loser fails on `relation "user" already exists`.
//
// So this goes through goose's Provider with a PostgreSQL session-level
// advisory lock rather than the bare UpContext. The loser blocks until
// the winner commits, then reads the version table, finds everything
// applied, and returns having done nothing. Using goose's own locker
// rather than hand-rolling pg_advisory_lock keeps the retry/timeout
// behaviour library-tested (5s probe, 5 min ceiling — comfortably
// above our migration runtime).
func Migrate(ctx context.Context, cfg config.Config) error {
	dsn := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword, cfg.DBName, cfg.DBSSLMode,
	)

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("migrate: open: %w", err)
	}
	defer sqlDB.Close()

	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("migrate: ping: %w", err)
	}

	// Provider reads migrations from the ROOT of the fs it's given,
	// where the old SetBaseFS+UpContext pair took a "migrations" dir
	// argument — hence the Sub.
	migrations, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("migrate: sub fs: %w", err)
	}

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("migrate: locker: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations,
		goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("migrate: provider: %w", err)
	}

	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("migrate: up: %w", err)
	}
	return nil
}
