-- artist-alley migration 00006 — leaner audit log.
--
-- RS's activity_log has 10+ generic columns ("remote_table", "remote_column",
-- "ref_column_override", etc.) that try to be a polymorphic foreign key
-- system in a single table. We're not porting that.
--
-- audit_events is a flat, append-only record of security- and policy-relevant
-- events. The metadata column is a typed JSONB blob whose shape depends on
-- event_type, documented in code at app/internal/audit/events.go.
--
-- Initial event types (Phase 1.5):
--   login.succeeded         — user logged in
--   login.failed            — bad password or unknown user
--   login.rate_limited      — request rejected by the limiter
--   logout                  — explicit logout
--   session.revoked         — session token invalidated (admin or self)
--   session.expired         — idle/hard timeout reached

-- +goose Up

CREATE TABLE audit_events (
    id                UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type        TEXT         NOT NULL,
    occurred_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    subject_user_ref  BIGINT       NULL,                       -- user the event happened to
    actor_user_ref    BIGINT       NULL,                       -- user who caused it (admin acting on another user); NULL = self or system
    ip                INET         NULL,
    user_agent        TEXT         NULL,
    metadata          JSONB        NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX audit_events__type_time_idx
    ON audit_events (event_type, occurred_at DESC);

CREATE INDEX audit_events__subject_time_idx
    ON audit_events (subject_user_ref, occurred_at DESC)
    WHERE subject_user_ref IS NOT NULL;

COMMENT ON TABLE  audit_events IS
    'Append-only audit log for security- and policy-relevant events.';
COMMENT ON COLUMN audit_events.event_type IS
    'Dotted-namespace event code, e.g. login.succeeded, session.revoked.';
COMMENT ON COLUMN audit_events.metadata IS
    'Event-type-specific JSON payload. See app/internal/audit/events.go for the typed shape per event_type.';

-- +goose Down

DROP TABLE IF EXISTS audit_events;
