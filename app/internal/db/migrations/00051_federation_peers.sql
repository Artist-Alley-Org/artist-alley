-- artist-alley migration 00051 — federation peers registry.
-- Phase 1.22.B-a, feat/user-surfaces.
--
-- The trusted-peers list — every artist-alley instance this
-- install will exchange federated activities with. Per ADR 0043
-- §"Trust model": pairing a peer establishes a trusted
-- communication channel — nothing more. No content shares.
-- federation_shares (Phase 1.22.C) is the load-bearing access
-- control layer; this table is just the registry of WHO can
-- talk.
--
-- Two sources of rows:
--   - Manual admin entry (1.22.B-a, this migration): operator
--     types in instance_url + display_name + instance_public_key
--     (pasted from the peer's actor doc) + initial trust_tier.
--     Both ends do this manually for now.
--   - Handshake protocol (1.22.B-b): one side initiates, the
--     other side accepts or rejects. Same schema; the row
--     transitions through state via the handshake state machine.
--
-- Caching: every inbound federation activity (Phase 1.22.D)
-- looks up the source peer by instance_url; every outbound
-- delivery iterates the enabled-peers list. Both are hot reads;
-- federation_peers gets a per-URL cache + a snapshot cache for
-- "all enabled peers" via cache.Registry NOTIFY (consistent
-- across federated processes).
--
-- The CHECK constraints mirror the typed Go constants in
-- federation.TrustTier + federation.EncryptionPolicy per
-- ADR 0042. Drift between this DDL and those types is a code-
-- review block.

-- +goose Up

CREATE TABLE federation_peers (
    id                       UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    -- The peer's instance URL — base URL without a trailing
    -- slash, e.g. "https://studio-b.example". UNIQUE because one
    -- row per peer (handshake re-issue updates the existing row,
    -- doesn't insert a duplicate).
    instance_url             TEXT         NOT NULL UNIQUE,

    -- Display name for the admin UI — operator-chosen,
    -- separate from the peer's site name (which may change).
    display_name             TEXT         NOT NULL,

    -- The peer's instance-level Ed25519 public key, PEM-wrapped
    -- per RFC 8410. Distinct from any per-actor key — this is the
    -- key the peer's *instance* uses for handshake + (future)
    -- HTTP-Sig transport auth on inbox POSTs. Per
    -- docs/spec/federation/v1.md §11 the per-actor key + the
    -- per-instance key are different key spaces; this column
    -- holds the latter.
    instance_public_key      TEXT         NOT NULL,

    -- Trust tier per ADR 0043 §"Trust model". CHECK mirrors
    -- federation.TrustTier:
    --   connected: standard tier; activities about explicitly-
    --     shared objects flow both ways.
    --   directory-listed: opt-in to the future artist-alley.org
    --     curated directory (Phase 1.22.H).
    --   auto-sync: per-peer opt-in for instances inside a single
    --     trust domain (HQ ↔ remote studio) that want certain
    --     object classes to share automatically via saved
    --     policies (Phase 1.22.J). Never the default.
    trust_tier               TEXT         NOT NULL DEFAULT 'connected'
                             CHECK (trust_tier IN ('connected', 'directory-listed', 'auto-sync')),

    -- Encryption policy per ADR 0043 §"Encryption tier" —
    -- orthogonal to trust_tier. CHECK mirrors
    -- federation.EncryptionPolicy:
    --   plaintext: plain payloads for public/team-tier content
    --     are fine; restricted/embargo content still gets
    --     NaCl-box-encrypted per ADR 0020 + v1.md §6.5.
    --   e2e-encrypted: force NaCl-box envelopes for ALL
    --     restricted/embargo content over this peer link,
    --     regardless of per-activity override.
    encryption_policy        TEXT         NOT NULL DEFAULT 'plaintext'
                             CHECK (encryption_policy IN ('plaintext', 'e2e-encrypted')),

    -- Soft kill-switch. Disabled peers are still in the table
    -- (audit + share-row references) but federation outbox skips
    -- delivery + federation inbox rejects inbound activities
    -- (federation_inbox.status = 'peer_disabled' per the v1.md
    -- §12.1 status catalog).
    enabled                  BOOLEAN      NOT NULL DEFAULT TRUE,

    -- Handshake provenance. handshake_at is when this row was
    -- last successfully paired (manual entry counts as a
    -- handshake at insert time); handshake_by_user_ref is the
    -- local admin who completed the pairing. last_seen_at is
    -- updated whenever we receive a valid signed request from
    -- the peer (Phase 1.22.D inbox; column reserved for now).
    handshake_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    handshake_by_user_ref    BIGINT       NOT NULL,
    last_seen_at             TIMESTAMPTZ  NULL,

    -- Operator notes (free text — "Studio B's main HQ install",
    -- "blocked due to ToS violation 2026-Q2", etc.).
    notes                    TEXT         NOT NULL DEFAULT '',

    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Hot read path #1: inbound activity dispatcher looks up the
-- source peer by instance_url to authenticate the request +
-- check the enabled flag. instance_url already has the UNIQUE
-- index implicit; no extra index needed.

-- Hot read path #2: outbound delivery worker iterates enabled
-- peers to know who can receive activities. Partial index
-- WHERE enabled keeps the working set bounded to live peers.
CREATE INDEX federation_peers_enabled_idx
    ON federation_peers (instance_url)
    WHERE enabled = TRUE;

-- Admin audit: list peers ordered by recency (newly-paired at
-- top). Not strictly hot-path but the admin UI default render.
CREATE INDEX federation_peers_handshake_at_idx
    ON federation_peers (handshake_at DESC);

COMMENT ON TABLE federation_peers IS
    'Trusted-peers registry (ADR 0043 §"Trust model"). One row '
    'per artist-alley instance this install will exchange '
    'federated activities with. Pairing alone shares no content '
    '— federation_shares (1.22.C) is the per-object access '
    'control layer.';

COMMENT ON COLUMN federation_peers.instance_public_key IS
    'Peer''s instance-level Ed25519 public key (PEM, RFC 8410). '
    'Distinct from per-actor keys (those live on user rows per '
    'migration 00048). Used for handshake + transport-layer '
    'HTTP-Sig auth.';

COMMENT ON COLUMN federation_peers.last_seen_at IS
    'Updated by the Phase 1.22.D inbox when we receive a valid '
    'signed request from this peer. NULL means never seen a '
    'live request (manually-entered peer with no traffic yet).';

-- +goose Down

DROP TABLE federation_peers;
