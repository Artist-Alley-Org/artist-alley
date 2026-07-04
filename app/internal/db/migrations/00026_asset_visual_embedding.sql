-- Phase 1.16.B-3-followup — CLIP visual encoder + reverse image search.
-- Closes #183.
--
-- Adds a NEW parallel embedding table for CLIP-visual embeddings.
-- Deliberately separate from asset_embedding_d768 (which holds
-- text-derived embeddings via Ollama nomic-embed-text) — the two
-- embedding SPACES are incompatible (cosine similarity between text
-- and visual embeddings is meaningless), so keeping them in
-- physically separate tables makes accidental cross-space queries
-- impossible.
--
-- Design decisions locked in the phase brief:
--   1. Two embedding spaces, two tables, zero cross-comparison.
--   2. HNSW cosine index — matches asset_embedding_d768's index shape
--      (m=16, ef_construction=64). Consistency > theoretical
--      superiority per the phase brief.
--   3. (model, checkpoint, provider) captured on every row so a
--      future re-embed can target rows by model without adding
--      metadata columns. Sysconfig-driven default is
--      "ViT-L-14"/"openai"/"aa-clip-visual-local"; overrides via
--      env vars in the sidecar.
--   4. Dim 768 matches ViT-L/14 output. Named to match the existing
--      asset_embedding_d768 convention (dim in the name) even
--      though the two tables are independent.
--   5. Per-instance state — federated peers embed their own image
--      assets against their own sidecars; no cross-instance
--      federation for visual embeddings.

-- +goose Up

CREATE TABLE asset_visual_embedding (
    asset_id UUID PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
    embedding vector(768) NOT NULL,
    model TEXT NOT NULL,
    checkpoint TEXT NOT NULL,
    provider TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- HNSW cosine index — matches asset_embedding_d768's shape for
-- operator familiarity. Query path is cosine similarity via
-- pgvector's <=> operator.
CREATE INDEX idx_asset_visual_embedding_hnsw_cosine
    ON asset_visual_embedding
    USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);

-- Partial index for the backlog gauge — assets that ARE images
-- but LACK a visual embedding. The dashboard's
-- asset_visual_embedding_backlog gauge hits this via anti-join
-- against assets. Small partial index: rows come + go as the
-- backfill catches up.
CREATE INDEX idx_asset_visual_embedding_provider
    ON asset_visual_embedding (provider, model);

-- +goose Down

DROP INDEX IF EXISTS idx_asset_visual_embedding_provider;
DROP INDEX IF EXISTS idx_asset_visual_embedding_hnsw_cosine;
DROP TABLE IF EXISTS asset_visual_embedding;
