-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00027_preview_gif_concurrency.sql
--
-- Concurrency cap for preview.gif (#832).
--
-- 4, which sits deliberately between the two families this job type
-- straddles. A still GIF is a raster ladder — Go-native, sub-second,
-- and preview.raster runs uncapped for exactly that reason. An animated
-- one is a short silent video: one ffmpeg pass over a clip that is
-- measured in seconds, not the multi-rendition HLS ladder that earned
-- preview.video its cap of 2 (migration 00004).
--
-- Capping at the video number would throttle the still case for no
-- reason; leaving it uncapped would let a bulk import of animated GIFs
-- take the whole pool (workerPoolSize() is NumCPU/2 clamped to 8) and
-- starve the 3D and video reservations that share it. 4 is half the
-- pool: enough that GIFs drain in parallel, bounded enough that the
-- expensive types still get slots.
--
-- Caps are loaded at boot (server.go, per #278), so this takes effect on
-- the next restart rather than the moment the migration lands. That is
-- fine: an install that has not restarted is running the old code, which
-- has no such job type to cap.

-- +goose Up
-- +goose StatementBegin
INSERT INTO public.system_config (key, value) VALUES
    ('jobs.type_concurrency.preview.gif', '4')
ON CONFLICT (key) DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM public.system_config
WHERE key = 'jobs.type_concurrency.preview.gif';
-- +goose StatementEnd
