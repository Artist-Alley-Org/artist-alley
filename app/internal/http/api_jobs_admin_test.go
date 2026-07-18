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
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
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
