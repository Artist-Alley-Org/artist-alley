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
	"testing"

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
