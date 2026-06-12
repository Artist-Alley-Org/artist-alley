-- artist-alley migration 00010 — federation_outbox.was_encrypted
-- for Phase 1.22.I-e (outbox encryption).
--
-- One observability column. The actual encryption state lives on
-- the wire envelope (federation/envelope.EncryptionBlock); the DB
-- column tells operators + scenario tests which outbox rows went
-- out encrypted vs plaintext without having to grep the audit log
-- or re-fetch + decode the dispatched envelope.
--
-- # Design references
--
--   * ADR 0049 §Track B — Decision 2 (NaCl-box per-recipient).
--   * Phase 1.22.I-d (commit bfedc36) shipped the capability gate
--     that decides whether to encrypt; this commit adds the
--     observability that records the gate's decision.
--   * Phase 1.22.I-f (next sub-phase) is the receiver-side
--     decryption; until it lands, CapNaClBox is REMOVED from
--     KnownCapabilities + the gate stays naturally false in
--     production. This column nonetheless exists from I-e so
--     scenario 09 (sender-side verification with capability
--     override) can SELECT was_encrypted to confirm the encrypt
--     path fired.
--
-- # Why a column + not just the audit event
--
-- The federation.emission.encrypted audit event (1.22.I-e-2) is
-- the primary observability surface; this column is the
-- query-without-joining-audit alternative for two narrow use cases:
--
--   (1) Scenario 09's assertion path — direct SELECT off the
--       row's UUID instead of a metadata->>'activity_uri' filter
--       on audit_events.
--   (2) Admin federation page's per-row indicator (planned for
--       I-h alongside rotation observability).
--
-- DEFAULT false so existing rows + the always-plaintext path
-- both get the right answer without backfill.

-- +goose Up

ALTER TABLE federation_outbox
    ADD COLUMN was_encrypted boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN federation_outbox.was_encrypted IS
    'Phase 1.22.I-e: TRUE when the dispatcher took the encryption '
    'branch for this row (peer.Capabilities.SupportsE2E + recipient '
    'key cached). FALSE for the legacy 1.22.D plaintext path. The '
    'wire envelope is the source of truth; this column is an '
    'observability mirror for scenario 09 + the admin federation '
    'surface.';

-- +goose Down

ALTER TABLE federation_outbox
    DROP COLUMN was_encrypted;
