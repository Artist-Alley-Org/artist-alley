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
-- inside its retention window. The I-h rotation metadata columns
-- ride along — overhead is ~16 bytes per row + the admin
-- /admin/federation/key-health surface uses the same query to
-- show "who rotated this key + when?" without a second round
-- trip.
SELECT user_id, version, algorithm, public_key, private_key_enc,
       is_current, created_at, retained_until,
       rotated_at, rotated_by_user_ref
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

-- name: ListUserKeysForDecrypt :many
-- Phase 1.22.I-f — returns the current + retained-not-expired keys
-- for a user, full row (public + WRAPPED PRIVATE bytes + version)
-- so the inbox decrypt path can walk them in order until one
-- successfully opens the NaCl-box ciphertext. Distinct from
-- ListPublicKeysByUser (no private bytes there) so the public
-- read-path can't accidentally leak the private column even
-- with code drift.
--
-- Ordering:
--   1. is_current DESC — the active key always tried first;
--      handles the common case (no rotation drift) at attempt #1.
--   2. version DESC — among retained keys, walk newest-to-oldest
--      so a sender that's one version behind hits a quick
--      success; very-stale senders pay the linear walk cost.
--
-- The retained_until filter excludes keys past their grace window
-- — those rows might still be in the table during the I-h sweeper
-- delay, but the dispatcher must NOT decrypt against them.
SELECT user_id, version, algorithm, public_key, private_key_enc,
       is_current, created_at, retained_until
FROM federation_user_keys
WHERE user_id = $1
  AND (is_current = TRUE OR retained_until > NOW())
ORDER BY is_current DESC, version DESC;

-- name: InsertUserKeyAsCurrent :one
-- Phase 1.22.I-h rotation primitive. Inserts a new key with
-- is_current=TRUE, retained_until=NULL, AND populates the
-- rotation metadata columns (rotated_at = NOW(), rotated_by_user_ref
-- = caller). Distinct from [InsertUserKey] (the bootstrap path)
-- which leaves rotated_at NULL — non-NULL rotated_at is the
-- forensic signal that a rotation primitive minted this row.
--
-- Callers MUST run [DemoteCurrentKey] for the same user in the
-- SAME transaction before this insert; the partial unique index
-- federation_user_keys_one_current_idx would otherwise fail the
-- second is_current=TRUE row.
INSERT INTO federation_user_keys (
    user_id,
    version,
    algorithm,
    public_key,
    private_key_enc,
    is_current,
    retained_until,
    rotated_at,
    rotated_by_user_ref
)
VALUES ($1, $2, $3, $4, $5, TRUE, NULL, NOW(), $6)
RETURNING user_id, version, algorithm, public_key, private_key_enc,
          is_current, created_at, retained_until,
          rotated_at, rotated_by_user_ref;

-- name: DemoteCurrentKey :exec
-- Phase 1.22.I-h rotation primitive. Flips the user's current key
-- to is_current=FALSE, sets retained_until=NOW()+($2 days), AND
-- records the rotation metadata on the demoted row (rotated_at +
-- rotated_by_user_ref). The CHECK constraint
-- federation_user_keys_current_xor_retained requires both column
-- flips atomically — this single UPDATE satisfies it.
--
-- Idempotency: WHERE is_current = TRUE means a repeated call (no
-- current key found) is a no-op; the rotation orchestration relies
-- on this when the user has no prior key (defensive — shouldn't
-- happen post-I-b but the path is exercised).
--
-- The retention interval is constructed from the $2 integer days
-- value (not interpolated SQL) so an operator-supplied retention
-- override goes through pgx parameter binding, not string concat.
UPDATE federation_user_keys
   SET is_current          = FALSE,
       retained_until      = NOW() + (sqlc.arg('retention_days')::int || ' days')::interval,
       rotated_at          = NOW(),
       rotated_by_user_ref = sqlc.arg('rotated_by_user_ref')
 WHERE user_id = sqlc.arg('user_id')
   AND is_current = TRUE;

-- name: SweepExpiredRetainedKeys :execrows
-- Phase 1.22.I-h sweeper. Hard-deletes every retained-but-expired
-- row (is_current=FALSE AND retained_until < NOW()). Returns the
-- count of reaped rows for the audit emit + the operator log line.
--
-- The partial index federation_user_keys_retained_idx on
-- retained_until WHERE retained_until IS NOT NULL keeps this sweep
-- O(k log n) where k is the expired-row count — usually zero on a
-- healthy tick, occasionally tens after a busy day.
--
-- No batching: the per-tick reap count is small (one user can
-- accumulate at most 1 retained key per rotation; the 30-day TTL
-- means the typical instance has 0-1 retained keys per user at
-- any moment); a single DELETE finishes in milliseconds.
DELETE FROM federation_user_keys
 WHERE is_current = FALSE
   AND retained_until IS NOT NULL
   AND retained_until < NOW();

