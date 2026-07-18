// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #400 (v0.4.0 Sprint 0) — the jobs admin read surface is gated on
// system.jobs.read (or system.admin) and is strictly read-only. This
// encodes the gate contract for all three GETs: read-cap => 200, cap
// absent => 403, no identity => 401.
//
// Real Postgres; skips without AA_DB_PASSWORD, same convention as the
// other integration suites in this package. The queries run against the
// (possibly empty) jobs table — an empty queue is a valid 200.

package http

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

func openPoolForJobs(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	dsn := "host=" + envOrSens("AA_DB_HOST", "postgres") +
		" port=" + envOrSens("AA_DB_PORT", "5432") +
		" user=" + envOrSens("AA_DB_USER", "artist_alley") +
		" dbname=" + envOrSens("AA_DB_NAME", "artist_alley") +
		" sslmode=disable password=" + pwd
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	return pool
}

func withCaps(caps ...string) context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{UserRef: 1, Capabilities: caps})
}

func TestJobsAdmin_CapGate(t *testing.T) {
	pool := openPoolForJobs(t)
	defer pool.Close()
	s := &apiServer{jobsAdmin: jobs.NewAdminHandler(pool)}

	readCtx := withCaps(jobs.CapJobsRead)  // read-only auditor
	adminCtx := withCaps("system.admin")   // wildcards everything
	noCapCtx := withCaps("some.other.cap") // authenticated, wrong cap
	anonCtx := context.Background()        // no identity

	t.Run("listJobs", func(t *testing.T) {
		if _, ok := mustListJobs(t, s, readCtx).(openapi.ListJobs200JSONResponse); !ok {
			t.Error("read cap: want 200")
		}
		if _, ok := mustListJobs(t, s, adminCtx).(openapi.ListJobs200JSONResponse); !ok {
			t.Error("system.admin: want 200")
		}
		if _, ok := mustListJobs(t, s, noCapCtx).(openapi.ListJobs403JSONResponse); !ok {
			t.Error("wrong cap: want 403")
		}
		if _, ok := mustListJobs(t, s, anonCtx).(openapi.ListJobs401JSONResponse); !ok {
			t.Error("no identity: want 401")
		}
	})

	t.Run("listJobWorkers", func(t *testing.T) {
		if _, ok := mustListWorkers(t, s, readCtx).(openapi.ListJobWorkers200JSONResponse); !ok {
			t.Error("read cap: want 200")
		}
		if _, ok := mustListWorkers(t, s, noCapCtx).(openapi.ListJobWorkers403JSONResponse); !ok {
			t.Error("wrong cap: want 403")
		}
		if _, ok := mustListWorkers(t, s, anonCtx).(openapi.ListJobWorkers401JSONResponse); !ok {
			t.Error("no identity: want 401")
		}
	})

	t.Run("listJobStatusCounts", func(t *testing.T) {
		if _, ok := mustStatusCounts(t, s, readCtx).(openapi.ListJobStatusCounts200JSONResponse); !ok {
			t.Error("read cap: want 200")
		}
		if _, ok := mustStatusCounts(t, s, noCapCtx).(openapi.ListJobStatusCounts403JSONResponse); !ok {
			t.Error("wrong cap: want 403")
		}
		if _, ok := mustStatusCounts(t, s, anonCtx).(openapi.ListJobStatusCounts401JSONResponse); !ok {
			t.Error("no identity: want 401")
		}
	})
}

