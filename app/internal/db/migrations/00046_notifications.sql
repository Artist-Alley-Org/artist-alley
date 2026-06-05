-- artist-alley migration 00046 — notifications.
-- Phase 1.17.I2, feat/user-surfaces.
--
-- One row per (recipient, event). Broadcasts (admin announcements
-- in 1.17.I) will sit on a separate `messages` + `user_messages`
-- join later — that's the fan-out shape RS uses. Per-user
-- notifications stay one-row-per-recipient: simpler queries, no
-- fan-out wait at write time, payload carries any per-recipient
-- context the verb needs.
--
-- Verb taxonomy is shared with the notification-channel prefs in
-- userprefs.KnownEventTypes — the same string identifies "what kind
-- of event happened" both when checking "does the user want this
-- event delivered?" (write-time) and when filtering the inbox
-- (read-time). Drift would break the prefs UI, so it's documented
-- as a strict contract in app/internal/notifications/events.go.
--
-- Payload column: typed per-verb extra data — comment_excerpt,
-- license_kid, follow target ref — keyed by short stable strings.
-- The notification renderer in the frontend reads these by key.
-- JSONB so adding new verbs in follow-on sub-phases (I DMs,
-- L resource_requests) is additive without further migrations.
--
-- Read/delivered/email_sent split: in-app delivery is recorded at
-- INSERT time (the row landing IS the delivery). read_at flips when
-- the user opens the notifications panel + clicks. email_sent_at
-- stays NULL until the email-channel job runs (deferred — Phase
-- I2-b lands the job, this migration just reserves the column).
--
-- Federation: origin_server_id carries the same NULL-default
-- federation-prep column every per-user table in this arc has
-- grown.

-- +goose Up

CREATE TABLE notifications (
    id                 UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    recipient_user_ref BIGINT       NOT NULL,
    actor_user_ref     BIGINT       NULL,
    verb               TEXT         NOT NULL,
    target_kind        TEXT         NULL,
    target_id          TEXT         NULL,
    payload            JSONB        NOT NULL DEFAULT '{}'::jsonb,
    read_at            TIMESTAMPTZ  NULL,
    delivered_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    email_sent_at      TIMESTAMPTZ  NULL,
    origin_server_id   UUID         NULL,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Inbox-page query: cursor-paginated by (created_at DESC, id DESC)
-- per recipient. Covering index keeps the page render index-only
-- for the (verb, payload, ...) fields the renderer actually reads.
CREATE INDEX idx_notifications_recipient_recent
    ON notifications (recipient_user_ref, created_at DESC, id DESC);

-- Unread badge count: the bell on every page render. Partial index
-- so we don't carry every row in the count's working set — only
-- the unread tail. On a steady-state install the unread set is tiny
-- relative to the lifetime notifications table.
CREATE INDEX idx_notifications_unread
    ON notifications (recipient_user_ref)
    WHERE read_at IS NULL;

-- Actor-side audit / abuse investigation ("show every notification
-- this user generated in the last day"). Not hot-path; covers the
-- admin moderation workflow.
CREATE INDEX idx_notifications_actor
    ON notifications (actor_user_ref, created_at DESC)
    WHERE actor_user_ref IS NOT NULL;

COMMENT ON TABLE notifications IS
    'Per-user in-app notification feed. One row per (recipient, '
    'event). Verb taxonomy mirrors userprefs.KnownEventTypes — drift '
    'between them breaks the prefs UI.';

COMMENT ON COLUMN notifications.payload IS
    'Typed per-verb extra data. Schema documented in '
    'app/internal/notifications/events.go alongside the verb consts.';

COMMENT ON COLUMN notifications.delivered_at IS
    'Set at insert — in-app delivery is the row landing. Stays NOT '
    'NULL so the index is small. The email-channel side uses '
    'email_sent_at separately so a delivery problem in one channel '
    'never blocks the other.';

-- +goose Down

DROP TABLE notifications;
