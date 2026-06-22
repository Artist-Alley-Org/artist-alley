-- 00014_creative_lineage.sql
--
-- Phase 1.14.E-1 — first internal MCP caller (img2img via ComfyUI
-- MCP bridge). This migration adds the lineage table that records
-- "derivative asset X was generated from source asset Y by provider
-- P with parameters Q" — the source-of-truth for ADR 0026's "output
-- as new asset, not replacement" rule.
--
-- # What lands
--
--   - creative_lineage — one row per derivative asset. PK on
--     derivative_asset_id (a derivative has exactly one source);
--     source asset is FK + indexed for "show me all variations of
--     this asset" queries. generation_metadata is JSONB so each
--     provider can record its own knobs (prompt, seed, steps, model,
--     mcp_server_name, mcp_tool, …) without a schema-extension dance
--     every time a new op or provider ships in E-2 / E-3.
--
-- # Why one row per derivative, not a tree table
--
-- A derivative has exactly one parent. Multi-step lineage (a variation
-- of a variation of an upload) is reconstructed by walking the chain
-- via source_asset_id. A polymorphic ancestry table would be wrong:
-- the relationship IS directly parent→child.
--
-- # Why JSONB metadata, not typed columns
--
-- ADR 0026 says "the new asset's metadata records the provider, the
-- prompt, the seed (if available), and the parameters used". Provider-
-- specific knobs vary (ComfyUI cares about denoise_strength + steps +
-- model; OpenAI img2img cares about quality + size). JSONB keeps the
-- schema stable across providers and ops.
--
-- # Append-only intent
--
-- ADR 0046's append-only-migrations rule applies once v1.0.0 ships;
-- pre-v1 the schema can still evolve. Write this as if append-only-
-- already (no down migration, no destructive edits expected).
--
-- # Federation
--
-- ADR 0026 says creative_lineage rows replicate via the federation
-- outbox eventually. Don't wire that here (out of scope for E-1; the
-- 1.22.D federation soak window applies). The row shape stays
-- forward-compatible: metadata only carries values that survive
-- federation (no node-local refs).

-- +goose Up
-- +goose StatementBegin

CREATE TABLE creative_lineage (
    derivative_asset_id  UUID PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
    source_asset_id      UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    generation_metadata  JSONB NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_creative_lineage_source ON creative_lineage(source_asset_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS creative_lineage;

-- +goose StatementEnd
