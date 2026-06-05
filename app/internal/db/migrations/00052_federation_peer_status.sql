-- artist-alley migration 00052 — federation peer status column.
-- Phase 1.22.B-b, feat/user-surfaces.
--
-- Adds the handshake state machine to federation_peers per the
-- v1 handshake protocol (docs/spec/federation/v1.md §11, written
-- alongside this migration):
--
--   pending_outbound  — we sent the handshake POST to the peer;
--                       awaiting their admin's accept
--   pending_inbound   — we received a handshake POST; awaiting
--                       OUR admin's accept (TOFU + manual review)
--   connected         — both sides confirmed; full federation
--
-- Manual peer entry from Phase 1.22.B-a always lands as
-- 'connected' (the operator pasted both URL + pubkey out-of-band,
-- which IS the confirmation step). Auto-handshake from 1.22.B-b
-- transitions through the pending states.
--
-- Outbound delivery (Phase 1.22.D) gates on
-- (enabled AND status = 'connected'). Pending peers don't
-- receive activities; they exist for admin review only.
--
-- The CHECK constraint mirrors federation.PeerStatus per ADR 0042.

-- +goose Up

ALTER TABLE federation_peers
    ADD COLUMN status TEXT NOT NULL DEFAULT 'connected'
        CHECK (status IN ('pending_outbound', 'pending_inbound', 'connected'));

-- Pending-inbound feed for the admin UI ("requests awaiting your
-- approval"). Partial index keeps it small — most of the table
-- is connected/connected.
CREATE INDEX federation_peers_pending_inbound_idx
    ON federation_peers (handshake_at DESC)
    WHERE status = 'pending_inbound';

COMMENT ON COLUMN federation_peers.status IS
    'Handshake state machine per docs/spec/federation/v1.md §11. '
    'Mirrors federation.PeerStatus typed catalogue per ADR 0042. '
    'Outbound delivery (1.22.D) gates on (enabled AND status=connected).';

-- Refine the enabled_idx (set up in migration 00051) to also
-- gate on status. The hot read path "give me peers that can
-- receive activities" wants both flags applied at the index
-- level so the partial index covers the predicate exactly.
DROP INDEX federation_peers_enabled_idx;
CREATE INDEX federation_peers_enabled_idx
    ON federation_peers (instance_url)
    WHERE enabled = TRUE AND status = 'connected';

-- +goose Down

DROP INDEX IF EXISTS federation_peers_enabled_idx;
CREATE INDEX federation_peers_enabled_idx
    ON federation_peers (instance_url)
    WHERE enabled = TRUE;

DROP INDEX IF EXISTS federation_peers_pending_inbound_idx;
ALTER TABLE federation_peers DROP COLUMN status;
