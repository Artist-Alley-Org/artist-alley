-- artist-alley migration 00045 — social graph (follows + blocks).
-- Phase 1.17.G2, feat/user-surfaces.
--
-- Directional follows (ArtStation-shape): follower → followee with a
-- since timestamp. "Friends" = mutual follow query, NOT a separate
-- accept/decline state (per scoping decision logged in memory
-- project_phase_1_17_inflight 2026-06-04).
--
-- Blocks are bidirectional in EFFECT (each side stops seeing the
-- other in feeds, profile pages, notifications) but stored
-- directionally (blocker → blocked) so we can show "who I blocked"
-- separately from "who blocked me" and so an unblock by one party
-- doesn't require the other to also unblock.
--
-- Federation: origin_server_id carries the same NULL-default
-- federation-prep column every per-user table grew during the 1.17
-- arc. Per memory project_federation_is_real.
--
-- Wiring this migration to the long-parked posts.handler.go:877
-- TODO lands in the same commit: visibility='followers' has been a
-- valid CHECK option on posts since Phase 1.13 but did nothing
-- until follows existed.

-- +goose Up

-- Directional follow edge.
CREATE TABLE user_follows (
    follower_user_ref BIGINT       NOT NULL,
    followee_user_ref BIGINT       NOT NULL,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    origin_server_id  UUID         NULL,
    PRIMARY KEY (follower_user_ref, followee_user_ref),
    CHECK (follower_user_ref <> followee_user_ref)
);

-- Reverse-lookup index: "who follows me?" + "how many followers do
-- I have?" — the followee column is the join key for both. PK
-- already covers (follower, followee) lookups so we don't need a
-- second (follower, ...) index.
CREATE INDEX idx_user_follows_followee
    ON user_follows (followee_user_ref, created_at DESC);

COMMENT ON TABLE user_follows IS
    'Directional follow edges. (follower, followee) is the natural '
    'key. Mutual follow = "friends," computed by query rather than '
    'modeled as a separate first-class state.';

-- Directional block edge. Blocks affect visibility in BOTH
-- directions at query time (the consumer joins out both blocker and
-- blocked sides) but persist directionally so unblock semantics
-- stay clear.
CREATE TABLE user_blocks (
    blocker_user_ref BIGINT       NOT NULL,
    blocked_user_ref BIGINT       NOT NULL,
    reason           TEXT         NULL,           -- optional private note for the blocker
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    origin_server_id UUID         NULL,
    PRIMARY KEY (blocker_user_ref, blocked_user_ref),
    CHECK (blocker_user_ref <> blocked_user_ref)
);

-- Reverse lookup: "who blocked me?" is a load-bearing query for
-- filtering my outgoing interactions (the UI shouldn't surface a
-- "Send DM" button against someone who blocked me; the writer
-- shouldn't deliver the message even if the UI mis-renders).
CREATE INDEX idx_user_blocks_blocked
    ON user_blocks (blocked_user_ref);

COMMENT ON TABLE user_blocks IS
    'Directional block edges. Blocker → blocked. Consumers check '
    'both directions when deciding visibility — A blocking B AND/OR '
    'B blocking A both hide content from each other.';

-- +goose Down

DROP TABLE user_blocks;
DROP TABLE user_follows;
