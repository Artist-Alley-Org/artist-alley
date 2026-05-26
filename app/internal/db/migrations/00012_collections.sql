-- artist-alley migration 00012 — collections (Phase 1.11.A).
--
-- Per ADR 0009: UUID-keyed collection with three orthogonal axes
-- (visibility / membership / lifecycle), per-membership TTL,
-- federation-prep via origin_server_id and stable owner_user_ref.
--
-- This first migration ships:
--   - collections                — the entity table, ALL ADR 0009 columns
--   - collection_resources       — the manual-membership join table
--
-- Deferred to 1.11.B/C:
--   - collection_queries         — query-membership backing (needs ADR 0010 search DSL)
--   - collection_grants          — per-user sharing
--   - collection_access_links    — public link tokens
--
-- Naming: RS ships a singular `collection` table that we baseline in
-- migration 00007. We use plural `collections` here so the two
-- coexist during the strangler-fig phase. PHP code still queries
-- RS's table; new Go code uses ours.

-- +goose Up

CREATE TABLE collections (
    id                UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_ref    BIGINT       NOT NULL,
    name              TEXT         NOT NULL,
    description       TEXT         NOT NULL DEFAULT '',
    visibility        TEXT         NOT NULL DEFAULT 'private'
                                   CHECK (visibility IN ('private','shared','public')),
    membership        TEXT         NOT NULL DEFAULT 'manual'
                                   CHECK (membership IN ('manual','query','hybrid')),
    expires_at        TIMESTAMPTZ  NULL,
    featured          BOOLEAN      NOT NULL DEFAULT FALSE,
    purpose           TEXT         NULL,
    origin_server_id  UUID         NULL,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX collections_owner_idx
    ON collections (owner_user_ref);
CREATE INDEX collections_expires_idx
    ON collections (expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX collections_featured_idx
    ON collections (featured) WHERE featured;
CREATE INDEX collections_visibility_idx
    ON collections (visibility);
CREATE INDEX collections_created_at_idx
    ON collections (created_at DESC);

COMMENT ON TABLE  collections IS
    'Phase 1.11 collection entity per ADR 0009. UUID-keyed; coexists with the legacy RS `collection` table during the strangler-fig phase.';
COMMENT ON COLUMN collections.visibility IS
    'private = owner only; shared = via collection_grants (future); public = anyone signed in.';
COMMENT ON COLUMN collections.membership IS
    'manual = collection_resources rows; query = collection_queries (1.11.B); hybrid = both ∪ excluded pins.';
COMMENT ON COLUMN collections.expires_at IS
    'TTL. NULL = permanent. Sweeper job (later phase) hard-deletes once NOW() passes this.';

CREATE TABLE collection_resources (
    collection_id  UUID         NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    asset_id       UUID         NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    sort_order     INTEGER      NOT NULL DEFAULT 0,
    pinned         BOOLEAN      NOT NULL DEFAULT TRUE,
    expires_at     TIMESTAMPTZ  NULL,
    added_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (collection_id, asset_id)
);

CREATE INDEX collection_resources_asset_idx
    ON collection_resources (asset_id);
CREATE INDEX collection_resources_expires_idx
    ON collection_resources (expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX collection_resources_sort_idx
    ON collection_resources (collection_id, sort_order);

COMMENT ON TABLE collection_resources IS
    'Manual membership of assets in collections. `pinned=false` will be used by hybrid mode (1.11.B) to record explicit exclusions from a query result; for manual-only collections every row is pinned=true.';
COMMENT ON COLUMN collection_resources.expires_at IS
    'Per-membership TTL — drops the asset from the collection without affecting the asset itself. The sweeper handles both this and the collections.expires_at column.';

-- +goose Down

DROP TABLE IF EXISTS collection_resources;
DROP TABLE IF EXISTS collections;
