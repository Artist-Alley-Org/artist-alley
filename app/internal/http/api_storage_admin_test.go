// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #402 (v0.4.0 Sprint 2) — the storage admin read surface is gated on
// system.storage.read (or system.admin) and is strictly read-only. This
// encodes the gate contract for both GETs: read-cap => 200, cap absent
// => 403, no identity => 401. It also pins the byte-accounting
// invariant that made this surface worth a dedicated query (originals
// live inside storage_variants, so totals must not add storage_objects).
//
// Real Postgres; skips without AA_DB_PASSWORD, same convention as the
// other integration suites in this package. An empty storage table is a
// valid 200 — the assertions below are all invariants that hold at zero.

package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

func newStorageAdminServer(t *testing.T) *apiServer {
	t.Helper()
	pool := openPoolForJobs(t) // shared integration-pool helper
	t.Cleanup(pool.Close)
	return &apiServer{storageAdmin: storage.NewAdminHandler(pool)}
}

func TestStorageAdmin_CapGate(t *testing.T) {
	s := newStorageAdminServer(t)

	t.Run("read cap gets usage", func(t *testing.T) {
		resp, err := s.GetStorageUsage(withCaps(storage.CapStorageRead), openapi.GetStorageUsageRequestObject{})
		if err != nil {
			t.Fatalf("GetStorageUsage: %v", err)
		}
		if _, ok := resp.(openapi.GetStorageUsage200JSONResponse); !ok {
			t.Fatalf("want 200, got %T", resp)
		}
	})

	t.Run("read cap gets variant families", func(t *testing.T) {
		resp, err := s.ListStorageVariantFamilies(withCaps(storage.CapStorageRead), openapi.ListStorageVariantFamiliesRequestObject{})
		if err != nil {
			t.Fatalf("ListStorageVariantFamilies: %v", err)
		}
		if _, ok := resp.(openapi.ListStorageVariantFamilies200JSONResponse); !ok {
			t.Fatalf("want 200, got %T", resp)
		}
	})

	t.Run("system.admin wildcard opens both", func(t *testing.T) {
		ctx := withCaps("system.admin")
		u, err := s.GetStorageUsage(ctx, openapi.GetStorageUsageRequestObject{})
		if err != nil {
			t.Fatalf("GetStorageUsage: %v", err)
		}
		if _, ok := u.(openapi.GetStorageUsage200JSONResponse); !ok {
			t.Fatalf("usage: want 200, got %T", u)
		}
		v, err := s.ListStorageVariantFamilies(ctx, openapi.ListStorageVariantFamiliesRequestObject{})
		if err != nil {
			t.Fatalf("ListStorageVariantFamilies: %v", err)
		}
		if _, ok := v.(openapi.ListStorageVariantFamilies200JSONResponse); !ok {
			t.Fatalf("families: want 200, got %T", v)
		}
	})

	t.Run("unrelated cap is forbidden on both", func(t *testing.T) {
		ctx := withCaps("system.jobs.read")
		u, err := s.GetStorageUsage(ctx, openapi.GetStorageUsageRequestObject{})
		if err != nil {
			t.Fatalf("GetStorageUsage: %v", err)
		}
		if _, ok := u.(openapi.GetStorageUsage403JSONResponse); !ok {
			t.Fatalf("usage: want 403, got %T", u)
		}
		v, err := s.ListStorageVariantFamilies(ctx, openapi.ListStorageVariantFamiliesRequestObject{})
		if err != nil {
			t.Fatalf("ListStorageVariantFamilies: %v", err)
		}
		if _, ok := v.(openapi.ListStorageVariantFamilies403JSONResponse); !ok {
			t.Fatalf("families: want 403, got %T", v)
		}
	})

	t.Run("anonymous is unauthorized on both", func(t *testing.T) {
		ctx := context.Background() // no identity
		u, err := s.GetStorageUsage(ctx, openapi.GetStorageUsageRequestObject{})
		if err != nil {
			t.Fatalf("GetStorageUsage: %v", err)
		}
		if _, ok := u.(openapi.GetStorageUsage401JSONResponse); !ok {
			t.Fatalf("usage: want 401, got %T", u)
		}
		v, err := s.ListStorageVariantFamilies(ctx, openapi.ListStorageVariantFamiliesRequestObject{})
		if err != nil {
			t.Fatalf("ListStorageVariantFamilies: %v", err)
		}
		if _, ok := v.(openapi.ListStorageVariantFamilies401JSONResponse); !ok {
			t.Fatalf("families: want 401, got %T", v)
		}
	})
}