func mustListJobs(t *testing.T, s *apiServer, ctx context.Context) openapi.ListJobsResponseObject {
	t.Helper()
	r, err := s.ListJobs(ctx, openapi.ListJobsRequestObject{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	return r
}

func mustListWorkers(t *testing.T, s *apiServer, ctx context.Context) openapi.ListJobWorkersResponseObject {
	t.Helper()
	r, err := s.ListJobWorkers(ctx, openapi.ListJobWorkersRequestObject{})
	if err != nil {
		t.Fatalf("ListJobWorkers: %v", err)
	}
	return r
}

func mustStatusCounts(t *testing.T, s *apiServer, ctx context.Context) openapi.ListJobStatusCountsResponseObject {
	t.Helper()
	r, err := s.ListJobStatusCounts(ctx, openapi.ListJobStatusCountsRequestObject{})
	if err != nil {
		t.Fatalf("ListJobStatusCounts: %v", err)
	}
	return r
}

// #401 (v0.4.0 Sprint 1) — the MUTATING actions gate on system.admin,
// the new reads (scheduled, concurrency) on system.jobs.read. And a
// running job must never be requeued/cancelled out from under its
// worker (WHERE-clause guard => 409, no row touched).

func TestJobsAdmin_ActionGate(t *testing.T) {
	pool := openPoolForJobs(t)
	defer pool.Close()
	s := &apiServer{
		jobsAdmin: jobs.NewAdminHandler(pool),
		jobsSvc:   jobs.NewService(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), jobs.NewRegistry()),
		sysCfg:    sysconfig.NewStore(pool),
	}
	readCtx := withCaps(jobs.CapJobsRead)
	adminCtx := withCaps("system.admin")
	anonCtx := context.Background()
	someID := openapi.RequeueJobRequestObject{Id: uuid.New()}

	// --- reads: jobs.read => 200, anon => 401 -----------------------
	t.Run("scheduled read", func(t *testing.T) {
		if r, _ := s.ListScheduledJobs(readCtx, openapi.ListScheduledJobsRequestObject{}); !is200Sched(r) {
			t.Errorf("read cap: want 200, got %T", r)
		}
		if r, _ := s.ListScheduledJobs(anonCtx, openapi.ListScheduledJobsRequestObject{}); !is401Sched(r) {
			t.Errorf("anon: want 401, got %T", r)
		}
	})
	t.Run("concurrency read", func(t *testing.T) {
		if r, _ := s.ListJobConcurrency(readCtx, openapi.ListJobConcurrencyRequestObject{}); !is200Conc(r) {
			t.Errorf("read cap: want 200, got %T", r)
		}
	})

	// --- mutations: read-cap => 403 (the whole point of the split) --
	t.Run("requeue forbidden for read cap", func(t *testing.T) {
		r, _ := s.RequeueJob(readCtx, someID)
		if _, ok := r.(openapi.RequeueJob403JSONResponse); !ok {
			t.Errorf("read cap requeue: want 403, got %T", r)
		}
	})
	t.Run("cancel forbidden for read cap", func(t *testing.T) {
		r, _ := s.CancelJob(readCtx, openapi.CancelJobRequestObject{Id: someID.Id})
		if _, ok := r.(openapi.CancelJob403JSONResponse); !ok {
			t.Errorf("read cap cancel: want 403, got %T", r)
		}
	})
	t.Run("set-concurrency forbidden for read cap", func(t *testing.T) {
		body := openapi.SetJobConcurrency{Cap: 2}
		r, _ := s.SetJobConcurrency(readCtx, openapi.SetJobConcurrencyRequestObject{Type: "preview.raster", Body: &body})
		if _, ok := r.(openapi.SetJobConcurrency403JSONResponse); !ok {
			t.Errorf("read cap set-concurrency: want 403, got %T", r)
		}
	})

	// --- admin actions succeed / classify correctly -----------------
	t.Run("admin requeue missing => 404", func(t *testing.T) {
		r, _ := s.RequeueJob(adminCtx, someID)
		if _, ok := r.(openapi.RequeueJob404JSONResponse); !ok {
			t.Errorf("admin requeue missing: want 404, got %T", r)
		}
	})
	t.Run("admin set-concurrency => 204", func(t *testing.T) {
		body := openapi.SetJobConcurrency{Cap: 3}
		r, _ := s.SetJobConcurrency(adminCtx, openapi.SetJobConcurrencyRequestObject{Type: "test.jobtype.s1", Body: &body})
		if _, ok := r.(openapi.SetJobConcurrency204Response); !ok {
			t.Errorf("admin set-concurrency: want 204, got %T", r)
		}
		// clean up the config row we just wrote
		_ = s.sysCfg.SetJobTypeConcurrency(context.Background(), "test.jobtype.s1", 0)
	})
	t.Run("set-concurrency out of range => 400", func(t *testing.T) {
		body := openapi.SetJobConcurrency{Cap: 999}
		r, _ := s.SetJobConcurrency(adminCtx, openapi.SetJobConcurrencyRequestObject{Type: "x", Body: &body})
		if _, ok := r.(openapi.SetJobConcurrency400JSONResponse); !ok {
			t.Errorf("cap 999: want 400, got %T", r)
		}
	})
}

// TestJobsAdmin_RequeueCancelGuards proves the WHERE-clause guards: a
// failed job requeues (204) and lands back pending; a RUNNING job can
// be neither requeued nor cancelled (409, row untouched).
func TestJobsAdmin_RequeueCancelGuards(t *testing.T) {
	pool := openPoolForJobs(t)
	defer pool.Close()
	s := &apiServer{jobsAdmin: jobs.NewAdminHandler(pool)}
	adminCtx := withCaps("system.admin")
	ctx := context.Background()

	insert := func(status, worker string) uuid.UUID {
		var id uuid.UUID
		var claimedBy any
		if worker != "" {
			claimedBy = worker
		}
		err := pool.QueryRow(ctx, `
			INSERT INTO jobs (type, payload, status, priority, attempts, max_attempts,
			                  claimed_by, lease_expires_at, enqueued_at)
			VALUES ('test.s1.guard','{}',$1,100,1,3,$2::TEXT,
			        CASE WHEN $2::TEXT IS NULL THEN NULL ELSE NOW()+INTERVAL '1m' END, NOW())
			RETURNING id`, status, claimedBy).Scan(&id)
		if err != nil {
			t.Fatalf("insert %s job: %v", status, err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE id=$1`, id) })
		return id
	}
	statusOf := func(id uuid.UUID) string {
		var st string
		_ = pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, id).Scan(&st)
		return st
	}

	t.Run("failed job requeues to pending", func(t *testing.T) {
		id := insert("failed", "")
		r, _ := s.RequeueJob(adminCtx, openapi.RequeueJobRequestObject{Id: id})
		if _, ok := r.(openapi.RequeueJob204Response); !ok {
			t.Fatalf("requeue failed: want 204, got %T", r)
		}
		if st := statusOf(id); st != "pending" {
			t.Errorf("after requeue: status=%q want pending", st)
		}
	})

	t.Run("running job cannot be requeued (409, untouched)", func(t *testing.T) {
		id := insert("running", "aa://guard/w1")
		r, _ := s.RequeueJob(adminCtx, openapi.RequeueJobRequestObject{Id: id})
		if _, ok := r.(openapi.RequeueJob409JSONResponse); !ok {
			t.Fatalf("requeue running: want 409, got %T", r)
		}
		if st := statusOf(id); st != "running" {
			t.Errorf("running job corrupted by requeue: status=%q", st)
		}
	})

	t.Run("running job cannot be cancelled (409, untouched)", func(t *testing.T) {
		id := insert("running", "aa://guard/w2")
		r, _ := s.CancelJob(adminCtx, openapi.CancelJobRequestObject{Id: id})
		if _, ok := r.(openapi.CancelJob409JSONResponse); !ok {
			t.Fatalf("cancel running: want 409, got %T", r)
		}
		if st := statusOf(id); st != "running" {
			t.Errorf("running job corrupted by cancel: status=%q", st)
		}
	})

	t.Run("pending job cancels to cancelled", func(t *testing.T) {
		id := insert("pending", "")
		r, _ := s.CancelJob(adminCtx, openapi.CancelJobRequestObject{Id: id})
		if _, ok := r.(openapi.CancelJob204Response); !ok {
			t.Fatalf("cancel pending: want 204, got %T", r)
		}
		if st := statusOf(id); st != "cancelled" {
			t.Errorf("after cancel: status=%q want cancelled", st)
		}
	})
}

func is200Sched(r openapi.ListScheduledJobsResponseObject) bool {
	_, ok := r.(openapi.ListScheduledJobs200JSONResponse)
	return ok
}
func is401Sched(r openapi.ListScheduledJobsResponseObject) bool {
	_, ok := r.(openapi.ListScheduledJobs401JSONResponse)
	return ok
}
func is200Conc(r openapi.ListJobConcurrencyResponseObject) bool {
	_, ok := r.(openapi.ListJobConcurrency200JSONResponse)
	return ok
}
