// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #618 → #765 — pixel_width / pixel_height must be SEEDED, and must NOT
// be extraction-wired.
//
// #618's version of this file asserted the opposite: extraction_source
// non-empty, because the EXIF extractor was the only producer and the
// mapping query
//
//	SELECT id, extraction_source, extraction_mode
//	  FROM field_definition WHERE extraction_source != '';
//
// routes nothing without it. #757 gave the fields their real producer —
// the preview pipeline, which measures the ladder SOURCE — and left the
// extractor's write in place beside it. Two writers, one pair of rows,
// extraction_mode='replace' on both, and no precedence rule anywhere:
// the applier's only check is "is a value present", never set_by, so
// ADR 0012's written-but-unimplemented "skip if set_by='manual'" rule
// never applied. On the upload path the extract job is enqueued after
// the preview job, so the extractor wrote last and won.
//
// It won with the WRONG NUMBER. image.DecodeConfig reports the stored
// pixel grid, and this subsystem deliberately leaves the source bytes
// (and their orientation tag) untouched, rotating at variant-render
// time — so for an orientation=6 phone photo the extractor's pair is
// the transpose of what every rung and every tile actually shows. ADR
// 0071 §6 names the quantity these rows hold: the shape of the image the
// contain rungs are built from, which for a font, a waveform or a
// turntable is the ONLY image there is and has nothing to do with source
// pixels at all.
//
// So #765 removed the extractor's write and migration 00020 removed its
// route. What this file now pins is the pair of facts that keeps the
// definitions useful without letting the second writer back:
//
//   - the migration provides both rows, typed number, resolvable by code
//     (dropping them re-404s every info.json — #618);
//   - the extract path writes NEITHER of them, whatever it is handed;
//   - a value recorded the way production records it reaches IIIF.
//
// THE FIXTURE RULE IS STILL THE POINT OF THIS FILE. The reason the #618
// gap survived a release is that IIIF's own tests call ensurePixelFields
// and created the definitions themselves, exercising a schema state no
// real install has ever had. Nothing here creates a field definition.
// These tests run against artist_alley_test, built from the embedded
// migrations and nothing else — if the migrations do not provide the
// rows in the right state, they fail, which is the entire assertion.

package metadata_test

