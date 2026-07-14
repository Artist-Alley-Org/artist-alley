// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Binary aa-migrate applies the embedded goose migrations to the
// database named by AA_DB_NAME, then exits. It is the migrate-only
// slice of the server's boot sequence (see db.Migrate) with none of
// the runtime dependencies — no AA_MASTER_KEY, no AA_SCRAMBLE_KEY, no
// HTTP server.
//
// Its reason for existing is scripts/test.sh (#291): the Go suite runs
// against a disposable artist_alley_test database, and this command
// brings that freshly-created database up to schema using the exact
// same embedded migrations + goose configuration the app uses at boot,
// so there is no risk of the test schema drifting from production.
//
// Reads AA_DB_HOST / AA_DB_PORT / AA_DB_USER / AA_DB_PASSWORD /
// AA_DB_NAME / AA_DB_SSLMODE straight from the environment (the same
// vars the server reads) and does not go through config.Load, which
// would demand runtime-only secrets a migration doesn't need.
package main

import (
	"context"
	"log"
	"os"
	"strconv"

	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/db"
)

func main() {
	port, err := strconv.Atoi(env("AA_DB_PORT", "5432"))
	if err != nil {
		log.Fatalf("aa-migrate: invalid AA_DB_PORT: %v", err)
	}
	cfg := config.Config{
		DBHost:     env("AA_DB_HOST", "postgres"),
		DBPort:     port,
		DBUser:     env("AA_DB_USER", "artist_alley"),
		DBPassword: os.Getenv("AA_DB_PASSWORD"),
		DBName:     env("AA_DB_NAME", "artist_alley"),
		DBSSLMode:  env("AA_DB_SSLMODE", "disable"),
	}
	if cfg.DBPassword == "" {
		log.Fatal("aa-migrate: AA_DB_PASSWORD is required")
	}
	if err := db.Migrate(context.Background(), cfg); err != nil {
		log.Fatalf("aa-migrate: %v", err)
	}
	log.Printf("aa-migrate: migrations applied to %q", cfg.DBName)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
