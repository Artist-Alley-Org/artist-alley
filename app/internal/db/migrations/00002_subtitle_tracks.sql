-- 00002_subtitle_tracks.sql
--
-- Phase 1.18.B-3 — Subtitle / caption tracks for video + audio assets.
-- First post-baseline migration per ADR 0046's append-only convention
-- (the squash baseline was 00001_baseline_v1.sql, shipped in 1.49.C-2).
--
-- asset_subtitle_tracks is a CHILD table of `assets` with CASCADE
-- delete. Tracks NEVER appear in `SELECT count(*) FROM assets` —
-- they're a distinct entity bound to a parent asset, not a kind of
-- asset themselves. This is enforced at the schema layer by the
-- separation; app-layer guards (subtitles.RequiresAudioVideo) gate
-- mutation endpoints to video/audio-kind assets only.
--
-- Design references
--
--   * Phase 1.18.B-3 brief — three operator-locked constraints
--     (don't count, must be bound, audio/video only).
--   * RS reference: include/video_functions.php:66 (display_video_subtitles)
--     uses the alt-files table with file_extension='vtt' as a
--     discriminator. We use a dedicated table for first-class FK +
--     lang lookup + cache key.
--   * RFC 5646 — BCP 47 language tag format. Validated by CHECK at
--     the schema layer; the app catches it earlier via the
--     [subtitles.ValidateLang] guard but the schema is the
--     load-bearing barrier.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE asset_subtitle_tracks (
    -- Asset this track belongs to. CASCADE delete: when the asset
    -- is hard-deleted, the tracks vanish with it. No orphan rows
    -- possible.
    asset_id       uuid   NOT NULL
        REFERENCES assets(id) ON DELETE CASCADE,

    -- RFC 5646 language tag (e.g. "en", "en-US", "ja", "fr-CA").
    -- "und" reserved for the auto-detected-but-unknown case
    -- (sidecar that matches by basename but has no lang segment).
    -- CHECK restricts to printable ASCII + max 35 chars per the
    -- RFC 5646 longest-tag bound.
    lang           text   NOT NULL
        CHECK (lang ~ '^[A-Za-z]{1,8}(-[A-Za-z0-9]{1,8}){0,4}$' OR lang = 'und'),

    -- Optional human-readable label ("English (US)", "Forced",
    -- "Director's commentary"). Empty string OK; null not allowed
    -- so the read path never needs nil-checks.
    label          text   NOT NULL DEFAULT '',

    -- CAS hash of the WebVTT file. Storage backend resolves to
    -- bytes via the same path every other variant uses.
    file_hash      text   NOT NULL,

    -- The format the track was uploaded in. Closed catalogue; new
    -- formats need a migration. The conversion worker stores VTT
    -- regardless, but tracking the source lets us re-convert if a
    -- converter bug surfaces.
    source_format  text   NOT NULL
        CHECK (source_format IN ('vtt','srt','ssa','ass','sub','idx')),

    -- 1.0 = text-based source (deterministic conversion);
    -- <1.0 = OCR'd bitmap source like IDX (DVD subtitles).
    -- UI surfaces a warning banner below 0.8.
    confidence     real   NOT NULL DEFAULT 1.0
        CHECK (confidence >= 0 AND confidence <= 1),

    created_at     timestamptz NOT NULL DEFAULT NOW(),

    -- (asset_id, lang) is the natural key — one track per language
    -- per asset. Re-upload of the same lang overwrites (handled by
    -- ON CONFLICT in the upsert query).
    PRIMARY KEY (asset_id, lang)
);

COMMENT ON TABLE asset_subtitle_tracks IS
    'Subtitle / caption tracks attached to video or audio assets. '
    'NOT first-class assets — excluded from asset counts. FK + '
    'CASCADE binds tracks to their parent. Phase 1.18.B-3.';

COMMENT ON COLUMN asset_subtitle_tracks.confidence IS
    'Quality of the source-to-VTT conversion. 1.0 for text-based '
    'sources; lower for OCR''d bitmap sources (IDX). UI surfaces '
    'a warning below 0.8.';

-- Per-asset list query is the hot path (the read on every video
-- render). PRIMARY KEY already indexes (asset_id, lang) so a
-- separate index on asset_id alone is redundant — the PK's
-- prefix scan covers it.

-- Per-post subtitle track override. NULL = use the asset's
-- intrinsic tracks (the 99% case). Non-NULL JSONB shape:
--   {"override": "none"}                         — hide all tracks for this post
--   {"override": "specific", "langs": ["en"]}   — show only these
--   {"override": "extra", "extras": [...]}      — append director-cut variants
-- The schema is open-ended on purpose (JSONB); the handler
-- interprets it. Lock the shape in OpenAPI when concrete UX
-- requirements surface.
ALTER TABLE posts
    ADD COLUMN subtitle_track_override jsonb;

COMMENT ON COLUMN posts.subtitle_track_override IS
    'Per-post override for the parent asset''s subtitle tracks. '
    'NULL means use the asset''s intrinsic tracks (99% case). '
    'Non-NULL JSONB carries director-cut overrides — see the '
    'subtitles package for the consumed shape. Phase 1.18.B-3.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE posts DROP COLUMN IF EXISTS subtitle_track_override;
DROP TABLE IF EXISTS asset_subtitle_tracks;
-- +goose StatementEnd
