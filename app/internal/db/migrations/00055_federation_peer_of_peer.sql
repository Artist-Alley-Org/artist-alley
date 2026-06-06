-- artist-alley migration 00055 — peer-of-peer discovery.
-- Phase 1.22.B-d, feat/user-surfaces.
--
-- Two additions for the "trust-laundered suggestions through the
-- social graph" feature:
--
--   1. federation_peers.share_in_visible_list — opt-in toggle per
--      peer. When TRUE, this peer appears in the response to
--      GET /federation/peers/visible (the public endpoint other
--      peers query when asking "who do you federate with?").
--      Default FALSE — sharing is opt-in.
--
--   2. federation_peer_suggestions — cache of what OUR peers told
--      us they federate with. When admin clicks "refresh
--      suggestions", we walk each connected+enabled peer, hit
--      their /federation/peers/visible, and upsert the results
--      here. Dedup against our own federation_peers happens at
--      query time so a peer we're already paired with doesn't
--      pollute the suggestions list.
--
-- Per docs/spec/federation/v1.md §"Peer-of-peer discovery" the
-- subscriber MUST present suggestions with provenance ("via Studio
-- B"); source_peer_id is the FK that records which of our peers
-- contributed each suggestion.

-- +goose Up

ALTER TABLE federation_peers
    ADD COLUMN share_in_visible_list BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN federation_peers.share_in_visible_list IS
    'Opt-in: include this peer in the response to '
    'GET /federation/peers/visible. Default FALSE — operators '
    'must explicitly enable sharing per pair. The endpoint '
    'returns the union of all peers with this flag set.';

-- The "visible-peers snapshot" hot read (called by other peers
-- on every refresh) wants the union of (enabled AND status=connected
-- AND share_in_visible_list). Partial index keeps it bounded to
-- the actually-visible set even if the table grows.
CREATE INDEX federation_peers_visible_idx
    ON federation_peers (instance_url)
    WHERE enabled = TRUE AND status = 'connected' AND share_in_visible_list = TRUE;

CREATE TABLE federation_peer_suggestions (
    id                     UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Which of OUR peers contributed this suggestion. ON DELETE
    -- CASCADE so unpairing a peer drops their suggestions too —
    -- we don't surface stale recommendations from a defederated
    -- source.
    source_peer_id         UUID         NOT NULL REFERENCES federation_peers(id) ON DELETE CASCADE,

    -- The suggested peer's properties as reported by the source.
    -- We don't yet contact the suggested peer to verify — that's
    -- the operator's job (clicking "Pair" kicks off the existing
    -- handshake which IS the trust verification).
    suggested_url          TEXT         NOT NULL,
    suggested_display_name TEXT         NOT NULL,
    suggested_public_key   TEXT         NOT NULL,
    suggested_fingerprint  TEXT         NOT NULL,

    -- When we last heard about this suggestion from the source.
    -- Admin UI shows staleness; a suggestion the source has
    -- since dropped from their visible list ages out of our
    -- cache via DeleteSuggestionsNotIn (mirrors the directory-
    -- entries refresh pattern).
    cached_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    -- One row per (source, suggested URL). Re-fetching the same
    -- source overwrites in place.
    UNIQUE (source_peer_id, suggested_url)
);

COMMENT ON TABLE federation_peer_suggestions IS
    'Locally cached peer-of-peer suggestions per ADR 0043. '
    'Each row records: a peer we trust (source) told us about '
    'another peer (suggested) we should consider pairing with. '
    'Pairing still goes through the handshake — these are advisory.';

CREATE INDEX federation_peer_suggestions_by_source_idx
    ON federation_peer_suggestions (source_peer_id, cached_at DESC);

-- Hot read for the admin "suggestions" feed: looks up by suggested
-- URL to dedup against our own peers table. instance_url-style
-- index keeps the join cheap.
CREATE INDEX federation_peer_suggestions_by_url_idx
    ON federation_peer_suggestions (suggested_url);

-- +goose Down

DROP TABLE federation_peer_suggestions;
DROP INDEX IF EXISTS federation_peers_visible_idx;
ALTER TABLE federation_peers DROP COLUMN share_in_visible_list;
