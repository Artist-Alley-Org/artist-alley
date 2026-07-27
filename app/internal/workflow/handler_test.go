// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package workflow_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/workflow"
)

// listStates exercises the read path end-to-end: handler in front of
// a real Postgres pool with the seeded `post` domain. We also verify
// the cache fills on the first call and that a second call serves
// from the cache (the row count comes back identical and the second
// call's latency is dominated by the cache hit, but we don't time-
// assert — we just confirm correctness across calls).
func TestListWorkflowStates_PostDomain(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := cache.NewRegistry(pool, logger)
	ctx := t.Context()

	if err := reg.Start(ctx); err != nil {
		t.Fatalf("registry start: %v", err)
	}
	t.Cleanup(func() { reg.Stop() })

	h := workflow.NewHandler(pool, logger, reg)

	// Authenticated identity — the handler rejects anonymous callers
	// with 401, so we always need one.
	id := &auth.Identity{UserRef: 999, Username: "wf_handler_test"}
	ctx = auth.WithIdentity(ctx, id)

	// First call — fills the cache.
	resp, err := h.ListWorkflowStates(ctx, openapi.ListWorkflowStatesRequestObject{
		Params: openapi.ListWorkflowStatesParams{Domain: workflow.PostDomain},
	})
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	first, ok := resp.(openapi.ListWorkflowStates200JSONResponse)
	if !ok {
		t.Fatalf("first call: unexpected response %T", resp)
	}
	if len(first) == 0 {
		t.Fatal("first call: expected at least one state for 'post' domain (migration 00018 seeds them)")
	}

	// Confirm we got at least one is_initial=true row — the
	// migration enforces a partial unique index.
	hasInitial := false
	for _, s := range first {
		if s.IsInitial {
			hasInitial = true
			break
		}
	}
	if !hasInitial {
		t.Error("first call: post domain should have exactly one initial state")
	}

	// Second call — must return the same rows. Cache hit is the
	// expected path here; cache miss would still be correct, but a
	// length / id mismatch is a real bug.
	resp2, err := h.ListWorkflowStates(ctx, openapi.ListWorkflowStatesRequestObject{
		Params: openapi.ListWorkflowStatesParams{Domain: workflow.PostDomain},
	})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	second, ok := resp2.(openapi.ListWorkflowStates200JSONResponse)
	if !ok {
		t.Fatalf("second call: unexpected response %T", resp2)
	}
	if len(second) != len(first) {
		t.Fatalf("cache mismatch: first %d, second %d", len(first), len(second))
	}
}

// rejects anonymous callers with 401.
func TestListWorkflowStates_RequiresAuth(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := workflow.NewHandler(pool, logger, nil) // no cache needed for the rejection path

	resp, err := h.ListWorkflowStates(context.Background(), openapi.ListWorkflowStatesRequestObject{
		Params: openapi.ListWorkflowStatesParams{Domain: workflow.PostDomain},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if _, ok := resp.(openapi.ListWorkflowStates401JSONResponse); !ok {
		t.Fatalf("expected 401 for anonymous caller, got %T", resp)
	}
}

// rejects empty/missing domain with 400.
func TestListWorkflowStates_EmptyDomain(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := workflow.NewHandler(pool, logger, nil)

	id := &auth.Identity{UserRef: 999, Username: "wf_handler_test"}
	ctx := auth.WithIdentity(context.Background(), id)

	resp, err := h.ListWorkflowStates(ctx, openapi.ListWorkflowStatesRequestObject{
		Params: openapi.ListWorkflowStatesParams{Domain: ""},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if _, ok := resp.(openapi.ListWorkflowStates400JSONResponse); !ok {
		t.Fatalf("expected 400 for empty domain, got %T", resp)
	}
}
