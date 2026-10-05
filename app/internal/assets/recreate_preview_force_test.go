// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assets_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------------------
// #760 — what the endpoint actually QUEUES.
//
// recreate_preview_test.go pins the pre-DB guards and says the happy
// path "lives in the integration tests". It did not: nothing anywhere
// asserted on the enqueued job, which is how the endpoint spent three
// releases returning 202 for a job that could not change anything.
//
// A response assertion would not have caught it either — the 202 was
// always correct. The payload is the only place the difference exists.
// ---------------------------------------------------------------------------

func TestRecreateAssetPreview_EnqueuesForcedJobByDefault(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	const ownerRef int64 = 9_142_760
	const hash = "760abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456"
	assetID := seedBridgeAsset(t, pool, ownerRef, "recreate force", hash)
	cleanupBridgeAsset(t, pool, assetID)

	h := &assets.Handler{
		Pool:   pool,
		Jobs:   jobs.NewService(pool, slog.Default(), nil),
		Logger: slog.Default(),
	}
	ctx := auth.WithIdentity(t.Context(), &auth.Identity{UserRef: ownerRef})

	// No force parameter: the operator clicked "Recreate previews".
	resp, err := h.RecreateAssetPreview(ctx, openapi.RecreateAssetPreviewRequestObject{
		Id: openapi_types.UUID(assetID),
	})
	if err != nil {
		t.Fatalf("RecreateAssetPreview: %v", err)
	}
	accepted, ok := resp.(openapi.RecreateAssetPreview202JSONResponse)
	if !ok {
		t.Fatalf("expected 202, got %T", resp)
	}
	if !accepted.Force {
		t.Error("202 body says force=false for a bare recreate request")
	}
	if got := jobForce(t, pool, uuid.UUID(accepted.JobId)); !got {
		t.Fatal("the enqueued job does not carry force: the worker will skip every " +
			"variant that already exists and the preview will not change (#760)")
	}

	// force=false is still reachable — the cheap gap-filling pass.
	no := false
	resp, err = h.RecreateAssetPreview(ctx, openapi.RecreateAssetPreviewRequestObject{
		Id:     openapi_types.UUID(assetID),
		Params: openapi.RecreateAssetPreviewParams{Force: &no},
	})
	if err != nil {
		t.Fatalf("RecreateAssetPreview(force=false): %v", err)
	}
	accepted, ok = resp.(openapi.RecreateAssetPreview202JSONResponse)
	if !ok {
		t.Fatalf("expected 202, got %T", resp)
	}
	if accepted.Force {
		t.Error("202 body says force=true for an explicit force=false request")
	}
	if jobForce(t, pool, uuid.UUID(accepted.JobId)) {
		t.Error("force=false enqueued a forced job")
	}
}

// jobForce reads the `force` flag out of a queued job's payload.
func jobForce(t *testing.T, pool *pgxpool.Pool, jobID uuid.UUID) bool {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT payload FROM jobs WHERE id = $1`, jobID).Scan(&raw); err != nil {
		t.Fatalf("read job payload: %v", err)
	}
	var p struct {
		Force bool `json:"force"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode job payload: %v", err)
	}
	return p.Force
}