// TestStorageAdmin_ByteAccounting pins the invariant that the usage
// query was built around: storage_variants already holds every object's
// original, so total = originals + derivatives and never needs
// storage_objects added on top. If a future change starts summing
// storage_objects into the total, originals get counted twice and this
// fails.
func TestStorageAdmin_ByteAccounting(t *testing.T) {
	s := newStorageAdminServer(t)
	ctx := withCaps(storage.CapStorageRead)

	resp, err := s.GetStorageUsage(ctx, openapi.GetStorageUsageRequestObject{})
	if err != nil {
		t.Fatalf("GetStorageUsage: %v", err)
	}
	u, ok := resp.(openapi.GetStorageUsage200JSONResponse)
	if !ok {
		t.Fatalf("want 200, got %T", resp)
	}

	if got, want := u.OriginalBytes+u.DerivativeBytes, u.TotalBytes; got != want {
		t.Errorf("originals+derivatives = %d, want total_bytes = %d", got, want)
	}
	if u.OriginalBytes > u.TotalBytes {
		t.Errorf("original_bytes %d exceeds total_bytes %d", u.OriginalBytes, u.TotalBytes)
	}
	// Every object contributes exactly one `original` variant row, so
	// there can never be more objects than variant rows.
	if u.ObjectCount > u.VariantCount {
		t.Errorf("object_count %d exceeds variant_count %d", u.ObjectCount, u.VariantCount)
	}

	// The family rollup must account for exactly the same bytes as the
	// flat total — that is what makes the two tiles agree.
	fresp, err := s.ListStorageVariantFamilies(ctx, openapi.ListStorageVariantFamiliesRequestObject{})
	if err != nil {
		t.Fatalf("ListStorageVariantFamilies: %v", err)
	}
	families, ok := fresp.(openapi.ListStorageVariantFamilies200JSONResponse)
	if !ok {
		t.Fatalf("want 200, got %T", fresp)
	}
	var sum, rows int64
	for _, f := range families.Items {
		sum += f.TotalBytes
		rows += f.VariantCount
	}
	if sum != u.TotalBytes {
		t.Errorf("family bytes sum to %d, usage total_bytes = %d", sum, u.TotalBytes)
	}
	if rows != u.VariantCount {
		t.Errorf("family rows sum to %d, usage variant_count = %d", rows, u.VariantCount)
	}
}

