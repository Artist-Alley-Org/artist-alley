-- artist-alley adjunct database initialization.
-- Runs once on first container start, before the app connects.

-- pgvector for embedding storage (multi-provider, multi-model).
CREATE EXTENSION IF NOT EXISTS vector;

-- pg_trgm for fuzzy text search and "search 4 years from now" fallback.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- uuid_generate_v4() / gen_random_uuid() for new-feature primary keys.
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS pgcrypto;
