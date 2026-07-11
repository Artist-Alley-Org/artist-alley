// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.18.B-3 — operator-locked policy guards.
//
// Three constraints from the brief:
//   1. Subtitles MUST NOT count toward asset totals. Enforced
//      structurally: asset_subtitle_tracks is a separate table,
//      so SELECT count(*) FROM assets naturally excludes tracks.
//      No code change in count surfaces.
//   2. Subtitles MUST be bound to an actual asset. Enforced at
//      the schema layer: FK + CASCADE delete. App layer cannot
//      construct an orphan; INSERT fails.
//   3. Subtitles MUST only apply to assets with audio or video
//      renderable variants. Enforced HERE — RequiresAudioVideo
//      gates every mutating endpoint + the sidecar detector.
//
// The lookup uses GetAssetRenderableKind (sqlc-generated) which
// joins assets → asset_types and returns the name (Image, Video,
// Audio, …). Kept in the subtitles package so the dependency
// graph stays one-way (assets/ does not import subtitles/).

package subtitles

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrSubtitlesNotApplicable signals the asset's renderable kind
// doesn't support subtitles (not video, not audio). The API layer
// maps this to HTTP 422 Unprocessable Entity with a structured
// error body so frontend clients can render a "subtitles aren't
// available for this kind of asset" message rather than a generic
// 500.
var ErrSubtitlesNotApplicable = errors.New("subtitles: asset has no audio or video renderable variants")

// ErrAssetNotFound is returned when the policy gate queries the
// asset and gets pgx.ErrNoRows. Distinct from ErrSubtitlesNotApplicable
// so the API layer can surface 404 (the asset doesn't exist) vs
// 422 (it exists but isn't audio/video).
var ErrAssetNotFound = errors.New("subtitles: asset not found")

// rfc5646Pattern mirrors the CHECK constraint on
// asset_subtitle_tracks.lang. Keep the two in lockstep — if you
// loosen one without the other, schema-layer INSERTs reject
// values the app layer thinks are fine, and vice versa.
//
// The regex permits the standard BCP 47 form (primary subtag
// 1-8 alpha, optional script/region/variant subtags) plus the
// reserved "und" sentinel for sidecar files we couldn't auto-tag.
var rfc5646Pattern = regexp.MustCompile(`^[A-Za-z]{1,8}(-[A-Za-z0-9]{1,8}){0,4}$`)

// ValidateLang is the app-layer pre-check before the schema's
// CHECK constraint sees the value. Two reasons to validate twice:
//
//  1. We get a typed error (ErrInvalidLang) instead of a Postgres
//     CHECK violation that has to be string-parsed at the API layer.
//  2. The sidecar parser can reject bad lang segments before they
//     reach the upsert query, keeping bad uploads from logging as
//     SQL errors.
//
// "und" is accepted — it's the sentinel for auto-detected-but-unknown.
func ValidateLang(lang string) error {
	if lang == "und" {
		return nil
	}
	if !rfc5646Pattern.MatchString(lang) {
		return fmt.Errorf("%w: %q not a valid RFC 5646 tag", ErrInvalidLang, lang)
	}
	return nil
}

// ErrInvalidLang is returned by ValidateLang for malformed tags.
// Permanent — no retry will help.
var ErrInvalidLang = errors.New("subtitles: invalid language tag")

// requiresAudioVideoQuerier is the subset of *Queries we need for
// the policy check. Tests stub it; production wires *Queries.
type requiresAudioVideoQuerier interface {
	GetAssetRenderableKind(ctx context.Context, id pgtype.UUID) (*string, error)
}

// RequiresAudioVideo gates every subtitle endpoint + the
// sidecar detector. Returns:
//
//   - nil          — asset is video or audio; subtitle ops apply.
//   - ErrSubtitlesNotApplicable — asset exists but is image/3D/PDF/etc.
//   - ErrAssetNotFound          — asset row absent (deleted or never existed).
//   - wrapped error — DB hiccup; caller decides retry.
//
// The query joins assets → asset_types and respects deleted_at.
// A soft-deleted asset reads as not-found (correct — subtitle ops
// on a deleted asset should 404, not 422).
func RequiresAudioVideo(ctx context.Context, q requiresAudioVideoQuerier, assetID uuid.UUID) error {
	pgID := pgtype.UUID{Bytes: assetID, Valid: true}
	name, err := q.GetAssetRenderableKind(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAssetNotFound
		}
		return fmt.Errorf("subtitles: lookup asset kind: %w", err)
	}
	if name == nil {
		// asset_types.name is nullable per the legacy schema but
		// in practice it's always populated. Treat NULL as "not
		// audio/video" — conservative.
		return ErrSubtitlesNotApplicable
	}
	switch *name {
	case "Video", "Audio", "Audiobook":
		return nil
	default:
		return ErrSubtitlesNotApplicable
	}
}
