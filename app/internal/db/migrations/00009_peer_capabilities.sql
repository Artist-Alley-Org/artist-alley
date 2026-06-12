-- artist-alley migration 00009 — federation_peers gains capability
-- columns for Phase 1.22.I-d (peer capability negotiation).
--
-- Two columns:
--
--   capabilities                jsonb NOT NULL DEFAULT '[]'
--   capabilities_negotiated_at  timestamptz NULL
--
-- # Design references
--
--   * ADR 0049 §Track B — Decision 3 ("Peer-handshake capability
--     advertisement"). The set is what BOTH peers concurrently
--     support, recorded after the handshake completes on each side.
--   * ADR 0042 — distributed catalogues. Capability vocabulary is
--     open on the wire (peers MAY advertise unknown values; we
--     record them but never dispatch on them) and closed in code
--     (federation/peer.KnownCapabilities is the dispatch set).
--
-- # Why JSONB
--
--   Not an enum: capability vocabulary is open — future versions
--   will add more (post-quantum KEM, compression, batched-inbox
--   variants). Enum would require a schema migration per addition.
--
--   Not a join table: over-engineered for a small bilateral
--   metadata blob. JSONB also gracefully accepts unknown
--   capabilities from peers — store the peer-side truth without
--   dispatching on it.
--
-- # Why DEFAULT '[]' and NOT NULL
--
--   Forces explicit consideration: a peer row with [] means "no
--   encryption negotiated" → the I-e + I-g dispatch gates treat
--   it as legacy. Avoids the trinary state (NULL / empty / populated)
--   that callers would have to handle at every check site.
--
-- # Why capabilities_negotiated_at distinct from the array
--
--   Differentiates two states that LOOK the same to a `capabilities = '[]'`
--   check:
--
--     1. "we negotiated and got nothing"  (peer truly supports no
--        capabilities — legal but rare). capabilities_negotiated_at
--        IS NOT NULL.
--     2. "we never negotiated"            (pre-I-d peer, never
--        re-paired since the migration ran). capabilities_negotiated_at
--        IS NULL. ListPeersMissingCapabilities surfaces these to
--        operators for re-pairing.

-- +goose Up

ALTER TABLE federation_peers
    ADD COLUMN capabilities                jsonb       NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN capabilities_negotiated_at  timestamptz NULL;

-- Operator-facing observability — backed by the partial index so
-- the admin federation surface can answer "which paired peers are
-- still pre-I-d?" cheaply regardless of total peer volume. Partial
-- because the populated state is the steady state.
CREATE INDEX federation_peers_unnegotiated_idx
    ON federation_peers (id)
    WHERE capabilities_negotiated_at IS NULL;

COMMENT ON COLUMN federation_peers.capabilities IS
    'Bilateral capability intersection (what BOTH peers support). '
    'JSONB array of typed strings per federation/peer.Capability. '
    'Open vocabulary on the wire, closed dispatch in code. '
    'See ADR 0049 §Track B Decision 3.';

COMMENT ON COLUMN federation_peers.capabilities_negotiated_at IS
    'When the handshake completed with capability exchange. NULL '
    'means "never negotiated" (pre-1.22.I-d peer); peers in that '
    'state are surfaced via ListPeersMissingCapabilities for operator '
    're-pairing. Distinct from `capabilities = ''[]''` which means '
    '"we negotiated and got an empty intersection" — also legal.';

-- +goose Down

DROP INDEX federation_peers_unnegotiated_idx;
ALTER TABLE federation_peers
    DROP COLUMN capabilities_negotiated_at,
    DROP COLUMN capabilities;
