// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #614 — the IIIF Image API returned 404 for every asset, for a full
// release, because both endpoints gated on assets.has_image: a column
// that is DEFAULT false NOT NULL with no writer anywhere in the tree.
// Live, 1007/1007 rows were false. There was no input for which IIIF
// served anything.
//
// It broke INVISIBLY, which is why the guard is the deliverable. Every
// existing IIIF test passed throughout, because they all stub the lookup
// and stamped HasImage: true — a value the real query could never
// produce. The suite was green against a fixture that did not exist in
// production.
//
// So the guard has three layers, deliberately, because the unit layer is
// the one that already failed to catch this:
//
//  1. STRUCTURAL — iiif.IIIFAsset no longer HAS a HasImage field, so no
//     handler can consult it without a visible edit re-adding it. That
//     is enforced by the compiler, not by a test.
//  2. SOURCE — TestLookupSQLDoesNotConsultHasImage reads the query text
//     and fails if the column reappears there. Cheap, and it survives
//     someone reintroducing the field under another name in the row
//     projection.
//  3. BEHAVIOURAL — the servability gate is exercised in both
//     directions, including the case the fix must NOT become: an asset
//     with no rasters must still 404.
//
// The integration test at the bottom is the one that would actually have
// caught the original bug, because it runs the REAL query against a real
// row with has_image at its default false.

package iiif_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/iiif"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const regressionAssetID = "11111111-1111-1111-1111-111111111111"

func getIIIF(t *testing.T, h *iiif.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	newRouterFor(t, h).ServeHTTP(rr,
		httptest.NewRequest("GET", "http://test.example.com"+path, nil))
	return rr
}

// TestServabilityFollowsStoredVariants pins the replacement gate in both
// directions.
//
// The permissive failure is as bad as the original: a universal 404
// swapped for a universal 500 or a wrong-aspect image. So the "no
// rasters" case is asserted as hard as the "has rasters" case.
func TestServabilityFollowsStoredVariants(t *testing.T) {
	cases := []struct {
		name       string
		keys       []string
		wantStatus int
		why        string
	}{
		{
			name:       "full ladder stored",
			keys:       []string{"col", "preview", "screen", "hires"},
			wantStatus: http.StatusOK,
		},
		{
			name:       "partial ladder still serves",
			keys:       []string{"col", "preview"},
			wantStatus: http.StatusOK,
			why: "servability is ANY configured variant, not ALL — an asset " +
				"missing hires can still answer at the sizes it does have, and " +
				"Resolve/OpenVariant handle the per-request miss precisely",
		},
		{
			name:       "cover rung only",
			keys:       []string{"col"},
			wantStatus: http.StatusOK,
			why: "Resolve serves region=square from a cover rung, so col alone " +
				"is a servable asset even though info.json advertises no " +
				"proportional sizes for it",
		},
		{
			name:       "no variants at all — the md / failed-gltf case",
			keys:       nil,
			wantStatus: http.StatusNotFound,
			why: "THE FIX MUST NOT BECOME PERMISSIVE. A genuinely non-raster " +
				"asset has nothing IIIF can serve and must still 404",
		},
		{
			name:       "variants exist but none are configured",
			keys:       []string{"legacy_thumb", "old_big"},
			wantStatus: http.StatusNotFound,
			why: "Resolve only ever picks from the CONFIGURED list, so stale " +
				"keys left behind by a config change are bytes no request can " +
				"reach and must not make the asset look servable",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := defaultIIIFHandler(t, iiif.IIIFAsset{
				FileHash: "abc", VariantKeys: c.keys,
				PixelWidth: 6000, PixelHeight: 4000,
			})
			rr := getIIIF(t, h, "/iiif/3/"+regressionAssetID+"/info.json")
			if rr.Code != c.wantStatus {
				t.Errorf("info.json = %d, want %d — %s\nbody: %s",
					rr.Code, c.wantStatus, c.why, strings.TrimSpace(rr.Body.String()))
			}
		})
	}
}

// TestLookupSQLDoesNotConsultHasImage reads the real query text.
//
// Layer 2 of the guard. The struct field is gone, but a future change
// could reintroduce the column into the projection under any name; this
// fails on the column itself, which is the thing that has no writer.
func TestLookupSQLDoesNotConsultHasImage(t *testing.T) {
	src, err := os.ReadFile("http.go")
	if err != nil {
		t.Fatalf("read http.go: %v", err)
	}
	body := string(src)
	// Strip comments — this file EXPLAINS has_image at length, and the
	// explanation must not trip the check on itself.
	var code strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	if strings.Contains(code.String(), "has_image") {
		t.Error("the IIIF lookup references has_image again — that column is " +
			"DEFAULT false NOT NULL with no writer, so gating on it returns 404 " +
			"for every asset in the install (#614). Gate on stored variants.")
	}
	if strings.Contains(code.String(), "HasImage") {
		t.Error("iiif.IIIFAsset carries a HasImage field again (#614)")
	}
}

