-- artist-alley migration 00005 — federation_outbox + cursor
-- state + LISTEN/NOTIFY trigger on the activities ledger.
-- Phase 1.22.D-b-1.
--
-- Architecture: Option B (ledger-derived) with 5 refinements
-- per the design proposal §3.1 lock-in:
--
--   1. LISTEN/NOTIFY for sub-100ms latency in the happy path;
--      30s ticker as correctness backstop for missed events.
--   2. Cursor-based progress in federation_dispatch_state;
--      atomic cursor advance with outbox INSERTs in one tx.
--   3. Caching (federation/shares.cache + federation/follows.
--      cache) is at the Go layer — not in this migration.
--   4. UNIQUE (activity_id, peer_id, target_user_url) partial
--      indexes for ON CONFLICT DO NOTHING idempotency.
--   5. Sender-side emission refusal for restricted/embargo
--      lives at the dispatcher; this migration just provides
--      the storage shape.

-- +goose Up

-- --- federation_outbox -----------------------------------------------

CREATE TABLE federation_outbox (
    id                     UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    -- The activity row this outbox entry derives from (ADR 0044
    -- ledger). FK CASCADE: deleting an activity drops its
    -- outbox rows. The dispatcher rebuilds the envelope from
    -- the activity at send time so we don't duplicate body
    -- bytes here.
    activity_id            UUID         NOT NULL
        REFERENCES activities(id) ON DELETE CASCADE,

    -- Recipient peer. FK CASCADE: defederation drops queued
    -- rows alongside the peer row (per ADR 0043 §1.22.C-e the
    -- cascade also sets status='cancelled' first, but the FK
    -- is the belt-and-braces fallback).
    peer_id                UUID         NOT NULL
        REFERENCES federation_peers(id) ON DELETE CASCADE,

    -- Per-recipient target URL within the peer (e.g. an actor's
    -- inbox URL for Follow / Like / Comment activities). NULL
    -- means "broadcast to the peer's instance" (rare — most
    -- activities have a specific recipient actor). Together
    -- with activity_id + peer_id this is the idempotency key.
    target_user_url        TEXT         NULL,

    status                 TEXT         NOT NULL DEFAULT 'queued',
    CONSTRAINT federation_outbox_status_check CHECK (
        status IN ('queued', 'sent', 'failed', 'cancelled')
    ),

    -- Backoff schedule per design §3.4: instant / +30s / +5m /
    -- +1h / +6h then status='failed' at attempt cap 5.
    attempts               SMALLINT     NOT NULL DEFAULT 0,
    next_attempt_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_attempt_at        TIMESTAMPTZ  NULL,
    last_error             TEXT         NOT NULL DEFAULT '',

    -- Set on transition to 'sent'. NULL means "not yet
    -- delivered." Admin UI surfaces the latency
    -- (sent_at - created_at) per the 1.22.D-c queue metrics.
    sent_at                TIMESTAMPTZ  NULL,

    -- HTTP-Sig key the dispatcher signed the outbound POST
    -- with. Captured for forensics — if the recipient rejects
    -- with sig_invalid we need to know which key tried to sign.
    -- NULL until first delivery attempt.
    delivered_with_key_id  TEXT         NULL,

    created_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Partial UNIQUE indexes for ON CONFLICT DO NOTHING
-- idempotency (refinement 4). Postgres requires the WHERE
-- predicate to MATCH the index for the ON CONFLICT inference
-- to fire, so the dispatcher SQL repeats the predicate.
CREATE UNIQUE INDEX federation_outbox_dedup_targeted_idx
    ON federation_outbox (activity_id, peer_id, target_user_url)
    WHERE target_user_url IS NOT NULL;
CREATE UNIQUE INDEX federation_outbox_dedup_broadcast_idx
    ON federation_outbox (activity_id, peer_id)
    WHERE target_user_url IS NULL;

-- Hot-path index: the delivery worker reads "queued rows whose
-- next_attempt_at <= NOW(), ordered by next_attempt_at." Partial
-- on status='queued' keeps the index lean — sent/failed rows
-- shouldn't show up in the scan.
CREATE INDEX federation_outbox_due_idx
    ON federation_outbox (next_attempt_at)
    WHERE status = 'queued';

-- Per-peer admin view: "what's queued / failed / sent recently
-- for peer X?" — ordered by created_at DESC.
CREATE INDEX federation_outbox_by_peer_idx
    ON federation_outbox (peer_id, created_at DESC);

-- --- federation_dispatch_state ---------------------------------------

-- Single-row cursor table. Holds the activity_id of the last
-- activity the dispatcher has fanned out into outbox rows. On
-- restart the dispatcher reads this cursor + queries
-- `activities WHERE created_at > cursor's created_at OR
--  (created_at = cursor's created_at AND id > cursor's id)`
-- so it's tolerant of multi-row-same-timestamp orderings.
--
-- One row, primary-keyed on a fixed sentinel so concurrent
-- updates serialize cleanly under SELECT ... FOR UPDATE.
CREATE TABLE federation_dispatch_state (
    id                            INT          PRIMARY KEY CHECK (id = 1),
    last_dispatched_activity_id   UUID         NULL,
    last_dispatched_at            TIMESTAMPTZ  NULL,
    updated_at                    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
-- Seed the single row. last_dispatched_activity_id NULL means
-- "no activities dispatched yet" — the first tick processes
-- everything from the beginning of the activities table.
INSERT INTO federation_dispatch_state (id) VALUES (1);

-- --- LISTEN/NOTIFY trigger -------------------------------------------

-- pg_notify on every activities INSERT. The dispatcher arms a
-- LISTEN federation_dispatch_pending and processes the
-- payload's activity_id within ms of the commit. The 30s
-- ticker backstop (Go-side) catches anything LISTEN missed.
--
-- pgPL/pgSQL footprint stays trivial: no business logic — just
-- the notify. Recipient resolution + emission-refusal logic
-- lives in Go where sqlc + tests + diff review apply.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION federation_dispatch_notify()
RETURNS TRIGGER AS $$
BEGIN
    PERFORM pg_notify('federation_dispatch_pending', NEW.id::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER federation_dispatch_notify_trg
    AFTER INSERT ON activities
    FOR EACH ROW
    EXECUTE FUNCTION federation_dispatch_notify();

COMMENT ON TABLE federation_outbox IS
    'Per-recipient outbound queue. Derived from activities '
    'ledger via the dispatcher (Phase 1.22.D-b). Idempotent on '
    '(activity_id, peer_id, target_user_url) so the dispatcher '
    'is re-runnable. Sender-side emission refusal for '
    'restricted/embargo content (until 1.22.I ships X25519) is '
    'enforced at the dispatcher; refused activities never get '
    'an outbox row, they emit federation.emission.skipped audit '
    'instead.';
COMMENT ON TABLE federation_dispatch_state IS
    'Single-row cursor for the outbox dispatcher. Atomically '
    'advanced with the outbox INSERTs in one transaction; '
    'restart picks up at the cursor without duplicates.';

-- +goose Down

DROP TRIGGER IF EXISTS federation_dispatch_notify_trg ON activities;
DROP FUNCTION IF EXISTS federation_dispatch_notify();
DROP TABLE federation_dispatch_state;
DROP TABLE federation_outbox;
