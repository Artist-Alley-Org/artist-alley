// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package pixeldims projects an asset's recorded pixel dimensions onto
// the card payloads (#640).
//
// The dimensions are NOT a column on `assets`. They live in
// `asset_field_value` under the `pixel_width` / `pixel_height` field
// definitions seeded (extraction-wired) by migration 00017 for #618, and
// they are written by the EXIF pass. Until now the only reader was IIIF
// info.json, so every card surface shipped without them and the client
// had no way to know how tall a tile should be — masonry rendered a wall
// of identical squares because CSS was the only thing deciding the tile
// shape (#640, and the same "the client cannot see what the server
// knows" class as #591).
//
// WHY A SHARED FRAGMENT RATHER THAN A COLUMN OR A VIEW. Three of the
// four read paths that need this are hand-built SQL (a runtime
// visibility fragment can't be a static sqlc query — see
// assets.ListAssetsPageGated), so the projection has to be splice-able.
// A correlated scalar subquery per column is the shape that stays fast
// on a browse page: the `asset_field_value` primary key is
// (asset_id, field_id), so each is one index probe per row for the
// 24-72 rows a page returns. An aggregate VIEW over the whole
// field-value table would be the tidier spelling and the wrong plan —
// it invites a full grouped scan of every field value in the install to
// answer a question about one page.
//
// WHAT "NO DIMENSIONS" MEANS. NULL, deliberately — not 0. Only raster
// assets that the EXIF pass has actually seen carry these values, and
// the backfill selects `status = 'active'` only, so a draft raster, a
// video, a 3D model, an audio waveform and a font all legitimately have
// none. NULL says "unknown, decide client-side"; a 0 would have to be
// special-cased by every consumer and reads as a real measurement in
// logs. (iiif.PoolLookup COALESCEs to 0 for its own reasons — it needs a
// number for info.json — and that is a choice local to IIIF, not the
// storage contract.)
package pixeldims

// Width / Height are the canonical field_definition codes. They mirror
// metadata.FieldPixelWidth / FieldPixelHeight, which is where the
// extractor writes them; duplicated as bare constants so this leaf
// package stays free of the metadata package's dependency graph (jobs,
// storage, extractors) — every browse path imports this one.
const (
	Width  = "pixel_width"
	Height = "pixel_height"
)

// SelectColumnsSQL returns two correlated scalar subqueries, aliased
// `pixel_width` and `pixel_height`, to splice into a SELECT list.
// `idExpr` is the SQL expression naming the asset id in the enclosing
// query — a qualified column (`a.id`) or a bare one (`assets.id`).
//
// The result carries NO placeholders, so it is safe to splice at any
// point in a builder that is bookkeeping its own `$n` indexes (ADR 0063
// placeholder discipline): it neither consumes nor shifts one.
//
// The `::INT` cast is load-bearing. `value_num` is DOUBLE PRECISION (the
// column is the generic numeric slot for every numeric field), and
// scanning a float into an int32 destination is a pgx type error, not a
// silent truncation.
func SelectColumnsSQL(idExpr string) string {
	return `(SELECT afv.value_num::INT FROM asset_field_value afv
                 WHERE afv.asset_id = ` + idExpr + `
                   AND afv.field_id = (SELECT id FROM field_definition
                                        WHERE code = '` + Width + `' LIMIT 1)
                 LIMIT 1) AS pixel_width,
              (SELECT afv.value_num::INT FROM asset_field_value afv
                 WHERE afv.asset_id = ` + idExpr + `
                   AND afv.field_id = (SELECT id FROM field_definition
                                        WHERE code = '` + Height + `' LIMIT 1)
                 LIMIT 1) AS pixel_height`
}

// Sane reports whether a (width, height) pair is usable as an aspect
// ratio. Both must be present and positive; either alone is useless and
// a zero would divide.
//
// Callers pass what they scanned, so this is the ONE place that decides
// what "has dimensions" means — the API projects a pair or neither,
// never a half-populated one that the client has to re-validate.
func Sane(w, h *int32) bool {
	return w != nil && h != nil && *w > 0 && *h > 0
}
