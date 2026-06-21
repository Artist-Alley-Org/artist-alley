-- 00011_asset_embeddings.sql
--
-- Phase 1.14.B — pgvector-backed embedding storage for the AI bridge.
--
-- # Per-dim sibling tables (the multi-model story)
--
-- pgvector's HNSW index requires a fixed `vector(N)` column type, and
-- Postgres declarative LIST partitioning forbids varying column types
-- across partitions — so "one table, any model" can't be the answer.
-- Old-style table inheritance can work around the partition rule but
-- is deprecated and surprises the query planner.
--
-- The gold-standard approach Postgres + pgvector actually supports is
-- one table per vector dimension. Models that share a dim share a
-- table (nomic-embed-text and CLIP-ViT-L/14 both 768; mxbai-embed-
-- base also 768; mxbai-embed-large is 1024). Adding a new dim is one
-- migration: create `asset_embedding_dN`, mirror the indices, register
-- the dim in `app/internal/ai/embeddings/dim_registry.go`.
--
-- # Naming
--
-- `asset_embedding_d<N>` — `d` for "dimension", `<N>` the literal dim.
-- Consistent prefix lets the Go-side router enumerate tables with one
-- pattern (`asset_embedding_d%`); naming after the dim (not the model)
-- means two models of the same dim don't need separate tables.
--
-- # Composite PK
--
-- (asset_id, provider, model, modality) is the idempotency key — re-
-- running embedding for the same key replaces the vector via ON
-- CONFLICT DO UPDATE. Same key across different dim-tables can't
-- collide because each model's dim is fixed.
--
-- # Index
--
-- HNSW (hierarchical navigable small world) over `vector_cosine_ops`.
-- m=16, ef_construction=64 are pgvector defaults — fine up to ~1M
-- vectors per dim-table on a single host. Operators with larger
-- catalogues can REINDEX with tuned params per table.

-- +goose Up
-- +goose StatementBegin

-- ---------------------------------------------------------------------------
-- Common-shape pieces shared across all dim tables.
-- ---------------------------------------------------------------------------

-- Same modality CHECK constraint every dim table reuses. Declared
-- here as a domain so a future check-tweak is one place to edit.
CREATE DOMAIN ai_embedding_modality AS TEXT
    CHECK (VALUE IN ('text', 'image', 'multimodal'));

-- ---------------------------------------------------------------------------
-- 768-dim table — covers nomic-embed-text, CLIP-ViT-L/14, bge-m3,
-- mxbai-embed-base.
-- ---------------------------------------------------------------------------

CREATE TABLE asset_embedding_d768 (
    asset_id     UUID                     NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    provider     TEXT                     NOT NULL,
    model        TEXT                     NOT NULL,
    modality     ai_embedding_modality    NOT NULL,
    embedding    vector(768)              NOT NULL,
    content_hash TEXT,
    created_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (asset_id, provider, model, modality)
);

CREATE INDEX idx_asset_embedding_d768_hnsw_cosine
    ON asset_embedding_d768
    USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);

CREATE INDEX idx_asset_embedding_d768_asset
    ON asset_embedding_d768 (asset_id);

-- ---------------------------------------------------------------------------
-- Config seeds
-- ---------------------------------------------------------------------------
--
--   ai.embedding.default_model — model the embed job hands to the
--     router when the operator hasn't picked one. nomic-embed-text
--     is 768, Ollama-shipped, CPU-friendly.
--
--   ai.embedding.dim_registry — single source of truth mapping
--     model name → dim. The Go-side writer reads this to pick which
--     dim-table to upsert into. Operators add a model by extending
--     this map; adding a model with a new dim requires a follow-up
--     migration creating `asset_embedding_dN` plus extending the
--     registry. Keeping the registry in system_config (not hard-
--     coded in Go) lets operators register new same-dim models
--     without a code change.
--
--   ai.search.similar_default_limit — neighbours returned by
--     /search/similar when limit is omitted. 12 fits the 4-col grid.
INSERT INTO system_config (key, value) VALUES
    ('ai.embedding.default_model',      '"nomic-embed-text"'::jsonb),
    ('ai.embedding.dim_registry', '{"nomic-embed-text":768,"clip-vit-l-14":768,"bge-m3":768,"mxbai-embed-base":768}'::jsonb),
    ('ai.search.similar_default_limit', '12'::jsonb)
ON CONFLICT (key) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM system_config WHERE key IN (
    'ai.embedding.default_model',
    'ai.embedding.dim_registry',
    'ai.search.similar_default_limit'
);
DROP INDEX IF EXISTS idx_asset_embedding_d768_asset;
DROP INDEX IF EXISTS idx_asset_embedding_d768_hnsw_cosine;
DROP TABLE IF EXISTS asset_embedding_d768;
DROP DOMAIN IF EXISTS ai_embedding_modality;
-- +goose StatementEnd