import (
	"bytes"
	"context"
	"image"
	_ "image/jpeg" // DecodeConfig for the fixture-shape assertion
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	metadata "github.com/mscrnt/artist-alley/app/internal/asset/metadata"
	"github.com/mscrnt/artist-alley/app/internal/asset/metadata/exif"
	"github.com/mscrnt/artist-alley/app/internal/asset/pixeldims"
	"github.com/mscrnt/artist-alley/app/internal/iiif"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// TestMigrationProvidesUnwiredPixelDimensionDefinitions replaces #618's
// TestMigrationProvidesWired… and inverts its central clause.
//
// The row must exist and be typed number — IIIF's dimension join and
// pixeldims.SelectColumnsSQL both resolve it by code and read value_num,
// so a missing or mistyped row 404s info.json and blanks every masonry
// tile exactly as #618 described.
//
// The row must NOT be extraction-wired. extraction_source is the
// mapping from a canonical extractor field to a definition, and pointing
// one at these two says "any extractor that reports a pixel_width may
// write this row" — the sentence #765 retracts. The value is computed
// off the rotated ladder source; no extractor can see that image.
func TestMigrationProvidesUnwiredPixelDimensionDefinitions(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	for _, code := range []string{"pixel_width", "pixel_height"} {
		var source, ftype string
		err := pool.QueryRow(ctx, `
			SELECT extraction_source, type
			  FROM field_definition WHERE code = $1`, code).
			Scan(&source, &ftype)
		if err != nil {
			t.Fatalf("%s: definition missing from the migrated schema — the seed "+
				"does not provide it and IIIF's dimension join cannot match: %v", code, err)
		}
		if ftype != "number" {
			t.Errorf("%s: type = %q, want number (feeds value_num, which the IIIF "+
				"lookup and the browse projection read)", code, ftype)
		}
		if source != "" {
			t.Errorf("%s: extraction_source = %q, want EMPTY. A route here lets an "+
				"extractor's pre-rotation stored grid overwrite the preview "+
				"pipeline's post-rotation measurement, and nothing arbitrates "+
				"between them — extraction_mode is 'replace' and the applier never "+
				"reads set_by (#765, ADR 0071 §6)", code, source)
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

// readPixelDims returns the recorded pair and its provenance, or
// found=false when no row exists. Reads by field_definition CODE, the
// way pixeldims.SelectColumnsSQL and IIIF's join do.
func readPixelDims(t *testing.T, pool *pgxpool.Pool, assetID uuid.UUID) (w, h int, setBy string, found bool) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT f.code, v.value_num::INT, v.set_by
		  FROM asset_field_value v
		  JOIN field_definition f ON f.id = v.field_id
		 WHERE v.asset_id = $1 AND f.code IN ('pixel_width', 'pixel_height')`, assetID)
	if err != nil {
		t.Fatalf("read pixel dims: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var code, by string
		var num int
		if err := rows.Scan(&code, &num, &by); err != nil {
			t.Fatalf("scan pixel dims: %v", err)
		}
		found = true
		setBy = by
		if code == "pixel_width" {
			w = num
		} else {
			h = num
		}
	}
	return w, h, setBy, found
}

// TestExtractPathNeverWritesPixelDimensions is the #765 regression, run
// through the REAL pieces: the real EXIF extractor, the real applier,
// and the migration's own extraction config — no in-test field
// definitions, no hand-built Result.
//
// The fixture is stored landscape with an orientation=6 tag, so the
// extractor's old DecodeConfig sniff would report 96x48 while the
// displayed image, every rung, and every tile are 48x96. The asset
// starts with the correct pair already recorded the way production
// records it, which is the true starting state on the upload path (the
// preview job is enqueued first). Before this fix the applier
// overwrote it — extraction_mode is 'replace' and nothing consults
// set_by — and a portrait photo tiled as landscape.
func TestExtractPathNeverWritesPixelDimensions(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	raw, err := os.ReadFile(filepath.Join("testdata", "orientation_6_landscape.jpg"))
	if err != nil {
		t.Fatalf("load rotated fixture: %v", err)
	}

	// The fixture must actually be rotated, asserted before anything
	// depends on it. A square or untagged fixture makes every assertion
	// below pass for the wrong reason.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode fixture config: %v", err)
	}
	if cfg.Width <= cfg.Height {
		t.Fatalf("fixture stored grid %dx%d is not landscape — nothing distinguishes "+
			"the stored pair from the displayed pair (#765)", cfg.Width, cfg.Height)
	}
	storedW, storedH := cfg.Width, cfg.Height
	displayedW, displayedH := storedH, storedW

	var ex exif.Extractor
	result, err := ex.Extract(ctx, bytes.NewReader(raw), "image/jpeg")
	if err != nil && err != metadata.ErrNoMetadata {
		t.Fatalf("extract: %v", err)
	}
	if result.Orientation != 6 {
		t.Fatalf("fixture Orientation tag = %d, want 6 — the transposition under "+
			"test does not happen without it (#765)", result.Orientation)
	}

	assetID := seedAsset(t, pool) // active, public, jpg-shaped fixture with a file hash
	t.Cleanup(func() {
		// Cleanup runs after the test's context is cancelled, so this
		// keeps its own plain Background context (#622 class).
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM asset_field_value WHERE asset_id = $1`, assetID)
	})

	// The preview pipeline got there first, as it does on upload.
	if err := pixeldims.Record(ctx, pool, assetID, displayedW, displayedH); err != nil {
		t.Fatalf("pixeldims.Record: %v", err)
	}

	// Real applier over the migration-provided extraction config.
	applier := metadata.NewApplier(
		testConfigReader{pool}, testValueReader{pool},
		testValueWriter{pool}, testFailureWriter{t},
	)
	if _, err := applier.Apply(ctx,
		metadata.AssetRef{ID: assetID, MimeType: "image/jpeg"}, result); err != nil {
		t.Fatalf("apply: %v", err)
	}

	gotW, gotH, setBy, found := readPixelDims(t, pool, assetID)
	if !found {
		t.Fatal("the extract pass removed the recorded dimensions entirely")
	}
	if gotW == storedW && gotH == storedH {
		t.Fatalf("after the extract pass the asset records the STORED grid %dx%d "+
			"instead of the displayed %dx%d — the EXIF writer is back, and a "+
			"portrait phone photo reserves a landscape tile (#765)",
			gotW, gotH, displayedW, displayedH)
	}
	if gotW != displayedW || gotH != displayedH {
		t.Errorf("recorded %dx%d, want the displayed pair %dx%d", gotW, gotH, displayedW, displayedH)
	}
	if setBy != pixeldims.SetBy {
		t.Errorf("set_by = %q, want %q — the extract path claimed provenance over a "+
			"number it did not measure (#765)", setBy, pixeldims.SetBy)
	}

	// And the extractor did not merely lose the tie — it never entered
	// it. Nothing routes a pixel dimension through extraction any more.
	if _, ok := result.Fields[metadata.FieldPixelWidth]; ok {
		t.Error("extractor emitted pixel_width; it must not (#765)")
	}
	if _, ok := result.Fields[metadata.FieldPixelHeight]; ok {
		t.Error("extractor emitted pixel_height; it must not (#765)")
	}
}

// TestRecordedPixelDimensionsReachIIIF keeps the #618 end-to-end whose
// producer changed. info.json 404s an asset with no recorded dimensions
// (BuildInfo returns ErrUnsupportedAsset on 0x0), so removing the EXIF
// writer without a working replacement path would re-open that hole.
// The write here is the production one — pixeldims.Record, which is what
// preview.stampSourceShape calls — against the definitions the migration
// provides and nothing this test created.
func TestRecordedPixelDimensionsReachIIIF(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	// A deliberately odd, deliberately non-square pair: a constant or a
	// transposition is visible in the failure message.
	const wantW, wantH = 123, 77

	assetID := seedAsset(t, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM asset_field_value WHERE asset_id = $1`, assetID)
	})

	if err := pixeldims.Record(ctx, pool, assetID, wantW, wantH); err != nil {
		t.Fatalf("pixeldims.Record: %v", err)
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
