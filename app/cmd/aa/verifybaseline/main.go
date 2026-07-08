// Binary verifybaseline runs db.Migrate against a scratch Postgres
// pointed at by AA_DB_* env, then prints the applied head version_id.
// Only invoked by scripts/verify-baseline.sh — it exists to exercise
// the SAME goose invocation path the real Migrate function uses (so
// an embed-FS bug or a goose-dialect edge case surfaces here identically
// to how it would in production boot).
//
// See §4.21 of docs/v0_1_readiness.md.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/db"
)

func main() {
	ctx := context.Background()
	cfg := config.Config{
		DBHost:     envOr("AA_DB_HOST", "127.0.0.1"),
		DBPort:     mustAtoi(envOr("AA_DB_PORT", "5432")),
		DBUser:     envOr("AA_DB_USER", "verify"),
		DBPassword: envOr("AA_DB_PASSWORD", "verify"),
		DBName:     envOr("AA_DB_NAME", "verify"),
		DBSSLMode:  envOr("AA_DB_SSLMODE", "disable"),
	}

	if err := db.Migrate(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "verify-baseline: migrate: %v\n", err)
		os.Exit(1)
	}

	// Print the applied head so the calling shell can echo it. Uses
	// a fresh raw sql.DB rather than pgxpool to keep this binary
	// dependency-light.
	dsn := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword, cfg.DBName, cfg.DBSSLMode,
	)
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify-baseline: open: %v\n", err)
		os.Exit(1)
	}
	defer sqlDB.Close()

	var head int64
	if err := sqlDB.QueryRowContext(ctx, `SELECT MAX(version_id) FROM goose_db_version`).Scan(&head); err != nil {
		fmt.Fprintf(os.Stderr, "verify-baseline: read head: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(head)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustAtoi(s string) int {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		fmt.Fprintf(os.Stderr, "verify-baseline: bad int %q: %v\n", s, err)
		os.Exit(1)
	}
	return n
}
