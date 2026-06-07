-- artist-alley migration 00003 — federation_inbox table.
-- Phase 1.22.D-a, feat/federation-inbox-outbox.
--
-- One row per inbound activity received over the federation
-- inbox endpoint (POST /federation/inbox). Sender's envelope
-- `id` field becomes activity_uri + carries the UNIQUE
-- constraint that prevents replay even if HTTP-Signatures
-- date-skew check is bypassed (see docs/spec/federation/v1.md
-- §10 + the 1.22.D design proposal §5.5 addition B).
--
-- Pipeline per the design proposal §2.2 — stages 10+11 INSERT
-- this row (status='pending'); the background worker stages
-- 12+13 then update to 'processed' / 'rejected' / 'failed'.

-- +goose Up

CREATE TABLE federation_inbox (
    id                       UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Sender's canonical envelope id, dedup key. UNIQUE makes
    -- replay protection a DB invariant rather than a logical
    -- check (§5.5 addition B).
    activity_uri             TEXT         NOT NULL UNIQUE,

    -- Sending peer (resolved at pipeline stage 3). CASCADE so
    -- a defederated peer's inbox history drops with the row.
    peer_id                  UUID         NOT NULL
        REFERENCES federation_peers(id) ON DELETE CASCADE,

    -- Captured envelope fields for dispatch + audit. Verbatim
    -- so a re-dispatch never needs to refetch from the peer.
    actor_uri                TEXT         NOT NULL,
    activity_type            TEXT         NOT NULL,
    object_kind              TEXT         NULL,
    object_id                UUID         NULL,
    envelope_json            JSONB        NOT NULL,
    http_sig_key             TEXT         NOT NULL,

    -- Receipt + dispatch state.
    received_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    status                   TEXT         NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'processed', 'rejected', 'failed')),

    -- §12.1 closed catalogue when status='rejected'. Mirrors
    -- federation.InboxStatus in app/internal/federation/vocab.go;
    -- pre-existing constants reused (no new strings introduced
    -- per ADR 0042).
    reject_reason            TEXT         NULL,

    dispatch_attempts        INT          NOT NULL DEFAULT 0,
    last_attempt_at          TIMESTAMPTZ  NULL,
    last_error               TEXT         NOT NULL DEFAULT '',
    processed_at             TIMESTAMPTZ  NULL,

    -- Audit chain back to the dispatched/projected activities
    -- row when dispatch succeeds. NULL until the worker writes
    -- the projection (or forever if rejected at the gate).
    correlation_activity_id  UUID         NULL
        REFERENCES activities(id) ON DELETE SET NULL,

    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE federation_inbox IS
    'One row per inbound federation activity. Pipeline ingest '
    'persists status=pending; background worker transitions to '
    'processed/rejected/failed per §2.2 of the 1.22.D design '
    'proposal. activity_uri UNIQUE is the load-bearing replay '
    'guard.';

-- Hot read #1: worker pulls pending rows for dispatch.
CREATE INDEX federation_inbox_pending_idx
    ON federation_inbox (received_at)
    WHERE status = 'pending';

-- Hot read #2: admin per-peer view.
CREATE INDEX federation_inbox_by_peer_idx
    ON federation_inbox (peer_id, received_at DESC);

-- Hot read #3: admin status filter (failed-needing-review).
CREATE INDEX federation_inbox_by_status_idx
    ON federation_inbox (status, received_at DESC);

-- +goose Down

DROP TABLE federation_inbox;
