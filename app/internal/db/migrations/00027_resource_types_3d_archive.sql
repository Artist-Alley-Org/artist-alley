-- artist-alley migration 00027 — seed 3D Object + Archive resource types.
--
-- Phase 1.18.B-12f.
--
-- The 4 baseline RS resource types (Photo / Document / Video / Audio)
-- don't have a slot for 3D model uploads or general archive bundles.
-- The preview worker already dispatches by file extension, so this
-- migration is purely a taxonomy fix: it lets the upload UI offer a
-- proper category for these formats and gives the browse filters
-- something to group on.
--
-- Refs picked outside the RS-imported range (1..4) so they don't
-- collide with future RS data syncs.
--
-- Icons match lucide-svelte names already in the frontend bundle.
--
-- +goose Up
INSERT INTO resource_type (ref, name, icon, order_by) VALUES
    (5, '3D Object', 'box',     50),
    (6, 'Archive',   'archive', 60)
ON CONFLICT (ref) DO NOTHING;

-- +goose Down
DELETE FROM resource_type WHERE ref IN (5, 6);
