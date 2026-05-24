// Package http wires up the artist-alley HTTP server: middleware, routes,
// graceful shutdown.
package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/http/handlers"
	"github.com/mscrnt/artist-alley/app/internal/http/middleware"
)

// Server bundles the [http.Server] with its dependencies so the
// run-loop can shut it down cleanly.
type Server struct {
	cfg    config.Config
	logger *slog.Logger
	pool   *pgxpool.Pool
	srv    *http.Server
}

// New builds a Server with routes wired up but not yet listening.
// Call [Server.Run] to start serving.
func New(cfg config.Config, logger *slog.Logger, pool *pgxpool.Pool, version string) *Server {
	r := chi.NewRouter()

	// Order matters: RequestID first so it shows up in every later log.
	r.Use(middleware.RequestID)
	r.Use(middleware.Logger(logger))
	r.Use(middleware.Recover(logger))

	health := &handlers.Health{
		Pool:    pool,
		Version: version,
		Started: time.Now(),
	}
	r.Method(http.MethodGet, "/healthz", health)
	r.Method(http.MethodGet, "/readyz", health) // same handler for now

	// Future: r.Mount("/api/v1", apiv1.Router(...)) etc.

	return &Server{
		cfg:    cfg,
		logger: logger,
		pool:   pool,
		srv: &http.Server{
			Addr:              cfg.HTTPAddr,
			Handler:           r,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
	}
}

// Run starts the listener and blocks until ctx is cancelled or the
// server itself errors. On cancellation it performs a graceful shutdown
// bounded by 15 seconds.
func (s *Server) Run(ctx context.Context) error {
	listenErr := make(chan error, 1)
	go func() {
		s.logger.LogAttrs(ctx, slog.LevelInfo, "http.listen",
			slog.String("addr", s.cfg.HTTPAddr),
		)
		err := s.srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
			return
		}
		listenErr <- nil
	}()

	select {
	case err := <-listenErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		s.logger.LogAttrs(ctx, slog.LevelInfo, "http.shutdown.start", slog.Any("cause", ctx.Err()))
		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			s.logger.LogAttrs(ctx, slog.LevelError, "http.shutdown.error", slog.Any("err", err))
			return err
		}
		s.logger.LogAttrs(ctx, slog.LevelInfo, "http.shutdown.done")
		return nil
	}
}
