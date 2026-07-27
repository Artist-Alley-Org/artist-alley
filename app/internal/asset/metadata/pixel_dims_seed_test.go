// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #618 — pixel_width / pixel_height must be SEEDED AND WIRED, and the
// wiring is the part that matters: the extractor routes values through
//
//	SELECT id, extraction_source, extraction_mode
//	  FROM field_definition WHERE extraction_source != '';
//
// so a definition with the default '' extraction_source routes nothing —
// the backfill reports success, asset_field_value stays empty, and
// info.json 404s exactly as if the definition did not exist. #621 made
// the extractor RUN (0 → 192 eligible); this makes its output LAND.
//
// THE FIXTURE RULE IS THE POINT OF THIS FILE. The reason this gap
// survived a release is that IIIF's own tests call ensurePixelFields and
// create the definitions themselves, exercising a schema state no real
// install has ever had. Nothing here creates a field definition. These
// tests run against artist_alley_test, which is built from the embedded
// migrations and nothing else — if migration 00017 does not provide the
// wired rows, they fail, which is the entire assertion.

package metadata_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	metadata "github.com/mscrnt/artist-alley/app/internal/asset/metadata"
	"github.com/mscrnt/artist-alley/app/internal/asset/metadata/exif"
	"github.com/mscrnt/artist-alley/app/internal/iiif"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// TestMigrationProvidesWiredPixelDimensionDefinitions is acceptance #2,
// with the sharpened clause: extraction_source NON-EMPTY, not merely
// that rows exist. A row with ” here reproduces today's failure shape
// byte for byte, so existence alone proves nothing.
func TestMigrationProvidesWiredPixelDimensionDefinitions(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	for _, code := range []string{"pixel_width", "pixel_height"} {
		var source, mode, ftype string
		err := pool.QueryRow(ctx, `
			SELECT extraction_source, extraction_mode, type
			  FROM field_definition WHERE code = $1`, code).
			Scan(&source, &mode, &ftype)
		if err != nil {
			t.Fatalf("%s: definition missing from the migrated schema — the seed "+
				"does not provide it and IIIF's dimension join cannot match: %v", code, err)
		}
		if source == "" {
			t.Errorf("%s exists but extraction_source is EMPTY — the mapping query "+
				"filters WHERE extraction_source != '', so this routes nothing and "+
				"is indistinguishable from the row not existing (#618)", code)
		}
		if source != code {
			t.Errorf("%s: extraction_source = %q, want the extractor's canonical "+
				"name %q (metadata.FieldPixelWidth/Height)", code, source, code)
		}
		if mode != "replace" {
			t.Errorf("%s: extraction_mode = %q, want 'replace' — dimensions are a "+
				"fact about the bytes; skip_if_set would freeze stale values "+
				"across a re-render or replace-file", code, mode)
		}
		if ftype != "number" {
			t.Errorf("%s: type = %q, want number (feeds value_num, which the IIIF "+
				"lookup reads)", code, ftype)
		}
	}
}

// ---------------------------------------------------------------------------
// End to end: real extractor → real applier → real IIIF lookup, against
// the definitions the MIGRATION provides. In-test adapters implement the
// applier's narrow interfaces over the pool with the same SQL production
// uses — they read and write VALUES; they never touch field_definition.
// ---------------------------------------------------------------------------

type testConfigReader struct{ pool *pgxpool.Pool }

func (r testConfigReader) ListExtractionConfig(ctx context.Context) ([]metadata.FieldExtractionConfig, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, extraction_source, extraction_mode
		  FROM field_definition WHERE extraction_source != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []metadata.FieldExtractionConfig
	for rows.Next() {
		var id pgtype.UUID
		var source, mode string
		if err := rows.Scan(&id, &source, &mode); err != nil {
			return nil, err
		}
		out = append(out, metadata.FieldExtractionConfig{
			FieldID: uuid.UUID(id.Bytes),
			Source:  metadata.CanonicalField(source),
			Mode:    metadata.ExtractionMode(mode),
		})
	}
	return out, rows.Err()
}

type testValueReader struct{ pool *pgxpool.Pool }

