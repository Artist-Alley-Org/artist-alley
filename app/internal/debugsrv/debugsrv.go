// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package debugsrv serves net/http/pprof on a dedicated, opt-in listener.
//
// # Why this is not on the main mux
//
// pprof is a genuine data-exfiltration surface, not merely an ops nicety.
// A heap profile contains live object contents: session tokens, password
// hashes in flight, decoded file bytes, DB rows. /debug/pprof/cmdline
// leaks argv, and the CPU and trace endpoints will stall the process for
// as long as the caller asks. Any of that on the public router is one
// routing mistake or one auth-middleware regression away from being
// world-readable, and it would not show up as a failing test.
//
// So the gate is structural rather than a permission check:
//
//   - OFF unless AA_PPROF_ADDR is explicitly set. No default port, no
//     "enabled in dev" special case that can ship by accident.
//   - A separate net.Listener and a separate http.ServeMux. The profile
//     endpoints are never registered on the application router, so no
//     change to API middleware can expose them.
//   - No compose file publishes the port. Reaching it means
//     `docker compose exec` into the container, which already implies
//     host access.
//   - Binding anywhere other than loopback logs a WARN naming the risk,
//     because 0.0.0.0 inside a container is reachable from every other
//     container on the compose network.
//
// The recommended value is 127.0.0.1:6060 — reachable from inside the
// container, unreachable from the network.
package debugsrv

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

// Server is an opt-in pprof listener with a lifecycle tied to the
// caller's context.
type Server struct {
	addr   string
	logger *slog.Logger
	srv    *http.Server
	// bound is the address actually listened on, which differs from
	// addr when the caller asked for port 0.
	bound string
}

// BoundAddr returns the address the listener bound, empty before Start.
func (s *Server) BoundAddr() string {
	if s == nil {
		return ""
	}
	return s.bound
}

// New returns a Server, or nil when addr is empty — the disabled case,
// which is the default and is not an error. A nil *Server's methods are
// safe to call, so callers need no branch.
func New(addr string, logger *slog.Logger) *Server {
	if addr == "" {
		return nil
	}
	// Explicit registration on our OWN mux. Importing net/http/pprof
	// also registers these on http.DefaultServeMux as an import side
	// effect; we never serve DefaultServeMux, and doing this by hand
	// keeps it that way even if something else in the process starts
	// one.
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	return &Server{
		addr:   addr,
		logger: logger,
		srv: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
			// No WriteTimeout: /debug/pprof/profile?seconds=30 and
			// trace are long-lived by design and a write deadline
			// would truncate exactly the profile being collected.
		},
	}
}

// Enabled reports whether a profiling listener is configured.
func (s *Server) Enabled() bool { return s != nil }

// Start binds the listener and serves in a background goroutine,
// shutting down when ctx is cancelled. Binding synchronously means a
// port clash surfaces as a boot error rather than a silent no-op that
// leaves the operator waiting on a port nothing is listening to.
func (s *Server) Start(ctx context.Context) error {
	if s == nil {
		return nil
	}
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.bound = ln.Addr().String()

	level := slog.LevelInfo
	msg := "pprof.listener.start"
	if !isLoopback(s.addr) {
		level = slog.LevelWarn
		msg = "pprof.listener.start_non_loopback"
	}
	s.logger.LogAttrs(ctx, level, msg,
		slog.String("addr", ln.Addr().String()),
		slog.String("warning", "pprof exposes heap contents (tokens, file bytes, DB rows); bind loopback only"),
	)

	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.LogAttrs(context.Background(), slog.LevelError, "pprof.listener.error",
				slog.String("err", err.Error()))
		}
	}()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
	}()
	return nil
}

// isLoopback reports whether addr binds only the loopback interface.
// An unparseable or hostname-based address is treated as NOT loopback —
// the conservative direction, since the cost of being wrong is a missing
// warning on an exposed profiler.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
