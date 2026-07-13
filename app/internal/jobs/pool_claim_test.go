// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Regression test for the v0.1.0 processing outage: an unrestricted
// worker pool (Types == nil) wired with a non-nil per-type gate never
// claimed any job, because tryReserve(nil) returns nil and the guard
// backed the worker off on every poll. Every preview.*, ai.*, and
// metadata.extract job sat `pending` forever.
//
// This drives a real Pool end-to-end against the compose postgres:
// enqueue one job, start a nil-Types pool, and assert the row reaches
// `done`. It FAILS on the pre-fix code (the job never leaves pending)
// and passes once the guard only gates type-restricted workers.
package jobs_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// ranHandler records that Handle was invoked and reports success.
type ranHandler struct {
	typ jobs.JobType
	ran atomic.Bool
}

func (h *ranHandler) Type() jobs.JobType { return h.typ }

func (h *ranHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	h.ran.Store(true)
	return json.RawMessage(`{"ok":true}`), nil
}

// TestPool_NilTypes_ClaimsAndRunsJob is the regression guard.
func TestPool_NilTypes_ClaimsAndRunsJob(t *testing.T) {
	pool := openTestPool(t)
	defer pool.Close()

	clean := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM jobs WHERE type = 'claim_test_unrestricted'`)
	}
	clean()
	t.Cleanup(clean)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	h := &ranHandler{typ: jobs.JobType("claim_test_unrestricted")}
	reg := jobs.NewRegistry()
	reg.Register(h)

	svc := jobs.NewService(pool, logger, reg)

	jobID, err := svc.Enqueue(context.Background(), h.typ, map[string]any{"n": 1}, jobs.EnqueueOpts{})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// The production wiring: an unrestricted pool (Types == nil) whose
	// workers are gated by the Pool itself. This is exactly what
	// server.go builds.
	p := &jobs.Pool{
		Service: svc,
		Logger:  logger,
		Size:    1,
		Types:   nil,
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx, "claimtest")
	t.Cleanup(func() { cancel(); p.Stop() })

	// First claim is immediate (Run claims before any poll sleep), so
	// `done` should arrive well under a second; 15s covers slow CI.
	deadline := time.Now().Add(15 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		j, err := svc.GetByID(context.Background(), jobID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		last = j.Status
		if j.Status == "done" {
			if !h.ran.Load() {
				t.Fatalf("job reached done but handler never ran")
			}
			return // success
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("unrestricted pool never drained the job: status stuck at %q after 15s (want %q)", last, "done")
}
