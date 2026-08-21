// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/asset/metadata"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// openTestPool — duplicated from auth/last_admin_test.go to keep
// the metadata package's tests free-standing. Same skip-on-no-
// password contract.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := testdb.Name(t)
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx := t.Context()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// seedAsset inserts a minimal asset + its storage_object parent,
// returns the asset id. Hash is unique-per-call via rand so the
// per-user dedup index from migration 00001 doesn't fire across
// test cases.
func seedAsset(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	hashBytes := make([]byte, 32)
	if _, err := rand.Read(hashBytes); err != nil {
		t.Fatalf("rand: %v", err)
	}
	hash := hex.EncodeToString(hashBytes)
	id := uuid.New()
	_, err := pool.Exec(ctx, `
		INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
		VALUES ($1, 1024, 'image/jpeg', 'fs')
		ON CONFLICT (hash) DO NOTHING
	`, hash)
	if err != nil {
		t.Fatalf("seed storage_object: %v", err)
	}
	// has_image is deliberately NOT set — it takes its DEFAULT false,
	// exactly as every asset the upload path creates does (#579).
	//
	// It used to be stamped `true` here, and that single word is why the
	// EXIF backfill could ship processing zero assets with a green
	// suite: every test in this package asserted a row state that no
	// production code can produce. Eligibility is decided by
	// file_extension now, which is real data, so leaving the column at
	// its default costs nothing and keeps these tests honest.
	_, err = pool.Exec(ctx, `
		INSERT INTO assets (
			id, title, asset_type, status,
			file_hash, file_extension, file_size_bytes,
			sensitivity
		) VALUES ($1, $2, 1, 'active', $3, 'jpg', 1024, 'public')
	`, id, "metadata-admin-test-"+hash[:8], hash)
	if err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id)
	})
	return id
}

func seedFailure(t *testing.T, pool *pgxpool.Pool, assetID uuid.UUID, errorKind, format, message string) uuid.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		INSERT INTO extraction_failure
		    (asset_id, format, error_kind, message, field_key, raw_value)
		VALUES ($1, $2, $3, $4, '', 'null'::jsonb)
		RETURNING id
	`, assetID, format, errorKind, message).Scan(&id)
	if err != nil {
		t.Fatalf("seed extraction_failure: %v", err)
	}
	return uuid.UUID(id.Bytes)
}

// dismissAll wipes the test's own failures at the end of a case so
// later tests in the package don't see them in their listings.
func dismissAll(t *testing.T, pool *pgxpool.Pool, ids ...uuid.UUID) {
	t.Helper()
	for _, id := range ids {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM extraction_failure WHERE id = $1`, id)
	}
}

func TestAdminHandler_ListAndDismiss(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	h := metadata.NewAdminHandler(pool)

	// Baseline — record the existing pending count so we can assert
	// our deltas without assuming an empty queue.
	baseRows, baseTotal, err := h.ListFailures(ctx, metadata.ListFailuresFilter{})
	if err != nil {
		t.Fatalf("baseline list: %v", err)
	}
	_ = baseRows

	asset := seedAsset(t, pool)
	f1 := seedFailure(t, pool, asset, "validation", "image/jpeg", "value out of range")
	f2 := seedFailure(t, pool, asset, "malformed_file", "image/jpeg", "bad CRC")
	f3 := seedFailure(t, pool, asset, "validation", "image/png", "negative ISO")
	t.Cleanup(func() { dismissAll(t, pool, f1, f2, f3) })

	// Total should jump by 3.
	_, total, err := h.ListFailures(ctx, metadata.ListFailuresFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got, want := total-baseTotal, int64(3); got != want {
		t.Fatalf("delta total = %d, want %d", got, want)
	}

	// Filter by error_kind=validation should pick up f1 + f3.
	kind := "validation"
	_, validationTotal, err := h.ListFailures(ctx, metadata.ListFailuresFilter{ErrorKind: &kind, Limit: 100})
	if err != nil {
		t.Fatalf("list filter: %v", err)
	}
	if validationTotal < 2 {
		t.Fatalf("validation total = %d, want >= 2 (this case's f1 + f3)", validationTotal)
	}

	// Filter by format=image/png should at least catch f3.
	format := "image/png"
	pngRows, pngTotal, err := h.ListFailures(ctx, metadata.ListFailuresFilter{Format: &format, Limit: 100})
	if err != nil {
		t.Fatalf("list format: %v", err)
	}
	if pngTotal < 1 {
		t.Fatalf("png total = %d, want >= 1", pngTotal)
	}
	found := false
	for _, r := range pngRows {
		if r.ID == f3 {
			found = true
			if r.AssetID != asset {
				t.Errorf("row.AssetID = %v, want %v", r.AssetID, asset)
			}
			if r.ErrorKind != "validation" {
				t.Errorf("row.ErrorKind = %q, want validation", r.ErrorKind)
			}
			if !strings.Contains(r.Message, "negative ISO") {
				t.Errorf("row.Message = %q, want contains 'negative ISO'", r.Message)
			}
			if r.DismissedAt != nil {
				t.Errorf("pending row should have DismissedAt nil; got %v", r.DismissedAt)
			}
		}
	}
	if !found {
		t.Errorf("f3 not in png-filtered list")
	}

	// Dismiss f1 + verify it drops from the pending list.
	if err := h.DismissFailure(ctx, f1); err != nil {
		t.Fatalf("dismiss f1: %v", err)
	}
	_, afterDismiss, err := h.ListFailures(ctx, metadata.ListFailuresFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list after dismiss: %v", err)
	}
	if afterDismiss != total-1 {
		t.Fatalf("after-dismiss total = %d, want %d", afterDismiss, total-1)
	}

	// Idempotent — dismissing twice succeeds.
	if err := h.DismissFailure(ctx, f1); err != nil {
		t.Errorf("idempotent dismiss should succeed, got: %v", err)
	}

	// Unknown id → ErrFailureNotFound.
	if err := h.DismissFailure(ctx, uuid.New()); !errors.Is(err, metadata.ErrFailureNotFound) {
		t.Errorf("DismissFailure(unknown id) = %v, want ErrFailureNotFound", err)
	}
}

func TestAdminHandler_ListPagination(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	h := metadata.NewAdminHandler(pool)

	asset := seedAsset(t, pool)
	ids := make([]uuid.UUID, 0, 5)
	for i := 0; i < 5; i++ {
		ids = append(ids, seedFailure(t, pool, asset, "validation", "image/webp", "wat"))
	}
	t.Cleanup(func() { dismissAll(t, pool, ids...) })

	format := "image/webp"
	page1, total, err := h.ListFailures(ctx, metadata.ListFailuresFilter{
		Format: &format, Limit: 2, Offset: 0,
	})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if total < 5 {
		t.Fatalf("total = %d, want >= 5", total)
	}
	if len(page1) != 2 {
		t.Errorf("page1 len = %d, want 2", len(page1))
	}

	page2, _, err := h.ListFailures(ctx, metadata.ListFailuresFilter{
		Format: &format, Limit: 2, Offset: 2,
	})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 2 {
		t.Errorf("page2 len = %d, want 2", len(page2))
	}
	if len(page1) > 0 && len(page2) > 0 && page1[0].ID == page2[0].ID {
		t.Errorf("page2 overlaps page1 — pagination broken")
	}
}
