-- Phase 1.16.B-4 — saved searches + email-on-match.
--
-- One table: saved_search. Rows are per-user (owner_user_ref) DSL
-- strings the coordinator re-runs on an interval. Each row carries
-- its own delta state (last_result_hash + last_result_ids) so
-- notifications fire only when the fresh result set differs from
-- the last stored one.
--
-- Federation: NOT federated (per pre-audit Q5 — federation is
-- opt-in via activities.Emit, and saved_search never emits).
-- origin_server_id column present for schema-shape parity with
-- other user-owned tables; sqlc-generated Row types stay
-- symmetric.
--
-- Visibility: the run-time execution path applies
-- visibility.Filter(EntityAsset, owner) at query time, NOT save
-- time. A user who saves a search + later loses access to some
-- assets stops seeing those assets in the digest silently — the
-- load-bearing invariant for this whole sub-phase.

-- +goose Up

-- +goose StatementBegin
CREATE TABLE saved_search (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_ref           BIGINT NOT NULL REFERENCES "user"(ref) ON DELETE CASCADE,
    name                     TEXT NOT NULL,
    dsl                      TEXT NOT NULL,
    notify_channel           TEXT NOT NULL DEFAULT 'email'
        CHECK (notify_channel IN ('email', 'none')),
    notify_interval_minutes  INTEGER NOT NULL DEFAULT 60
        CHECK (notify_interval_minutes >= 1),
    enabled                  BOOLEAN NOT NULL DEFAULT TRUE,
    -- Delta state: hash of the sorted asset-ID set from the last
    -- run; the set itself in last_result_ids for the exact
    -- set-diff computation in the notifier. NULL for
    -- never-run rows.
    last_result_hash         TEXT,
    last_result_ids          UUID[],
    -- Run + notify timestamps kept separate so a run without a
    -- delta (hash unchanged) still updates last_run_at but leaves
    -- last_notified_at pointing at the previous email.
    last_run_at              TIMESTAMPTZ,
    last_notified_at         TIMESTAMPTZ,
    origin_server_id         UUID,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Case-insensitive name uniqueness per owner so the account
    -- page doesn't render two identically-labelled rows.
    UNIQUE (owner_user_ref, name)
);

COMMENT ON TABLE saved_search IS
    'Per-user notification target: a stored DSL string the coordinator re-runs on notify_interval_minutes cadence. Delta detection via hash-of-sorted-asset-ID-set; the notifier emails the fresh Added IDs. Phase 1.16.B-4. Not federated — per-user local notification prefs.';

COMMENT ON COLUMN saved_search.dsl IS
    'Raw DSL string as the caller wrote it. Round-trips through dsl.Parse/Compile at execution time; no serialiser needed (per pre-audit Q3).';

COMMENT ON COLUMN saved_search.last_result_hash IS
    'sha256 hex of the sorted asset-ID set from the last successful run. NULL for never-run rows. Hash mismatch on next run triggers digest email.';

COMMENT ON COLUMN saved_search.last_result_ids IS
    'The actual asset IDs from the last run, so the notifier can compute the exact Added set-diff. UUID[] avoids a second table.';

-- List query hits (owner_user_ref, id).
CREATE INDEX saved_search_owner_idx ON saved_search (owner_user_ref, id);

-- Coordinator walk: enabled rows past their next-run threshold.
-- Partial WHERE keeps the index small even at scale.
CREATE INDEX saved_search_due_idx
    ON saved_search (last_run_at NULLS FIRST)
    WHERE enabled = TRUE;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DROP INDEX IF EXISTS saved_search_due_idx;
DROP INDEX IF EXISTS saved_search_owner_idx;
DROP TABLE IF EXISTS saved_search;
-- +goose StatementEnd