func (r testValueReader) GetAssetFieldValue(ctx context.Context, assetID, fieldID uuid.UUID) (metadata.FieldValueSnapshot, bool, error) {
	var snap metadata.FieldValueSnapshot
	err := r.pool.QueryRow(ctx, `
		SELECT value_text, value_num, value_date FROM asset_field_value
		 WHERE asset_id = $1 AND field_id = $2`, assetID, fieldID).
		Scan(&snap.ValueText, &snap.ValueNum, &snap.ValueDate)
	if err != nil {
		return metadata.FieldValueSnapshot{}, false, nil
	}
	return snap, true, nil
}

type testValueWriter struct{ pool *pgxpool.Pool }

func (w testValueWriter) WriteAssetFieldValue(ctx context.Context, p metadata.WriteAssetFieldValueParams) error {
	var valNum *float64
	var valText *string
	if p.Value.Kind == metadata.ValueKindNum {
		n := p.Value.Num
		valNum = &n
	}
	if p.Value.Kind == metadata.ValueKindText {
		s := p.Value.Text
		valText = &s
	}
	_, err := w.pool.Exec(ctx, `
		INSERT INTO asset_field_value (asset_id, field_id, value_text, value_num, set_by, set_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (asset_id, field_id) DO UPDATE SET
		    value_text = EXCLUDED.value_text,
		    value_num  = EXCLUDED.value_num,
		    set_by     = EXCLUDED.set_by,
		    set_at     = NOW()`,
		p.AssetID, p.FieldID, valText, valNum, p.SetBy)
	return err
}

type testFailureWriter struct{ t *testing.T }

func (w testFailureWriter) RecordExtractionFailure(_ context.Context, p metadata.RecordExtractionFailureParams) error {
	w.t.Errorf("unexpected extraction failure recorded: %+v", p)
	return nil
}

func TestPixelDimensionsFlowFromExtractorToIIIF(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	// A real PNG with a deliberately odd size, generated in-test.
	const wantW, wantH = 123, 77
	img := image.NewRGBA(image.Rect(0, 0, wantW, wantH))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	img.Set(3, 3, color.RGBA{R: 80, G: 120, B: 20, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}

	assetID := seedAsset(t, pool) // active, public, jpg-shaped fixture with a file hash

	// Real extractor over the real bytes.
	var ex exif.Extractor
	result, err := ex.Extract(ctx, bytes.NewReader(buf.Bytes()), "image/png")
	if err != nil && err != metadata.ErrNoMetadata {
		t.Fatalf("extract: %v", err)
	}

	// Real applier over the migration-provided config. If 00017's rows
	// were missing or unwired, ListExtractionConfig routes nothing and
	// the IIIF assertion below fails — which is the point.
	applier := metadata.NewApplier(
		testConfigReader{pool}, testValueReader{pool},
		testValueWriter{pool}, testFailureWriter{t},
	)
	summary, err := applier.Apply(ctx, metadata.AssetRef{ID: assetID, MimeType: "image/png"}, result)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	t.Cleanup(func() {
		// Cleanup runs after the test's context is cancelled, so this
		// keeps its own plain Background context (#622 class).
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM asset_field_value WHERE asset_id = $1`, assetID)
	})
	if len(summary.FieldsSet) == 0 {
		t.Fatal("applier set zero fields — extraction config routed nothing; " +
			"the definitions are missing or unwired (#618)")
	}

	// The exact consumer this exists for: IIIF's lookup.
	// Authenticated caller: PoolLookup's predicate resolves any
	// non-deleted asset for an authenticated ref (same rule as browse).
	got, err := iiif.PoolLookup{Pool: pool}.GetIIIFAsset(ctx, assetID,
		visibility.NewCaller(ptrRef(4290618)))
	if err != nil {
		t.Fatalf("GetIIIFAsset: %v", err)
	}
	if got.PixelWidth != wantW || got.PixelHeight != wantH {
		t.Fatalf("IIIF sees %dx%d, want %dx%d — the dimension join did not "+
			"resolve through the seeded definitions", got.PixelWidth, got.PixelHeight, wantW, wantH)
	}
}

func ptrRef(v int64) *int64 { return &v }
