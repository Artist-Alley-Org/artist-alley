// Package config loads runtime configuration from environment variables.
//
// All artist-alley configuration is exposed via env vars (12-factor style).
// Defaults are baked in so the binary boots in a sane state without any env
// set; anything sensitive must be provided explicitly.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds every knob the app reads. Add a field here when you need a
// new env var; do not call [os.Getenv] from elsewhere in the codebase.
type Config struct {
	// HTTP listener.
	HTTPAddr string

	// PostgreSQL connection.
	DBHost     string
	DBPort     int
	DBUser     string
	DBPassword string
	DBName     string
	DBSSLMode  string // disable / require / verify-full

	// Pool tuning.
	DBMaxConns        int32
	DBMinConns        int32
	DBConnMaxLifetime time.Duration

	// Logging.
	LogLevel  string // debug / info / warn / error
	LogFormat string // json (default) or text

	// Legacy PHP-FPM upstream, used by the nginx layer for unported
	// routes. The Go app currently does not proxy fcgi itself; this
	// value is here so future ports can introspect or rewrite it.
	LegacyPHPAddr string
}

// Load reads the environment into a Config, applying defaults and
// reporting an error if anything required is missing.
func Load() (Config, error) {
	c := Config{
		HTTPAddr:          envStr("AA_HTTP_ADDR", ":8080"),
		DBHost:            envStr("AA_DB_HOST", "postgres"),
		DBPort:            envInt("AA_DB_PORT", 5432),
		DBUser:            envStr("AA_DB_USER", "artist_alley"),
		DBPassword:        envStr("AA_DB_PASSWORD", ""),
		DBName:            envStr("AA_DB_NAME", "artist_alley"),
		DBSSLMode:         envStr("AA_DB_SSLMODE", "disable"),
		DBMaxConns:        int32(envInt("AA_DB_MAX_CONNS", 20)),
		DBMinConns:        int32(envInt("AA_DB_MIN_CONNS", 2)),
		DBConnMaxLifetime: envDuration("AA_DB_CONN_MAX_LIFETIME", time.Hour),
		LogLevel:          envStr("AA_LOG_LEVEL", "info"),
		LogFormat:         envStr("AA_LOG_FORMAT", "json"),
		LegacyPHPAddr:     envStr("AA_LEGACY_PHP_ADDR", "php:9000"),
	}

	if c.DBPassword == "" {
		return c, errors.New("config: AA_DB_PASSWORD is required")
	}
	return c, nil
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		panic(fmt.Sprintf("config: %s=%q is not a valid int", key, v))
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		panic(fmt.Sprintf("config: %s=%q is not a valid duration", key, v))
	}
	return d
}
