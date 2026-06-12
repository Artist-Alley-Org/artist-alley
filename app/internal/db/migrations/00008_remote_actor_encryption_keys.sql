-- artist-alley migration 00008 — federation_remote_actors gains
-- the inbound encryption-key cache columns.
-- Phase 1.22.I-c. Storage for the X25519 public key a remote
-- peer publishes for one of their users, harvested from the
-- per-envelope `aa:encryptionPublicKey` block at inbox time.
--
-- Design references
--
--   * ADR 0049 §Track B — encrypted federation key distribution.
--   * Phase 1.22.I-b — federation_user_keys (LOCAL users) is the
--     paired surface; this commit covers the REMOTE side.
--
-- Architecture
--
--   - **Single key per remote actor in I-c.** The columns hold
--     the current advertised key. Rotation handling for remote
--     actors lands with I-h; until then a fresh inbound key
--     overwrites the previous one + bumps the version. The
--     audit event records the rotation event for operator
--     traceability.
--   - **Nullable.** Pre-I-c peers won't include the encryption
--     block in their envelopes. NULL columns mean "we don't have
--     a key for this actor"; the sender refusal flow (I-g)
--     surfaces this to the operator via the existing restricted-
--     share banner. Forward-only — no migration backfill.
--   - **bytea, not text.** Same convention as
--     federation_user_keys.public_key: 32 raw bytes are the
--     storage form; base64 happens at the wire boundary
--     (envelope JSON).
--   - **Version is just an int.** A single monotonic counter the
--     sender controls. I-h adds the multi-version retention
--     semantics for LOCAL users (federation_user_keys); remote
--     actors keep just the current advertised version because
--     we never have to decrypt with their key — we only encrypt
--     to it.

-- +goose Up

ALTER TABLE federation_remote_actors
    ADD COLUMN encryption_public_key            BYTEA       NULL,
    ADD COLUMN encryption_public_key_version    INTEGER     NULL,
    ADD COLUMN encryption_public_key_updated_at TIMESTAMPTZ NULL,

    -- All three columns either populated together or all NULL.
    -- Prevents partial states like "version set but key NULL"
    -- that would confuse the read path's "has a key?" check.
    -- Same defence-in-depth posture as federation_user_keys'
    -- current_xor_retained CHECK.
    ADD CONSTRAINT federation_remote_actors_encryption_key_atomic CHECK (
        (encryption_public_key IS NULL
            AND encryption_public_key_version IS NULL
            AND encryption_public_key_updated_at IS NULL)
     OR (encryption_public_key IS NOT NULL
            AND octet_length(encryption_public_key) = 32
            AND encryption_public_key_version IS NOT NULL
            AND encryption_public_key_version >= 1
            AND encryption_public_key_updated_at IS NOT NULL)
    );

-- Operator-facing observability — the admin federation surface
-- can answer "how many of my known remote actors are still on a
-- pre-I-c build?" via SELECT count(*) WHERE ... IS NULL. Partial
-- index keeps it cheap on instances where most actors do have
-- keys (the steady state); only the missing-key rows enter it.
CREATE INDEX federation_remote_actors_missing_encryption_key_idx
    ON federation_remote_actors (peer_id)
    WHERE encryption_public_key IS NULL;

COMMENT ON COLUMN federation_remote_actors.encryption_public_key IS
    'X25519 public key advertised by the remote actor in their '
    'envelope''s aa:encryptionPublicKey block. NULL when the peer '
    'is on a pre-1.22.I-c build. 32 bytes when populated.';

COMMENT ON COLUMN federation_remote_actors.encryption_public_key_version IS
    'Per-actor version number, monotonic. Bumped by the remote '
    'side on key rotation (1.22.I-h on their end); we just observe '
    'the value + persist alongside the key bytes.';

COMMENT ON COLUMN federation_remote_actors.encryption_public_key_updated_at IS
    'When we last observed a key for this actor. Updated on every '
    'inbound envelope carrying the block (even when the value did '
    'not change) so an operator can see how stale our knowledge is.';

-- +goose Down

DROP INDEX federation_remote_actors_missing_encryption_key_idx;
ALTER TABLE federation_remote_actors
    DROP CONSTRAINT federation_remote_actors_encryption_key_atomic,
    DROP COLUMN encryption_public_key_updated_at,
    DROP COLUMN encryption_public_key_version,
    DROP COLUMN encryption_public_key;
