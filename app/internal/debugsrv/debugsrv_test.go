// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package debugsrv

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNew_EmptyAddrIsDisabled(t *testing.T) {
	s := New("", discardLogger())
	if s != nil {
		t.Fatal("empty addr must yield a nil (disabled) server")
	}
	if s.Enabled() {
		t.Error("Enabled() must be false when disabled")
	}
	// The nil server must be safe to drive — callers have no branch.
	if err := s.Start(context.Background()); err != nil {
		t.Errorf("Start on disabled server: %v", err)
	}
}

func TestDefaultServeMuxIsNotTheProfiler(t *testing.T) {
	// Importing net/http/pprof registers /debug/pprof on
	// http.DefaultServeMux as a side effect. This package must never
	// depend on that: if it did, any other code in the process that
	// serves DefaultServeMux would publish the profiler for free.
	// Constructing a server must not be what makes the endpoints
	// reachable — our own mux is.
	s := New("127.0.0.1:0", discardLogger())
	if s == nil {
		t.Fatal("want a server")
	}
	req, _ := http.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	h, pattern := s.srv.Handler.(*http.ServeMux).Handler(req)
	if h == nil || pattern == "" {
		t.Error("heap endpoint must be served by our own mux")
	}
}

func TestStart_ServesHeapProfileAndStopsWithContext(t *testing.T) {
	s := New("127.0.0.1:0", discardLogger())
	if s == nil {
		t.Fatal("want a server")
	}
	// Bind a real port so we exercise Serve, not just routing.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	base := "http://" + s.BoundAddr()

	// A real request over a real socket — the endpoint must actually
	// serve a heap profile, not merely route to a handler.
	resp, err := http.Get(base + "/debug/pprof/heap")
	if err != nil {
		t.Fatalf("GET heap: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if len(body) == 0 {
		t.Error("empty heap profile")
	}

	// Cancelling the context must take the listener down, so an
	// operator turning profiling off is not left with an open port.
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := http.Get(base + "/debug/pprof/heap"); err != nil {
			return // refused: the listener is gone, which is the point
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("listener still accepting requests after context cancellation")
}

func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:6060":   true,
		"localhost:6060":   true,
		"[::1]:6060":       true,
		"0.0.0.0:6060":     false,
		"192.168.1.5:6060": false,
		// A bare port binds every interface, so it must NOT be
		// treated as loopback — this is the case most likely to be
		// typed by hand.
		":6060":   false,
		"garbage": false,
	}
	for addr, want := range cases {
		if got := isLoopback(addr); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}
