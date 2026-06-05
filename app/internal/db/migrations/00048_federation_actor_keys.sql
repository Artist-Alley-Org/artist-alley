-- artist-alley migration 00048 — federation actor keys.
-- Phase 1.22.A, feat/user-surfaces.
--
-- Adds per-user identity primitives for the walled-garden
-- federation protocol (ADR 0043). Every Artist Alley user
-- federated as an actor needs:
--
--   - actor_uri          The stable cross-instance handle
--                        (https://{instance}/users/{username}).
--                        Computed from site base URL + username
--                        at user creation. Backfilled below for
--                        existing rows.
--   - signing_public_key_pem
--                        Ed25519 public key (PEM-wrapped per
--                        RFC 8410) published at
--                        {actor_uri}#main-key. Authenticates the
--                        actor on every signed envelope.
--   - signing_private_key_enc
--                        AES-256-GCM-encrypted PKCS#8 PEM of the
--                        Ed25519 private key. Master key sourced
--                        from AA_MASTER_KEY at process boot
--                        (app/internal/atrest). Plain bytes never
--                        touch the database.
--   - encryption_public_key
--                        X25519 (Curve25519) public key, 32 raw
--                        bytes. Distinct from the signing key per
--                        spec §6.2 (key-separation hygiene; the
--                        Edwards-to-Montgomery birational map is
--                        rejected for v1). Used by NaCl-box
--                        multi-recipient encryption envelopes.
--   - encryption_private_key_enc
--                        AES-256-GCM-encrypted 32-byte X25519
--                        private scalar.
--
-- ALL columns are nullable. Existing users get NULL key columns +
-- backfilled actor_uri; key material is generated lazily on first
-- federation event involving them. Reason: generating + encrypting
-- keypairs for every existing user at migration time would require
-- the master key be available at migration time, which conflicts
-- with most operators' boot order (migrations run before secrets
-- are mounted). Lazy generation lets the user-facing federation
-- code call atrest.Initialised() and either generate or 503 as
-- appropriate.
--
-- Key rotation is deferred to Phase 1.22.K per spec §14. v1 is
-- one keypair per actor for life. If a key is compromised before
-- 1.22.K ships, the operational answer is delete-and-recreate
-- the user account; all federated references to the old actor_uri
-- become 404 and remote peers will need to re-pair.

-- +goose Up

ALTER TABLE "user" ADD COLUMN actor_uri                  TEXT  NULL;
ALTER TABLE "user" ADD COLUMN signing_public_key_pem     TEXT  NULL;
ALTER TABLE "user" ADD COLUMN signing_private_key_enc    BYTEA NULL;
ALTER TABLE "user" ADD COLUMN encryption_public_key      BYTEA NULL;
ALTER TABLE "user" ADD COLUMN encryption_private_key_enc BYTEA NULL;

-- Unique constraint on actor_uri at the storage layer. Defence
-- in depth: the username uniqueness already guarantees this at
-- the application layer, but a manual UPDATE that breaks the
-- invariant would silently break federation. The constraint
-- catches it.
CREATE UNIQUE INDEX user_actor_uri_idx ON "user" (actor_uri)
    WHERE actor_uri IS NOT NULL;

COMMENT ON COLUMN "user".actor_uri IS
    'Stable cross-instance handle for this user in federation. '
    'Immutable from the federation perspective per spec §8.4 — '
    'changing it breaks every federated reference and is an '
    'admin-gated operation.';

COMMENT ON COLUMN "user".signing_private_key_enc IS
    'Ed25519 private key encrypted at rest with the host master '
    'key (app/internal/atrest, AA_MASTER_KEY env var). Plain '
    'bytes are never stored or logged. Decryption happens only '
    'inside users.GetActorSigningPrivateKey for the duration of '
    'a sign operation.';

COMMENT ON COLUMN "user".encryption_private_key_enc IS
    'X25519 private scalar (32 bytes) encrypted at rest with the '
    'host master key. Same handling as signing_private_key_enc.';

-- +goose Down

DROP INDEX IF EXISTS user_actor_uri_idx;
ALTER TABLE "user" DROP COLUMN IF EXISTS encryption_private_key_enc;
ALTER TABLE "user" DROP COLUMN IF EXISTS encryption_public_key;
ALTER TABLE "user" DROP COLUMN IF EXISTS signing_private_key_enc;
ALTER TABLE "user" DROP COLUMN IF EXISTS signing_public_key_pem;
ALTER TABLE "user" DROP COLUMN IF EXISTS actor_uri;
