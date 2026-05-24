// Package handlers contains route handlers for the Go server.
//
// Each handler is a method on a struct that owns its dependencies
// (DB pool, logger, config, etc.). Keep them small — the heavy lifting
// belongs in domain packages.
package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Health responds to readiness/liveness probes.
type Health struct {
	Pool    *pgxpool.Pool
	Version string
	Started time.Time
}

type healthResponse struct {
	Status   string        `json:"status"`
	Version  string        `json:"version"`
	Uptime   time.Duration `json:"uptime_ns"`
	UptimeS  string        `json:"uptime"`
	Database string        `json:"database"`
}

// ServeHTTP implements [http.Handler]. Returns 200 if the database
// answers a ping within a short timeout, 503 otherwise.
func (h *Health) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	resp := healthResponse{
		Status:   "ok",
		Version:  h.Version,
		Uptime:   time.Since(h.Started),
		UptimeS:  time.Since(h.Started).Truncate(time.Second).String(),
		Database: "ok",
	}
	code := http.StatusOK

	if err := h.Pool.Ping(ctx); err != nil {
		resp.Status = "degraded"
		resp.Database = "unreachable: " + err.Error()
		code = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}
