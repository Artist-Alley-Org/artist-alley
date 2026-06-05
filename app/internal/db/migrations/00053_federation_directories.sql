-- artist-alley migration 00053 — federation directory subscriptions.
-- Phase 1.22.B-c, feat/user-surfaces.
--
-- Two tables for the subscriber side of the directory protocol
-- per docs/spec/federation-directory/v1.md:
--
--   federation_directories         — directories we subscribe to
--   federation_directory_entries   — locally cached entries per directory
--
-- Cache-on-disk per the spec: a directory outage shouldn't make
-- discovered peers disappear. The cached entries persist; the
-- admin sees "directory not reachable since X" on the row.

-- +goose Up

CREATE TABLE federation_directories (
    id                     UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Directory operator's canonical URL. UNIQUE because one
    -- subscription per directory.
    directory_url          TEXT         NOT NULL UNIQUE,

    -- Operator name + Ed25519 pubkey fetched from
    -- GET /v1/operator at subscribe time. Used to verify every
    -- subsequent /v1/listing response signature. Key rotation
    -- requires re-subscribing.
    operator_name          TEXT         NOT NULL DEFAULT '',
    operator_public_key    TEXT         NOT NULL,
    operator_fingerprint   TEXT         NOT NULL,
    operator_contact       TEXT         NOT NULL DEFAULT '',

    -- Subscription bookkeeping.
    subscribed_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    subscribed_by_user_ref BIGINT       NOT NULL,
    enabled                BOOLEAN      NOT NULL DEFAULT TRUE,
    last_polled_at         TIMESTAMPTZ  NULL,
    last_poll_status       TEXT         NOT NULL DEFAULT 'never_polled'
                           CHECK (last_poll_status IN (
                               'never_polled', 'ok', 'unreachable',
                               'signature_failed', 'malformed', 'spec_version_mismatch'
                           )),
    last_poll_error        TEXT         NOT NULL DEFAULT '',

    -- Polling cadence. Spec default is 6 hours; admins can tighten
    -- per-directory if they want fresher data.
    poll_interval_seconds  INTEGER      NOT NULL DEFAULT 21600
                           CHECK (poll_interval_seconds >= 300),

    notes                  TEXT         NOT NULL DEFAULT '',

    created_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

COMMENT ON COLUMN federation_directories.operator_public_key IS
    'Pinned at subscribe time. Used to verify every /v1/listing '
    'signature. Key rotation requires the admin to defederate + '
    're-subscribe (no in-band key rotation in v1).';

COMMENT ON COLUMN federation_directories.last_poll_status IS
    'Catalogue mirrors federation/directory.PollStatus typed '
    'constants. The admin UI renders these as friendly strings.';

-- Hot read: the polling worker iterates enabled directories
-- whose last_polled_at is older than poll_interval_seconds.
CREATE INDEX federation_directories_due_idx
    ON federation_directories (last_polled_at NULLS FIRST)
    WHERE enabled = TRUE;

CREATE TABLE federation_directory_entries (
    id                   UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    -- The directory this entry came from. ON DELETE CASCADE so
    -- unsubscribing drops the cached entries; ON DELETE no other
    -- side cares because entries don't have outbound FKs.
    directory_id         UUID         NOT NULL REFERENCES federation_directories(id) ON DELETE CASCADE,

    -- Per-spec entry fields.
    instance_url         TEXT         NOT NULL,
    display_name         TEXT         NOT NULL,
    instance_public_key  TEXT         NOT NULL,
    fingerprint          TEXT         NOT NULL,
    region               TEXT         NOT NULL DEFAULT '',
    description          TEXT         NOT NULL DEFAULT '',
    tags                 JSONB        NOT NULL DEFAULT '[]'::jsonb,
    verified_at          TIMESTAMPTZ  NOT NULL,
    verified_via         TEXT         NOT NULL,
    listing_id           TEXT         NOT NULL DEFAULT '',

    -- Cached_at tracks WHEN this row was last fetched from the
    -- directory. Admin UI renders staleness based on this.
    cached_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    -- One row per (directory, instance_url) — re-polling
    -- replaces the row rather than appending.
    UNIQUE (directory_id, instance_url)
);

COMMENT ON TABLE federation_directory_entries IS
    'Locally cached snapshot of directory entries per ADR 0043 '
    '§"Cache last-known list locally". Survives directory outages.';

CREATE INDEX federation_directory_entries_by_dir_idx
    ON federation_directory_entries (directory_id, verified_at DESC);

CREATE INDEX federation_directory_entries_by_url_idx
    ON federation_directory_entries (instance_url);

-- +goose Down

DROP TABLE federation_directory_entries;
DROP TABLE federation_directories;