-- name: GetKeyHealthSummary :one
-- Phase 1.22.I-h admin observability surface. One aggregate query
-- feeding /admin/federation/key-health's top-of-page tiles. Five
-- counts that surface the gaps dogfood exposed on the encryption
-- arc: users without keypairs (I-b backfill miss), remote actors
-- missing encryption keys (I-c cache miss), peers without
-- capabilities (I-d negotiation miss), retained keys near expiry
-- (sweeper preview), and the total approved-user count for
-- denominator context.
--
-- All five counts in one round-trip — the dashboard renders the
-- whole top panel without N HTTP calls. Per-row drill-downs use
-- the separate List* queries below.
SELECT
    (SELECT COUNT(*) FROM "user" WHERE approved = 1)
        AS users_total,
    (SELECT COUNT(*) FROM "user" u
       WHERE u.approved = 1
         AND NOT EXISTS (
             SELECT 1 FROM federation_user_keys k
             WHERE k.user_id = u.ref AND k.is_current = TRUE
         ))
        AS users_missing_keypair,
    (SELECT COUNT(*) FROM federation_remote_actors
        WHERE encryption_public_key IS NULL)
        AS remote_actors_missing_enc_key,
    (SELECT COUNT(*) FROM federation_peers
        WHERE capabilities_negotiated_at IS NULL)
        AS peers_missing_capabilities,
    (SELECT COUNT(*) FROM federation_user_keys
        WHERE is_current = FALSE
          AND retained_until IS NOT NULL
          AND retained_until > NOW()
          AND retained_until < NOW() + INTERVAL '7 days')
        AS retained_keys_near_expiry;

-- name: ListUsersMissingKeypair :many
-- Drill-down for the /admin/federation/key-health "users missing
-- keypair" tile. Returns enough per-row context for the operator
-- to either trigger a per-user backfill OR escalate (the user is
-- pending approval and shouldn't have a keypair yet).
--
-- LIMIT 100 caps the response — an instance with thousands of
-- pre-I-b users-without-keys would otherwise hit a multi-MB JSON
-- response; this query exists to surface the LAST FEW, not
-- replace the boot sweeper.
SELECT u.ref, u.username, u.created
  FROM "user" u
 WHERE u.approved = 1
   AND NOT EXISTS (
       SELECT 1 FROM federation_user_keys k
       WHERE k.user_id = u.ref AND k.is_current = TRUE
   )
 ORDER BY u.created DESC
 LIMIT 100;

-- name: ListRecentRotations :many
-- Drill-down for the /admin/federation/key-health "recent
-- rotations" tile. Audit-style list of the 50 most recent rotation
-- events. Pulls from federation_user_keys directly (not
-- audit_events) so the page renders without a JOIN against an
-- event-log table whose growth profile differs.
--
-- LIMIT 50 + DESC ordering on rotated_at: shows newest first;
-- pagination is a follow-up if rotation volume grows.
SELECT user_id, version, rotated_at, rotated_by_user_ref
  FROM federation_user_keys
 WHERE rotated_at IS NOT NULL
 ORDER BY rotated_at DESC
 LIMIT 50;

-- name: ListUsersWithoutCurrentKey :many
-- Phase 1.22.I-b boot-time backfill safety net. Returns refs of
-- every "user" row that has no federation_user_keys row with
-- is_current=TRUE. Caller iterates the results and invokes
-- [EnsureCurrentForUser] on each ref to mint the missing key.
--
-- Scope:
--   - approved=1 only — disabled/pending users (approved=0 or 2)
--     don't federate, so they don't need a keypair until + unless
--     they're re-approved.
--   - Anti-join via WHERE NOT EXISTS is faster than LEFT JOIN +
--     IS NULL on this table shape (small index on is_current=TRUE
--     keeps the EXISTS lookup O(log n)).
--   - LIMIT $1 caps the per-tick work so an instance with
--     100k pre-I-b users doesn't block boot for minutes; the
--     boot sweeper loops until LIMIT returns empty.
--
-- Order: by ref ASC so a partial sweep is deterministic + the next
-- tick continues from a known point. Boot doesn't care about
-- ordering on the happy path (zero rows) but a debugger watching
-- a real backfill in progress sees predictable progress.
SELECT u.ref
FROM "user" u
WHERE u.approved = 1
  AND NOT EXISTS (
      SELECT 1 FROM federation_user_keys fuk
      WHERE fuk.user_id = u.ref AND fuk.is_current = TRUE
  )
ORDER BY u.ref
LIMIT $1;
