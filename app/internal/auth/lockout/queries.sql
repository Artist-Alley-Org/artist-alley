-- name: GetLockoutState :one
-- Returns the failed-login counter + lockout deadline for one user.
-- Called on the hot login path (after LoginLimiter, before credential
-- verify) to decide whether to short-circuit into the anti-enumeration
-- 401 shape without ever consulting bcrypt.
SELECT ref, failed_login_count, lockout_until
FROM "user"
WHERE ref = $1
LIMIT 1;

-- name: IncrementFailedLogin :one
-- Atomic increment with lockout side-effect. Postgres row-level lock
-- serialises concurrent updates so the CASE expression sees a
-- consistent failed_login_count value + writes the lockout deadline
-- exactly when the threshold is crossed. Threshold + duration come
-- from sysconfig (auth.lockout_threshold + auth.lockout_duration_minutes).
--
-- Returns the new counter + deadline so the caller can decide whether
-- to emit an audit event (only when this call is the one that
-- crossed the threshold — attackers pounding a locked account should
-- get exactly one audit row, not one per attempt).
UPDATE "user"
SET
    failed_login_count = failed_login_count + 1,
    lockout_until = CASE
        WHEN failed_login_count + 1 >= sqlc.arg('threshold')::INTEGER
             AND (lockout_until IS NULL OR lockout_until < NOW())
        THEN NOW() + (sqlc.arg('duration_minutes')::INTEGER || ' minutes')::INTERVAL
        ELSE lockout_until
    END
WHERE ref = sqlc.arg('user_ref')::BIGINT
RETURNING failed_login_count, lockout_until;

-- name: ResetFailedLogin :exec
-- Clears the failed counter + any lingering lockout_until. Called from
-- inside the successful-auth transaction (same tx as session issue) so
-- rollback on session-create failure rolls the reset back too.
UPDATE "user"
SET
    failed_login_count = 0,
    lockout_until = NULL
WHERE ref = $1
  AND (failed_login_count > 0 OR lockout_until IS NOT NULL);

-- name: AdminUnlock :one
-- Admin-driven unlock: clears counter + deadline. Returns the previous
-- failed_login_count so the audit event can record it. Returns zero
-- rows when the caller hasn't crossed the threshold (idempotent no-op;
-- caller sees zero rows + can skip the audit emit if desired).
UPDATE "user"
SET
    failed_login_count = 0,
    lockout_until = NULL
WHERE ref = $1
  AND (failed_login_count > 0 OR lockout_until IS NOT NULL)
RETURNING failed_login_count AS prior_failed_count;

-- name: CountActiveLockouts :one
-- Health-gauge query. Uses the partial index idx_users_lockout_active
-- so this is cheap even under many-thousand-user installs. The stale-
-- lockout rows (lockout_until in the past) are filtered client-side
-- via the > NOW() predicate; DB doesn't sweep them.
SELECT COUNT(*)::BIGINT AS active_count
FROM "user"
WHERE lockout_until IS NOT NULL
  AND lockout_until > NOW();
