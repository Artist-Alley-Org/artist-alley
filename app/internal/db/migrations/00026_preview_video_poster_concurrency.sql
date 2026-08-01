-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00026_preview_video_poster_concurrency.sql
--
-- Concurrency cap for the cheap video-poster job (#818).
--
-- preview.video is capped at 2 (migration 00004) because it is the most
-- expensive job in the system — measured at 74% of render CPU. That cap
-- is right for an HLS ladder and wrong for the work that was trapped
-- behind it: a single input seek and one JPEG encode, which is all it
-- takes to put a picture on a card. Splitting that out (#818) only helps
-- if it is allowed to drain, so preview.video.poster gets 8 — the whole
-- global worker pool, since workerPoolSize() is NumCPU/2 clamped to 8.
--
-- Setting it AT the pool size rather than below is deliberate: the cap's
-- job here is not to reserve headroom, it is to declare that this type
-- may use whatever is free. Posters finish in seconds, so a burst of
-- them holds every slot only briefly, and the expensive types that share
-- the pool have caps of their own (3d 2, video 2) which this cannot
-- override — those two keep their reservations whatever the posters do.
--
-- Caps are loaded at boot (server.go, per #278), so this takes effect on
-- the next restart rather than the moment the migration lands. That is
-- fine: an install that has not restarted is running the old code, which
-- has no such job type to cap.

-- +goose Up
-- +goose StatementBegin
INSERT INTO public.system_config (key, value) VALUES
    ('jobs.type_concurrency.preview.video.poster', '8')
ON CONFLICT (key) DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM public.system_config
WHERE key = 'jobs.type_concurrency.preview.video.poster';
-- +goose StatementEnd
