package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx as a database/sql driver, needed by goose
	"github.com/pressly/goose/v3"

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

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("migrate: set dialect: %w", err)
	}
	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		return fmt.Errorf("migrate: up: %w", err)
	}
	return nil
}
