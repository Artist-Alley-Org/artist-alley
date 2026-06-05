-- artist-alley migration 00047 — direct messages (DMs).
-- Phase 1.17.I-a, feat/user-surfaces.
--
-- One-on-one peer-to-peer messages. Thread identity is implicit:
-- two users have ONE conversation regardless of who started it. We
-- compute the thread key on the fly as (LEAST(a,b), GREATEST(a,b))
-- — no separate threads table to maintain, no join to fetch the
-- peer list, and a single index covers both "messages I sent" and
-- "messages I received" lookups.
--
-- Permission-aware writes: the send-DM handler calls
-- social.HasBlockBetween before inserting; a block in either
-- direction rejects with 403. The DM also fires a
-- "direct_message_received" notification (Phase 1.17.I2) which
-- runs the same gate again — defense-in-depth.
--
-- Broadcasts (admin → audience) land in I-b via a separate
-- notifications fan-out path; this table is DM-only.
--
-- Federation: origin_server_id mirrors every per-user table in
-- this arc. The recipient-thread index keeps inbox queries fast
-- under federated growth where a single user might have peers on
-- many instances.

-- +goose Up

CREATE TABLE direct_messages (
    id                 UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    sender_user_ref    BIGINT       NOT NULL,
    recipient_user_ref BIGINT       NOT NULL,
    body               TEXT         NOT NULL,
    sent_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    read_at            TIMESTAMPTZ  NULL,
    origin_server_id   UUID         NULL,
    CHECK (sender_user_ref <> recipient_user_ref),
    CHECK (length(body) > 0)
);

-- Thread view: "all messages between me and peer X, ordered by
-- time." Composite index on (LEAST, GREATEST) is what we want
-- logically but Postgres doesn't index function calls of two
-- columns cheaply; instead we have two recipient-side indexes
-- and the query does the OR. The planner picks the right side per
-- direction.
CREATE INDEX idx_dm_recipient_recent
    ON direct_messages (recipient_user_ref, sent_at DESC);

CREATE INDEX idx_dm_sender_recent
    ON direct_messages (sender_user_ref, sent_at DESC);

-- Unread badge: total unread DMs for the envelope pill. Partial
-- index so it stays small no matter how big lifetime DMs grow.
CREATE INDEX idx_dm_unread
    ON direct_messages (recipient_user_ref)
    WHERE read_at IS NULL;

COMMENT ON TABLE direct_messages IS
    'One-on-one peer-to-peer DMs. Thread identity is implicit: '
    'the unordered (sender, recipient) pair.';

COMMENT ON COLUMN direct_messages.read_at IS
    'When the RECIPIENT opened the thread containing this message. '
    'Sender-side "delivered" / "read" indicators read this column '
    'across the WHERE recipient = peer set.';

-- +goose Down

DROP TABLE direct_messages;
