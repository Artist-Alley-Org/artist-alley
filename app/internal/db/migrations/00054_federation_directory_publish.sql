-- artist-alley migration 00054 — publish-to-directory state.
-- Phase 1.22.B-c-bis, feat/user-surfaces.
--
-- Extends federation_directories with columns tracking whether
-- THIS instance has published itself to the directory. This is
-- the inverse of the subscriber columns from 00053:
--
--   subscriber side (1.22.B-c)  — we POLL the directory
--   publisher side (1.22.B-c-bis, this migration) — we are LISTED
--
-- The CHECK matches federation/directory.PublishStatus per ADR 0042.
-- not_published → pending_dns → pending_register → listed
-- Failures land in 'failed' with publish_last_error populated.

-- +goose Up

ALTER TABLE federation_directories
    ADD COLUMN publish_status TEXT NOT NULL DEFAULT 'not_published'
        CHECK (publish_status IN (
            'not_published', 'pending_dns', 'pending_register', 'listed', 'failed'
        )),
    -- Token currently issued by /v1/challenge but not yet redeemed
    -- by /v1/register. Cleared once register succeeds or fails.
    -- The token is bound to the instance URL on the directory side
    -- so a leaked token can't be used to register a different URL.
    ADD COLUMN publish_pending_token TEXT NOT NULL DEFAULT '',
    ADD COLUMN publish_token_expires_at TIMESTAMPTZ NULL,
    -- The DNS-TXT record the operator must publish for our pending
    -- challenge. Persisted so the admin UI can show it across
    -- page refreshes + operator stages the DNS change later.
    ADD COLUMN publish_record_name TEXT NOT NULL DEFAULT '',
    ADD COLUMN publish_record_value TEXT NOT NULL DEFAULT '',
    -- listing_id assigned by the directory on successful register.
    -- Useful for future "self-unlist" support.
    ADD COLUMN publish_listing_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN publish_last_attempt_at TIMESTAMPTZ NULL,
    ADD COLUMN publish_last_error TEXT NOT NULL DEFAULT '',
    -- The operator-chosen display name + region + description +
    -- tags the directory will show. Persisted so the publish form
    -- pre-fills next time the admin opens it.
    ADD COLUMN publish_display_name TEXT NOT NULL DEFAULT '',
    ADD COLUMN publish_region TEXT NOT NULL DEFAULT '',
    ADD COLUMN publish_description TEXT NOT NULL DEFAULT '',
    ADD COLUMN publish_tags JSONB NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN federation_directories.publish_status IS
    'Publication state machine. not_published → pending_dns '
    '(challenge issued, operator must add TXT record) → '
    'pending_register (DNS visible, /v1/register in flight) → '
    'listed. Any failure lands in failed with last_error populated.';

-- +goose Down

ALTER TABLE federation_directories
    DROP COLUMN publish_tags,
    DROP COLUMN publish_description,
    DROP COLUMN publish_region,
    DROP COLUMN publish_display_name,
    DROP COLUMN publish_last_error,
    DROP COLUMN publish_last_attempt_at,
    DROP COLUMN publish_listing_id,
    DROP COLUMN publish_record_value,
    DROP COLUMN publish_record_name,
    DROP COLUMN publish_token_expires_at,
    DROP COLUMN publish_pending_token,
    DROP COLUMN publish_status;
