-- artist-alley migration 00007 — federation_user_keys.
-- Phase 1.22.I-b. Storage for per-user X25519 keypairs used by
-- encrypted federation (NaCl-box envelope encryption).
--
-- Design references
--
--   * ADR 0049 §"Track B" — encrypted federation overall, key
--     lifecycle decisions, rotation policy.
--   * ADR 0017 — master-key-on-host pattern; the wrapped private
--     keys here follow the same wire format as the federation
--     instance identity (app/internal/atrest).
--
-- Architecture
--
--   - **Eager generation per user.** Every user gets a keypair
--     at create time (bootstrap, /setup, /admin/seed/users — all
--     three paths). Eager removes the "what does federation do
--     when the recipient hasn't generated their key yet?" path.
--   - **Multi-version retention.** A rotation (1.22.I-h) inserts
--     a new (user_id, version=N+1) row with is_current=true,
--     flips the previous row's is_current=false, and sets that
--     row's retained_until=NOW()+grace. The inbox decrypt path
--     (1.22.I-f) tries current first, falls back to retained
--     versions, succeeds when a peer is still emitting against
--     the previous key.
--   - **Encrypted at rest.** The private key is wrapped through
--     app/internal/atrest (AES-256-GCM, host master key). The
--     bytea column carries the versioned ciphertext directly —
--     no base64 indirection like federation_instance_identity
--     does in system_config (which is JSONB and needs a string
--     value). Bytea here saves the ~33% base64 inflation.
--   - **Public key is raw bytes.** X25519 public keys are
--     32 bytes flat (crypto/ecdh.PublicKey.Bytes() shape). No
--     PEM wrapping because there's no standard PEM container
--     for X25519 the way there is for Ed25519. The actor
--     profile inline `publicKeys` block (1.22.I-c) base64s the
--     bytes for JSON transport.
--   - **`algorithm` is explicit.** Locked at 'naclbox-x25519-v1'
--     today; the column exists so a future algorithm migration
--     (Hybrid PQ KEM, etc.) lands without a schema change.

-- +goose Up

-- --- federation_user_keys --------------------------------------------

CREATE TABLE federation_user_keys (
    -- Owner. CASCADE: deleting a user drops every version of
    -- their keys — no orphaned cipherblobs left behind.
    user_id            BIGINT       NOT NULL
        REFERENCES "user"(ref) ON DELETE CASCADE,

    -- Per-user monotonic version. The first key for a user is
    -- version=1; each rotation increments. Composite PK with
    -- user_id means SELECT...ORDER BY version DESC LIMIT 1
    -- returns the latest version without a partial scan.
    version            INTEGER      NOT NULL CHECK (version >= 1),

    -- Algorithm-versioned token. Today: 'naclbox-x25519-v1'.
    -- Future algorithms (PQ KEM, Curve448, etc.) get their own
    -- token without a schema migration; the read path dispatches
    -- on the value.
    algorithm          TEXT         NOT NULL DEFAULT 'naclbox-x25519-v1',

    -- Raw 32-byte X25519 public key (crypto/ecdh.PublicKey.Bytes()).
    -- No PEM container — X25519 doesn't have a canonical one and
    -- the actor profile (1.22.I-c) base64s the bytes inline.
    public_key         BYTEA        NOT NULL CHECK (octet_length(public_key) = 32),

    -- Private key, wrapped by app/internal/atrest. Wire format:
    -- version_byte(0x01) || 12-byte nonce || AES-GCM ciphertext+tag.
    -- See app/internal/atrest/atrest.go.
    private_key_enc    BYTEA        NOT NULL CHECK (octet_length(private_key_enc) >= 13),

    -- Exactly one row per user has is_current=true. The partial
    -- unique index below enforces it; the boolean here is the
    -- read-path selector (no NULL semantics — every row is
    -- either current or retained).
    is_current         BOOLEAN      NOT NULL,

    -- When the row was inserted. Used by the admin UI for the
    -- "when did this user last rotate?" panel landing in 1.22.I-h.
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    -- When this version stops being used for inbound decryption.
    -- NULL while is_current=true; non-NULL once a rotation has
    -- flipped this row aside. Periodic sweeper (1.22.I-h) drops
    -- rows past their retained_until.
    retained_until     TIMESTAMPTZ  NULL,

    PRIMARY KEY (user_id, version),

    -- Defence-in-depth: the partial unique index below already
    -- enforces exactly-one-current; this CHECK forbids the
    -- inconsistent combination of "current AND already
    -- retained" at row level, so a partial UPDATE that flips
    -- is_current without clearing retained_until trips the
    -- check immediately instead of silently violating the
    -- invariant.
    CONSTRAINT federation_user_keys_current_xor_retained CHECK (
        (is_current = TRUE  AND retained_until IS NULL)
     OR (is_current = FALSE AND retained_until IS NOT NULL)
    )
);

-- Exactly one current key per user. Partial unique index is the
-- right shape (vs UNIQUE column constraint) because retained
-- versions of the same user can coexist — we just need at most
-- one with is_current = TRUE.
CREATE UNIQUE INDEX federation_user_keys_one_current_idx
    ON federation_user_keys (user_id)
    WHERE is_current = TRUE;

-- Drives the sweeper that drops rows whose retention has expired
-- (1.22.I-h). Partial index keeps it tiny — only non-NULL
-- retained_until rows enter the index, so the size is bounded by
-- "users currently in their rotation grace window," not by total
-- user count.
CREATE INDEX federation_user_keys_retained_idx
    ON federation_user_keys (retained_until)
    WHERE retained_until IS NOT NULL;

COMMENT ON TABLE federation_user_keys IS
    'Per-user X25519 keypairs for NaCl-box encrypted federation. '
    'Phase 1.22.I-b. Private key column is atrest-wrapped (AES-GCM, '
    'host master key per app/internal/atrest). See ADR 0049 §Track B.';

COMMENT ON COLUMN federation_user_keys.algorithm IS
    'Algorithm-version token. Single value today: naclbox-x25519-v1. '
    'Future algorithms add new tokens without a schema migration.';

COMMENT ON COLUMN federation_user_keys.is_current IS
    'Exactly one row per user has is_current=true (enforced by '
    'partial unique index federation_user_keys_one_current_idx).';

COMMENT ON COLUMN federation_user_keys.retained_until IS
    'Inbound-decrypt window for a rotated-aside key. NULL on the '
    'current key; NOW()+grace_period when a rotation flips this '
    'row aside.';

-- +goose Down

DROP TABLE federation_user_keys;