// ---------------------------------------------------------------------------
// The layer that would actually have caught it
// ---------------------------------------------------------------------------

func regressionPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	dsn := "host=" + env("AA_DB_HOST", "postgres") +
		" port=" + env("AA_DB_PORT", "5432") +
		" user=" + env("AA_DB_USER", "artist_alley") +
		" dbname=" + env("AA_DB_NAME", "artist_alley") +
		" sslmode=disable password=" + pwd
	ctx := t.Context()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestPoolLookupIgnoresHasImage runs the REAL query against a real row.
//
// The row is created the way every row in production is created: with
// has_image left at its DEFAULT FALSE. Under the old gate this asset was
// unservable; the lookup must now report its stored variants regardless,
// because has_image describes nothing.
//
// This is the shape of test the original code lacked. Every IIIF unit
// test stubbed the lookup and set HasImage: true — a value the real
// query could not return for any row in the database — so the suite
// stayed green while the feature was entirely dead.
func TestPoolLookupIgnoresHasImage(t *testing.T) {
	pool := regressionPool(t)
	ctx := context.Background()

	id := uuid.New()
	sum := sha256.Sum256([]byte("#614 iiif " + id.String()))
	hash := hex.EncodeToString(sum[:])

	if _, err := pool.Exec(ctx, `
		INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
		VALUES ($1, 1, 'image/webp', 'fs') ON CONFLICT (hash) DO NOTHING`, hash); err != nil {
		t.Fatalf("seed object: %v", err)
	}
	// has_image is NOT set — it takes its DEFAULT false, exactly as
	// every asset the upload path creates does.
	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity,
		                    processing_status, file_hash, created_at)
		VALUES ($1,'#614 iiif probe',4291001,(SELECT MIN(ref) FROM asset_types),
		        'active','public','ready',$2,NOW())`, id, hash); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	for _, k := range []string{"col", "preview", "screen", "hires"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type)
			VALUES ($1,$2,1,'image/webp') ON CONFLICT DO NOTHING`, hash, k); err != nil {
			t.Fatalf("seed variant %q: %v", k, err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM storage_variants WHERE object_hash = $1`, hash)
		_, _ = pool.Exec(bg, `DELETE FROM assets WHERE id = $1`, id)
		_, _ = pool.Exec(bg, `DELETE FROM storage_objects WHERE hash = $1`, hash)
	})

	// Confirm the premise this whole issue rests on, so the assertion
	// below cannot pass for the wrong reason.
	//
	// The premise used to be "has_image is false for this row", read
	// with a SELECT. Since #579 dropped the column the premise is
	// stronger and structural: the column does not exist, so nothing can
	// gate on it even by accident. Asserting its ABSENCE is what keeps
	// this test meaningful — a re-added column would silently restore
	// the option of gating on it again.
	var columnExists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                WHERE table_name = 'assets' AND column_name = 'has_image')
	`).Scan(&columnExists); err != nil {
		t.Fatalf("probe for has_image column: %v", err)
	}
	if columnExists {
		t.Fatal("assets.has_image is back. It had no writer and gating on it " +
			"returned 404 for every asset (#614); it was dropped in #579. If it " +
			"has been reintroduced deliberately, this test needs rewriting rather " +
			"than deleting.")
	}

	got, err := iiif.PoolLookup{Pool: pool}.GetIIIFAsset(ctx, id,
		visibility.NewCaller(ptrInt64(4291001)))
	if err != nil {
		t.Fatalf("GetIIIFAsset: %v", err)
	}
	if got.FileHash != hash {
		t.Errorf("FileHash = %q, want %q", got.FileHash, hash)
	}
	if len(got.VariantKeys) != 4 {
		t.Fatalf("VariantKeys = %v, want the 4 seeded rungs — the lookup is not "+
			"reporting stored variants, so the servability gate has nothing to "+
			"decide on and IIIF 404s every asset again (#614)", got.VariantKeys)
	}
}

func ptrInt64(v int64) *int64 { return &v }
