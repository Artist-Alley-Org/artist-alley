-- artist-alley migration 00011 — federation_inbox observability
-- for Phase 1.22.I-f (inbox decryption).
--
-- Two columns; mirror shape of 00010's federation_outbox.was_encrypted
-- so SELECTs across the two tables answer "per-row encryption state"
-- with the same column name.
--
-- # Why was_encrypted is bool, not derived from envelope_json
--
-- The envelope's "encryption" field is the source of truth for
-- whether the row arrived encrypted, but joining audit + analytics
-- via `envelope_json -> 'encryption' IS NOT NULL` is verbose and
-- forces the planner through the jsonb path every time. A bool
-- column is a one-byte index + a one-condition filter; the
-- dispatcher writes it during stage 4 alongside the existing
-- MarkInboxProcessed call.
--
-- # Why decrypted_with_key_version is int4 + nullable
--
--   - NULL when was_encrypted=false (plaintext envelope, no key
--     used). Distinct from "tried but failed" — that path rejects
--     the row entirely + records `reject_reason = decrypt_failed`,
--     not a marked-processed state.
--   - The version of the receiver's key that actually decrypted.
--     1 = current key worked; 2+ = retained-key fallback fired
--     (the rotation grace window from 1.22.I-h kicked in).
--   - Operator analytics: SELECT decrypted_with_key_version,
--     count(*) FROM federation_inbox WHERE was_encrypted = TRUE
--     GROUP BY 1 answers "are most encrypted envelopes still
--     hitting v1 of my key?" — the rotation health signal that
--     I-h's admin UI surfaces in a chart.
--
-- # Design references
--
--   * ADR 0049 §Track B — Decision 4 ("Multi-version retention").
--   * Phase 1.22.I-e shipped the sender-side encrypt + the
--     equivalent federation_outbox.was_encrypted column; this is
--     the receiver-side mirror.

-- +goose Up

ALTER TABLE federation_inbox
    ADD COLUMN was_encrypted               boolean  NOT NULL DEFAULT false,
    ADD COLUMN decrypted_with_key_version  integer  NULL;

COMMENT ON COLUMN federation_inbox.was_encrypted IS
    'Phase 1.22.I-f: TRUE when the dispatcher took the decrypt '
    'branch for this row (envelope had a non-empty encryption '
    'block). FALSE for the legacy 1.22.D plaintext path. Mirrors '
    'federation_outbox.was_encrypted on the sender side.';

COMMENT ON COLUMN federation_inbox.decrypted_with_key_version IS
    'Phase 1.22.I-f: which version of the receiver''s X25519 key '
    'successfully decrypted the envelope. NULL when '
    'was_encrypted=false. Surfaces rotation health to operator '
    'analytics via the I-h admin federation page.';

-- +goose Down

ALTER TABLE federation_inbox
    DROP COLUMN decrypted_with_key_version,
    DROP COLUMN was_encrypted;
