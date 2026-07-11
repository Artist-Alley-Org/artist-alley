-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- artist-alley adjunct database initialization.
-- Runs once on first container start, before the app connects.

-- pgvector for embedding storage (multi-provider, multi-model).
CREATE EXTENSION IF NOT EXISTS vector;

-- pg_trgm for fuzzy text search and "search 4 years from now" fallback.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- uuid_generate_v4() / gen_random_uuid() for new-feature primary keys.
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Pre-create goose's bookkeeping table so the migration runner's first
-- SELECT against it doesn't log a noisy "relation does not exist" ERROR
-- on every fresh-install boot. Schema matches goose v3 (pressly/goose);
-- update if we bump goose major versions.
CREATE TABLE IF NOT EXISTS goose_db_version (
    id          serial      NOT NULL PRIMARY KEY,
    version_id  bigint      NOT NULL,
    is_applied  boolean     NOT NULL,
    tstamp      timestamp   NULL DEFAULT NOW()
);
INSERT INTO goose_db_version (version_id, is_applied)
SELECT 0, true
WHERE NOT EXISTS (SELECT 1 FROM goose_db_version);
