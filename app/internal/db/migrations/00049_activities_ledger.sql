-- artist-alley migration 00049 — activities ledger.
-- Phase 1.22.A-bis-1, feat/user-surfaces.
--
-- The canonical record of every federated social action. AP-shape
-- per docs/spec/federation/v1.md §3. ADR 0044 codifies the CQRS-
-- lite invariant: this table is the source of truth for every
-- federated action; the domain tables (posts, comments, likes,
-- user_follows, user_blocks, direct_messages) remain as optimized
-- read projections kept in sync SYNCHRONOUSLY, in the same
-- database transaction as the activity insert.
--
-- One ledger, two sources:
--   - source = 'local' rows are emitted by handlers on this
--     instance; they feed the federation outbox (Phase 1.22.D).
--   - source = '<instance URL>' rows are received from federated
--     peers; they feed inbox dispatch (Phase 1.22.D) + drive the
--     side-effect updates to local projections.
--
-- Idempotency: activity_uri is UNIQUE. Re-recording the same
-- activity (job retry, peer redelivery, replay tool) is a no-op
-- via ON CONFLICT DO NOTHING in the writer.
--
-- Transactional emit: the writer requires a pgx.Tx, not a pool.
-- Handlers wrap their existing domain write + RecordActivity in
-- one transaction. Either both succeed and commit, or both fail
-- and rollback. No silent split-brain between the domain row and
-- its activity.
--
-- The CHECK constraints mirror the typed Go constants in
-- internal/federation/vocab.go (activity_type) and the new
-- internal/activities/ package (object_kind, source pattern) per
-- ADR 0042. Drift between this migration and those types is a
-- code-review block.

-- +goose Up

CREATE TABLE activities (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- AP identity. activity_uri is the cross-instance handle
    -- per docs/spec/federation/v1.md §8.1
    -- (https://{instance}/activities/{uuid}). UNIQUE for idempotent
    -- re-emission.
    activity_uri       TEXT NOT NULL UNIQUE,
    activity_type      TEXT NOT NULL CHECK (activity_type IN (
        -- Standard AS2 activities (per W3C AS2 Vocabulary).
        'Create', 'Update', 'Delete',
        'Follow', 'Accept', 'Reject',
        'Undo', 'Like', 'Announce', 'Block',
        -- Custom artist-alley activities (per ADR 0043 §"Custom activity types").
        'aa:Share', 'aa:Unshare',
        'aa:Approve', 'aa:RequestChanges', 'aa:MarkReviewed',
        'aa:Annotation', 'aa:WorkflowTransition', 'aa:AssetVersion',
        'aa:Subscribe', 'aa:Mention'
    )),

    -- Actor. actor_uri is the canonical handle; actor_user_ref is
    -- the local-user shortcut (NULL for peer-originated activities).
    actor_uri          TEXT NOT NULL,
    actor_user_ref     BIGINT NULL,

    -- Object. object_uri is the AP handle; object_kind + object_local_id
    -- is the local-lookup shortcut. object_kind is broader than
    -- federation_shares.object_kind (it includes 'comment',
    -- 'message', 'activity' for Undo targets).
    object_uri         TEXT NULL,
    object_kind        TEXT NULL CHECK (object_kind IS NULL OR object_kind IN (
        'post', 'comment', 'asset', 'user', 'collection',
        'workspace', 'brand_kit', 'message', 'activity'
    )),
    object_local_id    TEXT NULL,

    -- Target. Used by Add / Remove activities (the collection
    -- the object is added to / removed from). NULL for most
    -- activity types.
    target_uri         TEXT NULL,

    -- Addressing (AP §6.1). JSONB arrays of actor URIs. bto/bcc
    -- are stripped before federation delivery per AP §6; we
    -- preserve them in the ledger row so the originating instance
    -- can still answer DSAR queries about who was originally
    -- addressed.
    to_uris            JSONB NOT NULL DEFAULT '[]'::jsonb,
    cc_uris            JSONB NOT NULL DEFAULT '[]'::jsonb,
    bto_uris           JSONB NOT NULL DEFAULT '[]'::jsonb,
    bcc_uris           JSONB NOT NULL DEFAULT '[]'::jsonb,
    audience_uris      JSONB NOT NULL DEFAULT '[]'::jsonb,

    -- Wire form. payload is the full AP envelope (the JSON object
    -- defined in docs/spec/federation/v1.md §3) WITHOUT the
    -- signature field. signature_value + signature_pubkey are
    -- populated on local rows after sign (Phase 1.22.D outbox
    -- delivery) and on peer rows at ingest time.
    payload            JSONB NOT NULL,
    signature_value    TEXT NULL,
    signature_pubkey   TEXT NULL,

    -- Source. 'local' or the originating peer's instance URL.
    -- The CHECK enforces shape but does NOT validate the URL is
    -- a known peer (that gate lives at write time, after the
    -- federation_peers lookup).
    source             TEXT NOT NULL DEFAULT 'local'
                       CHECK (source = 'local' OR source LIKE 'https://%'),

    -- Timing. published_at is the activity's logical timestamp
    -- (when the actor performed the action). For local rows it's
    -- set to NOW() at emit time; for peer rows it's the
    -- envelope's `published` field. created_at is the row's
    -- physical insert time (always local NOW()).
    published_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Outbox query (Phase 1.22.D). "What activities did this local
-- actor emit, newest first?" Partial on source='local' so the
-- index covers only outbox-relevant rows.
CREATE INDEX activities_actor_outbox_idx
    ON activities (actor_user_ref, published_at DESC, id DESC)
    WHERE source = 'local' AND actor_user_ref IS NOT NULL;

-- Per-object timeline. "Show me everything that's happened about
-- this post / asset / collection / etc." Powers the admin UI's
-- per-object audit drill-down.
CREATE INDEX activities_object_recent_idx
    ON activities (object_kind, object_local_id, published_at DESC)
    WHERE object_kind IS NOT NULL AND object_local_id IS NOT NULL;

-- Type-filtered. "Show me all Like activities" / "all aa:Share
-- activities". Powers per-type admin filters + per-type
-- aggregate counters.
CREATE INDEX activities_type_recent_idx
    ON activities (activity_type, published_at DESC);

-- Peer-sourced. "What came in from this peer?" Powers admin
-- per-peer audit drill-down + abuse investigation.
CREATE INDEX activities_source_recent_idx
    ON activities (source, published_at DESC)
    WHERE source <> 'local';

COMMENT ON TABLE activities IS
    'CQRS-lite activities ledger (ADR 0044). Canonical record of '
    'every federated social action. Domain tables (posts, comments, '
    'likes, user_follows, user_blocks, direct_messages) are kept '
    'in sync synchronously via same-transaction RecordActivity '
    'calls. If you mutate social state without emitting an '
    'activity, you wrote a bug.';

COMMENT ON COLUMN activities.activity_uri IS
    'Cross-instance handle per docs/spec/federation/v1.md §8.1. '
    'UNIQUE for idempotent re-emission (job retry, peer '
    'redelivery, replay tool).';

COMMENT ON COLUMN activities.source IS
    '''local'' for activities emitted by this instance; the peer '
    'instance URL (e.g. ''https://studio-b.example'') for peer-'
    'received activities.';

COMMENT ON COLUMN activities.payload IS
    'Full AP envelope (docs/spec/federation/v1.md §3) WITHOUT the '
    'signature field. The signature value sits in a separate '
    'column so we can update it post-sign without rewriting the '
    'envelope JSON.';

-- +goose Down

DROP TABLE activities;
