-- app/internal/federation/userkeys/queries.sql
--
-- Per-user X25519 keypair storage for Phase 1.22.I encrypted
-- federation. Schema lives in migration 00007. Crypto primitives
-- + at-rest wrap/unwrap live in keygen.go in this package.
--
-- Scope for I-b (this package): the storage layer + crypto
-- primitive. No HTTP surface, no federation wire integration.
-- Consumers wire in commit 3 (bootstrap, /setup, /admin/seed/users).
--
-- Future phase coverage of these same primitives:
--
--   I-c (actor profile inline `publicKeys`) — ListPublicKeysByUser
--       returns current + retained-for-decrypt public keys.
--   I-e (outbox encryption)                 — GetCurrentByUser
--       supplies the recipient's encryption target.
--   I-f (inbox decryption)                  — GetByVersion drives
--       the fallback path when an envelope cites a previous
--       version still inside its retention window.
--   I-h (rotation)                          — adds Begin/Insert
--       rotation queries; not landed in this commit so the rotation
--       feature work owns its own atomic shape.

-- name: InsertUserKey :one
-- Inserts a new key row. Caller picks `version` (1 for first key,
-- N+1 for rotation), `is_current` (TRUE for new current, FALSE for
-- a row being rotated aside), and `retained_until` (NULL for the
-- current key; non-NULL only when inserting a row already in the
-- retention window — usually nothing to do at insert time, the
-- rotation path UPDATEs the previously-current row instead).
INSERT INTO federation_user_keys (
    user_id,
    version,
    algorithm,
    public_key,
    private_key_enc,
    is_current,
    retained_until
)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING user_id, version, algorithm, public_key, private_key_enc,
          is_current, created_at, retained_until;

-- name: GetCurrentUserKey :one
-- Returns the user's current key. Used by the EnsureCurrentForUser
-- idempotency check + I-e outbox encryption + I-c actor profile.
-- Returns pgx.ErrNoRows if the user has no keys yet.
SELECT user_id, version, algorithm, public_key, private_key_enc,
       is_current, created_at, retained_until
FROM federation_user_keys
WHERE user_id = $1 AND is_current = TRUE;

-- name: GetUserKeyByVersion :one
-- Returns a specific (user, version) row. Used by I-f inbound
-- decryption when an envelope cites a non-current version still
-- inside its retention window.
SELECT user_id, version, algorithm, public_key, private_key_enc,
       is_current, created_at, retained_until
FROM federation_user_keys
WHERE user_id = $1 AND version = $2;

-- name: ListPublicKeysByUser :many
-- Returns the user's current key + any retained-for-decrypt keys,
-- ordered version DESC so the actor profile (I-c) ships the current
-- key first. The private_key_enc column is intentionally omitted —
-- public lookups don't need it and shouldn't carry the ciphertext
-- across the read boundary.
SELECT user_id, version, algorithm, public_key,
       is_current, created_at, retained_until
FROM federation_user_keys
WHERE user_id = $1
  AND (is_current = TRUE OR retained_until > NOW())
ORDER BY version DESC;

-- name: CountUserKeys :one
-- Total key versions for a user (current + retained, regardless of
-- expiry). Test helper + future admin UI; not on any hot path.
SELECT COUNT(*) FROM federation_user_keys WHERE user_id = $1;
