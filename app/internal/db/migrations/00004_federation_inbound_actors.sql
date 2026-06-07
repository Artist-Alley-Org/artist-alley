-- artist-alley migration 00004 — inbound-federation columns
-- on likes + comments + the federation_remote_actors display
-- cache. Phase 1.22.D-a-4.
--
-- Pattern (per 1.22.D design proposal §5.5 dispatch refinement
-- 1): nullable peer_id + actor_uri on the existing domain
-- tables. NULL = local user; non-null = remote. The unified
-- read query gets you "all likes on this post" regardless of
-- origin, with no per-row branch.
--
-- CHECK invariant per ADR 0042 + 0046: exactly one of
-- {local_user, remote_pair} is set. Eliminates the "both NULL"
-- (orphan row) and "both set" (semantic ambiguity) cases at
-- the DB layer so handler code doesn't need defensive branches.
--
-- Dispatch idempotency (refinement 2): the inbox layer dedups
-- on activity_uri UNIQUE before dispatch fires, so under
-- normal operation each handler runs at most once per activity.
-- BUT the dispatch goroutine retries on transient domain-side
-- failures (DB lock contention etc.) so the INSERTs are
-- defensively keyed:
--
--   likes.local_uniq    UNIQUE (target_kind, target_id, user_ref)
--                       WHERE user_ref IS NOT NULL
--   likes.remote_uniq   UNIQUE (target_kind, target_id, peer_id, actor_uri)
--                       WHERE peer_id IS NOT NULL
--   comments.activity_uri UNIQUE WHERE activity_uri IS NOT NULL
--
-- Picking the conflict-key shape now means we never refactor a
-- unique-index migration on a populated table later.

-- +goose Up

-- --- likes -------------------------------------------------------------

-- 1) Drop the old (target_kind, target_id, user_ref) PK FIRST —
-- relaxing user_ref to nullable is blocked while it's part of a
-- PRIMARY KEY (Postgres 42P16). The old tuple is reinstated as
-- a partial UNIQUE index below so local uniqueness survives.
ALTER TABLE likes DROP CONSTRAINT likes_pkey;

-- 2) New columns. user_ref can now relax to nullable; id added
-- as the proper UUID PK; peer_id + actor_uri added for remote.
ALTER TABLE likes
    ADD COLUMN id          UUID         NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN peer_id     UUID         NULL REFERENCES federation_peers(id) ON DELETE CASCADE,
    ADD COLUMN actor_uri   TEXT         NULL,
    ALTER COLUMN user_ref  DROP NOT NULL;

ALTER TABLE likes ADD CONSTRAINT likes_pkey PRIMARY KEY (id);

-- 3) Partial UNIQUE indexes for the two per-origin idempotency
-- keys.
CREATE UNIQUE INDEX likes_local_uniq_idx
    ON likes (target_kind, target_id, user_ref)
    WHERE user_ref IS NOT NULL;

CREATE UNIQUE INDEX likes_remote_uniq_idx
    ON likes (target_kind, target_id, peer_id, actor_uri)
    WHERE peer_id IS NOT NULL;

-- 4) Origin-pair CHECK: exactly one of {local, remote} per row.
-- Pre-MVP — no migration ladder for existing rows; the constraint
-- holds because every existing row has user_ref set + the new
-- columns default NULL.
ALTER TABLE likes ADD CONSTRAINT likes_origin_check
    CHECK (
        (user_ref IS NOT NULL AND peer_id IS NULL AND actor_uri IS NULL)
        OR
        (user_ref IS NULL AND peer_id IS NOT NULL AND actor_uri IS NOT NULL)
    );

-- --- comments ----------------------------------------------------------

-- Same shape but comments already have an id PK + author_user_ref
-- is the only field we need to nullable. Plus activity_uri for
-- the dedup-on-source-envelope-id idempotency key.
ALTER TABLE comments
    ADD COLUMN peer_id            UUID NULL REFERENCES federation_peers(id) ON DELETE CASCADE,
    ADD COLUMN actor_uri          TEXT NULL,
    ADD COLUMN activity_uri       TEXT NULL,
    ALTER COLUMN author_user_ref  DROP NOT NULL;

CREATE UNIQUE INDEX comments_activity_uri_uniq_idx
    ON comments (activity_uri)
    WHERE activity_uri IS NOT NULL;

ALTER TABLE comments ADD CONSTRAINT comments_origin_check
    CHECK (
        (author_user_ref IS NOT NULL AND peer_id IS NULL AND actor_uri IS NULL)
        OR
        (author_user_ref IS NULL AND peer_id IS NOT NULL AND actor_uri IS NOT NULL)
    );

-- --- federation_remote_actors display cache ----------------------------

-- Display info for remote actors surfaced in UI. The inbound
-- dispatch handler upserts on every activity from a given actor
-- (so display fields refresh naturally on each interaction).
-- Read path joins likes / comments → federation_remote_actors
-- in one query, avoiding N+1.
--
-- Keyed on actor_uri (globally unique by the spec §8.3 URI
-- shape). peer_id is denormalized for the by-peer admin view +
-- the FK CASCADE: defederation drops every cached actor record
-- alongside the peer row.
CREATE TABLE federation_remote_actors (
    actor_uri         TEXT         PRIMARY KEY,
    peer_id           UUID         NOT NULL
        REFERENCES federation_peers(id) ON DELETE CASCADE,
    display_name      TEXT         NOT NULL DEFAULT '',
    avatar_url        TEXT         NOT NULL DEFAULT '',
    first_seen_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_seen_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX federation_remote_actors_by_peer_idx
    ON federation_remote_actors (peer_id, last_seen_at DESC);

COMMENT ON TABLE federation_remote_actors IS
    'Display cache for remote actors surfaced in UI. The inbound '
    'dispatch handler upserts on every activity from a remote '
    'actor; display fields refresh naturally on each interaction. '
    'Keyed on actor_uri (globally unique per spec §8.3).';

-- +goose Down

DROP TABLE federation_remote_actors;

ALTER TABLE comments DROP CONSTRAINT comments_origin_check;
DROP INDEX comments_activity_uri_uniq_idx;
ALTER TABLE comments ALTER COLUMN author_user_ref SET NOT NULL;
ALTER TABLE comments DROP COLUMN activity_uri;
ALTER TABLE comments DROP COLUMN actor_uri;
ALTER TABLE comments DROP COLUMN peer_id;

ALTER TABLE likes DROP CONSTRAINT likes_origin_check;
DROP INDEX likes_remote_uniq_idx;
DROP INDEX likes_local_uniq_idx;
ALTER TABLE likes DROP CONSTRAINT likes_pkey;
-- Restore the original tuple PK. ALTER COLUMN SET NOT NULL would
-- fail if any rows have NULL user_ref; the Down assumes a fresh
-- state (pre-MVP volatility).
ALTER TABLE likes ALTER COLUMN user_ref SET NOT NULL;
ALTER TABLE likes DROP COLUMN actor_uri;
ALTER TABLE likes DROP COLUMN peer_id;
ALTER TABLE likes DROP COLUMN id;
ALTER TABLE likes ADD CONSTRAINT likes_pkey PRIMARY KEY (target_kind, target_id, user_ref);
