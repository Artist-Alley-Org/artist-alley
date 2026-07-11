// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package metadata is artist-alley's upload-time file-metadata
// extraction subsystem (Phase 1.18.A-2). Lives as a sibling of
// [github.com/mscrnt/artist-alley/app/internal/metadata] —
// deliberately distinct:
//
//   - The top-level metadata/ package owns the operator-defined
//     custom-field system (FieldDefinition, AssetFieldValue, write
//     path through SetAssetFieldValue).
//   - This asset/metadata/ package extracts FILE-level technical
//     metadata (EXIF, ICC profile, dimensions, GPS, capture
//     datetime, …) from uploaded image bytes and writes the
//     extracted values INTO the field-value system.
//
// One is "what the operator decided this asset is about"; the
// other is "what the file itself claims about itself". They meet
// at the field-value write — but the extractor is upstream
// infrastructure, not part of the field-definition surface.
//
// # Arc shape
//
//   - 1.18.A-2 (current) — images only: EXIF + ICC + orientation
//     + per-user dedup + admin-triggered backfill + observability.
//   - 1.18.A-3 (next) — IPTC + XMP + raw embedded thumbnails.
//   - 1.18.A-4 (later) — PDF metadata + video timecode + Office
//     thumbs.
//
// # Why source bytes stay pristine
//
// The naive shape — read EXIF orientation, auto-rotate the source
// before storing — would mutate the source hash and break the
// content-addressed storage layer's dedup invariant. Instead this
// subsystem applies orientation at VARIANT render time:
// [orientation.RotateFromEXIF] runs inside the variant generator;
// the source's EXIF orientation tag stays as-is and the source
// hash is stable forever.
//
// # Why extraction failures are recorded, not silently dropped
//
// Photographers care about EXIF. A silent extraction failure
// means a user uploads a wedding photo and the capture date /
// camera model / GPS just don't appear — they think the feature
// is broken and lose trust. Recording every failure in the
// extraction_failure table + surfacing them at
// /admin/extraction-failures gives operators an actionable
// signal ("17 of last week's uploads had truncated EXIF segments
// — your import pipeline is corrupting files").
package metadata
