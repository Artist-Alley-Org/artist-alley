-- artist-alley migration 00031 — seed Font / Book / Illustration
-- asset types and re-classify existing assets.
--
-- The 6 baseline types (Photo / Document / Video / Audio / 3D Object /
-- Archive) lump together formats that have very different viewer
-- needs:
--
--   - Fonts (ttf/otf/ttc/woff/woff2) had no viewer affinity with
--     other Documents; they have their own FontView with a live
--     specimen + try-it text editor, and per-type metadata
--     (designer / glyph count / license / version).
--   - eBooks + comics (epub/cbr/cbz/cb7/mobi/azw/azw3) are paged
--     reading content, not generic Documents.
--   - Illustration source files (psd/ai/sketch/fig/xd/eps/cdr/etc.)
--     are layered editor projects, not finished raster output.
--     The distinction matters for an artist platform: a "Photo"
--     of an illustration is the export the artist shows the world,
--     while the Illustration source is what they actually work on.
--
-- Refs continue past the existing 1..6 range. Order_by leaves
-- headroom for inserting more types later. Icons are lucide-svelte
-- names already bundled with the frontend.
--
-- +goose Up
INSERT INTO asset_types (ref, name, icon, order_by) VALUES
    (7, 'Font',          'type',         70),
    (8, 'Book',          'book-open',    80),
    (9, 'Illustration',  'palette',      90)
ON CONFLICT (ref) DO NOTHING;

-- Backfill: re-type existing assets whose extension matches the new
-- categories. Anything that's currently a Document (2) or unset
-- gets recategorized; we don't touch Photo (1) for SVG/etc because
-- raster + vector finished art lives there by design — only the
-- source-file extensions (psd/ai/eps/...) move to Illustration.
--
-- Font: fonts that were misclassified as Document.
UPDATE assets
SET asset_type = 7
WHERE LOWER(file_extension) IN ('ttf','otf','ttc','otc','woff','woff2');

-- Book / publication: paged reading content. PDF deliberately
-- stays in Document — it's used for so many non-book things
-- (forms, sheet music, signed contracts, technical specs) that
-- forcing it into Book would mis-bucket the majority.
UPDATE assets
SET asset_type = 8
WHERE LOWER(file_extension) IN ('epub','cbr','cbz','cb7','mobi','azw','azw3','fb2','lit');

-- Illustration source files. PSD currently rides as Photo because
-- it can be read by raster pipelines; promote to Illustration so
-- the source-vs-export split shows in filters.
UPDATE assets
SET asset_type = 9
WHERE LOWER(file_extension) IN ('psd','ai','sketch','fig','xd','eps','cdr','afdesign','afphoto','afpub','clip','ora','kra');

-- +goose Down
-- Reverse the backfill — push everything back into Document so the
-- type rows can be dropped without a constraint violation.
UPDATE assets SET asset_type = 2
  WHERE asset_type IN (7, 8, 9);
DELETE FROM asset_types WHERE ref IN (7, 8, 9);