// #1598: an asset's preservation history is an operator read, with the same
// storage gate (401 without identity, 403 without system.storage.read or
// system.admin), 404 for an asset that does not exist, 400 for a cursor
// that is malformed or belongs to another asset. The 200 body carries
// the documented event fields only: never the sweep run's operator-only
// error text or a storage path, even when the event is a read error.
func TestStorageAdmin_PreservationHistory(t *testing.T) {
	pool := openPoolForJobs(t)
	t.Cleanup(pool.Close)
	s := &apiServer{storageAdmin: storage.NewAdminHandler(pool)}
	ctx := context.Background()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	hash := strings.Repeat("c", 63) + "1"
	asset, other, run := uuid.New(), uuid.New(), uuid.New()
	const leak = "read /srv/aa-storage/private/blobs/cc/c1: input/output error"
	exec(`INSERT INTO storage_objects (hash, size_bytes, content_type, backend) VALUES ($1, 4, 'image/png', 'fs') ON CONFLICT DO NOTHING`, hash)
	exec(`INSERT INTO assets (id, title, asset_type, file_hash) VALUES ($1, 'history', (SELECT MIN(ref) FROM asset_types), $3), ($2, 'other', (SELECT MIN(ref) FROM asset_types), $3)`, asset, other, hash)
	exec(`INSERT INTO storage_sweep_runs (id, kind, status, error) VALUES ($1, 'checksum_verify', 'failed', $2)`, run, leak)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id IN ($1, $2)`, asset, other)
		_, _ = pool.Exec(c, `DELETE FROM storage_sweep_runs WHERE id = $1`, run)
		_, _ = pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, hash)
	})
	q := storage.New(pool)
	version := "0.11.2-test"
	for i, reason := range []string{storage.ReasonReadError, storage.ReasonReadError} {
		p := storage.InsertFixityEventParams{
			AssetID:            pgtype.UUID{Bytes: asset, Valid: true},
			OccurredAt:         pgtype.Timestamptz{Time: time.Date(2026, 11, 4, 10, 0, i, 0, time.UTC), Valid: true},
			Outcome:            "failure",
			OutcomeReason:      reason,
			ObjectHash:         hash,
			VariantKey:         []string{storage.VariantOriginal, "preview"}[i],
			ObservedGeneration: 1,
			RecordedSha256:     &hash,
			FixityStatus:       storage.FixityVerified,
			SweepRunID:         pgtype.UUID{Bytes: run, Valid: true},
			AgentName:          storage.FixityAgentName,
			AgentVersion:       &version,
		}
		if _, err := q.InsertFixityEvent(ctx, p); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}

	req := func(id uuid.UUID, cursor *string, limit *int) openapi.ListAssetPreservationEventsRequestObject {
		return openapi.ListAssetPreservationEventsRequestObject{Id: id, Params: openapi.ListAssetPreservationEventsParams{Cursor: cursor, Limit: limit}}
	}
	call := func(c context.Context, r openapi.ListAssetPreservationEventsRequestObject) openapi.ListAssetPreservationEventsResponseObject {
		t.Helper()
		resp, err := s.ListAssetPreservationEvents(c, r)
		if err != nil {
			t.Fatalf("ListAssetPreservationEvents: %v", err)
		}
		return resp
	}

	if _, ok := call(ctx, req(asset, nil, nil)).(openapi.ListAssetPreservationEvents401JSONResponse); !ok {
		t.Fatal("no identity: want 401")
	}
	for _, caps := range [][]string{{"system.jobs.read"}, {"assets.admin"}, {}} {
		if _, ok := call(withCaps(caps...), req(asset, nil, nil)).(openapi.ListAssetPreservationEvents403JSONResponse); !ok {
			t.Fatalf("caps %v: want 403", caps)
		}
	}
	if _, ok := call(withCaps(storage.CapStorageRead), req(uuid.New(), nil, nil)).(openapi.ListAssetPreservationEvents404JSONResponse); !ok {
		t.Fatal("missing asset: want 404")
	}
	bad := "bm90LWEtY3Vyc29y"
	if _, ok := call(withCaps(storage.CapStorageRead), req(asset, &bad, nil)).(openapi.ListAssetPreservationEvents400JSONResponse); !ok {
		t.Fatal("malformed cursor: want 400")
	}

	one := 1
	for _, c := range []context.Context{withCaps(storage.CapStorageRead), withCaps("system.admin")} {
		first, ok := call(c, req(asset, nil, &one)).(openapi.ListAssetPreservationEvents200JSONResponse)
		if !ok {
			t.Fatal("read cap: want 200")
		}
		if first.AssetId != asset || first.Total != 2 || len(first.Items) != 1 || first.NextCursor == nil {
			t.Fatalf("first page: %+v", first)
		}
		e := first.Items[0]
		if e.VariantKey != "preview" || e.IsOriginal || e.OutcomeReason != openapi.PreservationEventOutcomeReason(storage.ReasonReadError) ||
			e.ObservedSha256 != nil || e.Algorithm != nil || e.SweepRunId == nil || *e.SweepRunId != run || !e.SweepRunPresent ||
			e.Agent.Name != storage.FixityAgentName || e.Agent.Version == nil || *e.Agent.Version != version {
			t.Fatalf("newest event: %+v", e)
		}
		// The asset's cursor is refused on another asset's history.
		if _, ok := call(c, req(other, first.NextCursor, &one)).(openapi.ListAssetPreservationEvents400JSONResponse); !ok {
			t.Fatal("foreign cursor: want 400")
		}
		second, ok := call(c, req(asset, first.NextCursor, &one)).(openapi.ListAssetPreservationEvents200JSONResponse)
		if !ok || len(second.Items) != 1 || !second.Items[0].IsOriginal || second.NextCursor != nil {
			t.Fatalf("second page: %+v", second)
		}

		// Nothing but the documented fields, and none of the run's error.
		body, err := json.Marshal(first)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{"input/output", "/srv/", "aa-storage", "blobs"} {
			if strings.Contains(string(body), s) {
				t.Fatalf("history body leaks %q: %s", s, body)
			}
		}
		var shape struct {
			Items []map[string]json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(body, &shape); err != nil {
			t.Fatal(err)
		}
		allowed := map[string]bool{
			"id": true, "source": true, "event_type": true, "occurred_at": true, "outcome": true, "outcome_reason": true,
			"object_hash": true, "variant_key": true, "is_original": true, "observed_generation": true, "algorithm": true,
			"observed_sha256": true, "observed_size_bytes": true, "recorded_sha256": true, "fixity_status": true,
			"sweep_run_id": true, "sweep_run_present": true, "agent": true,
		}
		for _, item := range shape.Items {
			for k := range item {
				if !allowed[k] {
					t.Fatalf("history event carries an undocumented field %q", k)
				}
			}
		}
	}
}
