-- 00012_ai_transcription.sql
--
-- Phase 1.14.C — Whisper transcription + subtitle pipeline integration.
--
-- # Adaptations vs the brief
--
--   - `ai.transcribe.use` granular capability NOT added. The umbrella
--     `ai.use` capability seeded by 00009 covers all five concerns
--     (tag/caption/embed/complete/transcribe). YAGNI per the project's
--     CLAUDE.md guidance — a per-concern split lands when an operator
--     actually wants to gate transcribe distinctly from tag/caption.
--
--   - `jobs.type_concurrency.ai.transcribe` was already seeded in
--     00009 (value=1) so the job slot is reserved + tunable from day
--     one. No change here.
--
--   - OpenAI Transcribe is already implemented (1.14.A shipped it
--     alongside the openai provider). Only Gemini Transcribe needs
--     a code-side addition in this PR. No config-seed change is
--     needed for openai; the existing per-provider routing entry
--     already covers it.

-- +goose Up
-- +goose StatementBegin

-- 1. Extend asset_subtitle_tracks.source_format CHECK to allow
--    'whisper' for AI-generated tracks. Postgres has no in-place
--    enum extension for CHECK lists; drop + recreate is the canonical
--    pattern. Existing rows (all in the original 6-value set) survive.
ALTER TABLE asset_subtitle_tracks
    DROP CONSTRAINT IF EXISTS asset_subtitle_tracks_source_format_check;

ALTER TABLE asset_subtitle_tracks
    ADD CONSTRAINT asset_subtitle_tracks_source_format_check
    CHECK (source_format IN ('vtt', 'srt', 'ssa', 'ass', 'sub', 'idx', 'whisper'));

-- 2. AI transcription config seeds.
--
--    ai.transcribe.default_model.* — per-provider default model so
--    the operator gets sensible behavior on a fresh install. large-v3
--    is the best-quality Whisper checkpoint as of 2026-06; operators
--    on smaller GPUs override to tiny/base/small/medium via the
--    admin UI.
--
--    ai.transcribe.chunk_seconds / chunk_overlap_seconds — Whisper's
--    native context window is 30s; we chunk longer audio with a 5s
--    overlap and stitch by halving the overlap region on each side
--    (standard production pattern). 25s/5s gives a clean 5-chunk
--    breakdown for a 2-minute clip.
--
--    ai.transcribe.auto_detect_language — Whisper auto-detects when
--    no language hint is supplied; flip to false to force the
--    operator to pass a hint every call.
--
--    ai.transcribe.min_confidence_warn — UI threshold; transcripts
--    below this avg-confidence (exp(avg_logprob)) get a "low
--    confidence" badge on the subtitle row.
INSERT INTO system_config (key, value) VALUES
    ('ai.transcribe.default_model.whisper_local', '"large-v3"'::jsonb),
    ('ai.transcribe.default_model.openai',        '"whisper-1"'::jsonb),
    ('ai.transcribe.default_model.gemini',        '"gemini-2.5-flash"'::jsonb),
    ('ai.transcribe.chunk_seconds',               '25'::jsonb),
    ('ai.transcribe.chunk_overlap_seconds',       '5'::jsonb),
    ('ai.transcribe.auto_detect_language',        'true'::jsonb),
    ('ai.transcribe.min_confidence_warn',         '0.7'::jsonb)
ON CONFLICT (key) DO NOTHING;

-- 3. Whisper-local provider registration. Disabled by default —
--    operator enables it from the admin UI after installing the
--    aa-whisper-local container (per ADR 0034 capability-add-on
--    pattern; AA itself doesn't ship the model files or runtime).
--    URL default targets the sibling container on the compose
--    network; operators on a remote box override via the admin UI.
INSERT INTO system_config (key, value) VALUES
    ('ai.providers.whisper_local',
     '{"kind":"whisper_local","url":"http://aa-whisper-local:9080","enabled":false,"privacy_class":"local"}'::jsonb)
ON CONFLICT (key) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM system_config WHERE key = 'ai.providers.whisper_local';
DELETE FROM system_config WHERE key LIKE 'ai.transcribe.%';

ALTER TABLE asset_subtitle_tracks
    DROP CONSTRAINT IF EXISTS asset_subtitle_tracks_source_format_check;
ALTER TABLE asset_subtitle_tracks
    ADD CONSTRAINT asset_subtitle_tracks_source_format_check
    CHECK (source_format IN ('vtt', 'srt', 'ssa', 'ass', 'sub', 'idx'));
-- +goose StatementEnd
