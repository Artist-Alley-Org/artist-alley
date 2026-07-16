-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00004_preview_type_concurrency.sql
--
-- Per-type concurrency caps for the expensive preview jobs (#355).
--
-- `aa seed` now dispatches a preview job per asset, so a single seed of
-- the demo dataset enqueues ~970 jobs at once — 328 of them 3D renders.
--
-- The pool is already bounded globally: workerPoolSize() is NumCPU/2
-- clamped to 8, so at most 8 jobs ever run concurrently no matter how
-- many are queued. That alone stops a literal 970-wide stampede.
--
-- What it does NOT stop is 3D monopolising all 8 slots. Blender renders
-- are the slowest job we have; 328 of them can hold every worker for a
-- long stretch while the 387 cheap raster previews — the ones the browse
-- grid actually needs to render a card — wait behind them. Seeded caps
-- keep the mix moving: 3D trickles on 2 workers, 6 stay free for raster
-- and friends.
--
-- The gate is real (Pool.tryReserve, loaded at boot by server.go per
-- #278); the baseline already seeds caps for the ai.* types the same
-- way. Values mirror that precedent — ai.caption is 2, ai.transcribe 1.
-- Operators can retune these rows without a deploy.

-- +goose Up
-- +goose StatementBegin
INSERT INTO public.system_config (key, value) VALUES
    ('jobs.type_concurrency.preview.3d', '2'),
    ('jobs.type_concurrency.preview.video', '2')
ON CONFLICT (key) DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM public.system_config
WHERE key IN (
    'jobs.type_concurrency.preview.3d',
    'jobs.type_concurrency.preview.video'
);
-- +goose StatementEnd
