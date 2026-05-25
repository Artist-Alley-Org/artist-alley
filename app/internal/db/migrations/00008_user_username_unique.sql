-- artist-alley migration 00008 — enforce UNIQUE on user.username.
--
-- Upstream RS never declared a UNIQUE on this column even though every
-- login path treats it as one. The omission lets duplicate usernames
-- creep in silently (an authentication-correctness bug) and made our
-- test fixtures' "ON CONFLICT (username)" upsert fall back to a
-- delete-then-insert with a noisy PG ERROR on every run.
--
-- We confirmed zero duplicates in the live table before writing this.
-- The index is non-partial because partial UNIQUE indexes can't serve
-- as the conflict target for ON CONFLICT (username) — PG requires the
-- predicate to match exactly and most callers don't carry one. Postgres
-- 15+ defaults UNIQUE indexes to NULLS DISTINCT, which lets RS keep
-- using NULL username for system/anonymous rows.

-- +goose Up

CREATE UNIQUE INDEX user_username_uniq_idx ON "user" (username);

COMMENT ON INDEX user_username_uniq_idx IS
    'Enforces username uniqueness. NULLs are distinct (PG15+ default), so RS''s NULL-username system rows still coexist.';

-- +goose Down

DROP INDEX IF EXISTS user_username_uniq_idx;
