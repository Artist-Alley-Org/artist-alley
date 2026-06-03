-- artist-alley migration 00034 — split out Ebook + Audiobook from
-- their parent buckets and add Texture / Sprite / Code categories
-- for game-art + scripting workflows.
--
-- Rename: "Graphic Novel/Comic/Manga" → "Comic" (label was too
-- much; the bucket only carries cbr/cbz/cb7 now that ebooks are
-- pulling out).
--
-- New types:
--   - Ebook       — epub / mobi / azw / azw3 / fb2 / lit / prc /
--                   pdb. Split out of Comic because reading a
--                   novel and reading a graphic novel are different
--                   modes (reflowable text vs. paged panels).
--   - Audiobook   — m4b / aax. Dedicated audiobook containers
--                   only; plain mp3 / m4a stay Audio because the
--                   extension can't tell music from narration.
--   - Texture     — dds / ktx / ktx2 / basis / sbsar / sbs. Game-
--                   asset texture formats that exist *because*
--                   they're textures (none are plain photos).
--                   PNGs used as textures stay Image since you
--                   can't tell from the extension alone.
--   - Sprite      — aseprite / ase / pyxel. Aseprite / Pyxel
--                   project files used by 2D / pixel artists.
--                   Sheet PNGs stay Image.
--   - Code        — scripts an artist would share as a tool:
--                   Python (py), JavaScript / TypeScript variants,
--                   shell scripts, shaders (glsl/hlsl/vert/frag),
--                   game-engine scripts (gd, mel, ms / mxs),
--                   common compiled-language sources. Config
--                   files (yaml/toml/ini) stay Document — they're
--                   not what users mean by "share a script".
--
-- +goose Up
UPDATE asset_types SET name = 'Comic' WHERE ref = 8;

INSERT INTO asset_types (ref, name, icon, order_by) VALUES
    (10, 'Ebook',     'book',         100),
    (11, 'Audiobook', 'headphones',   110),
    (12, 'Texture',   'grid-3x3',     120),
    (13, 'Sprite',    'grid-2x2',     130),
    (14, 'Code',      'file-code-2',  140)
ON CONFLICT (ref) DO NOTHING;

-- Backfill: pull ebooks out of Comic.
UPDATE assets SET asset_type = 10
WHERE LOWER(file_extension) IN ('epub','mobi','azw','azw3','fb2','lit','prc','pdb');

-- Backfill: pull dedicated audiobook containers out of Audio.
UPDATE assets SET asset_type = 11
WHERE LOWER(file_extension) IN ('m4b','aax');

-- Backfill: move texture-format files out of Image.
UPDATE assets SET asset_type = 12
WHERE LOWER(file_extension) IN ('dds','ktx','ktx2','basis','sbsar','sbs');

-- Backfill: move aseprite / pyxel project files.
UPDATE assets SET asset_type = 13
WHERE LOWER(file_extension) IN ('aseprite','ase','pyxel');

-- Backfill: code / script files. No existing dataset assets are
-- expected to match (none seeded), but the UPDATE is safe + idempotent.
UPDATE assets SET asset_type = 14
WHERE LOWER(file_extension) IN (
    'py','js','jsx','ts','tsx','mjs','cjs',
    'c','cpp','cc','cxx','h','hpp','hh',
    'cs','java','go','rs','rb','php','swift','kt','kts','scala',
    'sh','bash','zsh','fish','ps1','bat','cmd',
    'lua','gd','tres','tscn',
    'mel','ms','mxs','hda','vex',
    'hlsl','glsl','vert','frag','shader','cginc','usf'
);

-- +goose Down
-- Push every reclassified asset back to its old parent so the
-- type rows can drop cleanly.
UPDATE assets SET asset_type = 8 WHERE asset_type = 10;
UPDATE assets SET asset_type = 4 WHERE asset_type = 11;
UPDATE assets SET asset_type = 1 WHERE asset_type IN (12, 13);
UPDATE assets SET asset_type = 2 WHERE asset_type = 14;
DELETE FROM asset_types WHERE ref IN (10, 11, 12, 13, 14);
UPDATE asset_types SET name = 'Graphic Novel/Comic/Manga' WHERE ref = 8;
