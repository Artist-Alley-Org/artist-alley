-- 00002_digest_queue.sql
--
-- Phase 1.55.Y — email digest preferences + one-click unsubscribe.
--
-- The FIRST post-baseline migration. Per ADR 0046, append-only-forever
-- doesn't kick in until the v0.1.0 tag (pending issue #228), so pre-tag
-- migrations are added freely; a final re-squash folds this back into
-- 00001_baseline_v0_1.sql right before the tag. Do NOT edit the
-- baseline file for this — add it here.
--
-- Two changes:
--   1. digest_queue — one row per non-immediate notification email,
--      consumed + marked sent by the digest coordinator.
--   2. user_preferences.email_cadence — per-topic email cadence map
--      ({<verb>: "immediate|hourly|daily|weekly"}); absent = immediate,
--      so existing users keep send-now behaviour with no backfill.
--
-- Per-instance, never federates (no origin_server_id on digest_queue).

-- +goose Up
-- +goose StatementBegin
CREATE TABLE digest_queue (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_ref        bigint NOT NULL REFERENCES "user"(ref) ON DELETE CASCADE,
    topic           text NOT NULL,
    cadence         text NOT NULL CHECK (cadence IN ('hourly', 'daily', 'weekly')),
    notification_id uuid NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
    queued_at       timestamptz NOT NULL DEFAULT now(),
    sent_at         timestamptz
);
-- +goose StatementEnd

-- +goose StatementBegin
-- Partial index over the pending set — the coordinator's hot query is
-- "unsent rows for this cadence, grouped by user."
CREATE INDEX digest_queue_pending_idx
    ON digest_queue (cadence, user_ref)
    WHERE sent_at IS NULL;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE user_preferences
    ADD COLUMN email_cadence jsonb DEFAULT '{}'::jsonb NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE user_preferences DROP COLUMN IF EXISTS email_cadence;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS digest_queue;
-- +goose StatementEnd
