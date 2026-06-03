-- artist-alley migration 00033 — rename Photo → Image, retire
-- Illustration, rename Book → Graphic Novel/Comic/Manga.
--
-- The taxonomy from 00031 didn't fit the audience. On an artist
-- platform "Photo" reads as "photography only" — but the bucket
-- actually carries every raster + vector image (paintings,
-- concept art, finished illustrations, screenshots…). "Image"
-- describes the bucket honestly.
--
-- "Illustration" likewise read like a content genre when it was
-- really meant to separate editor sources (psd/ai/eps) from
-- finished outputs. Users don't care about that split — they want
-- to find "the image" regardless of whether it's a finished png
-- or a layered psd source. Fold Illustration back into Image.
--
-- "Book" was too generic for what we actually carry (cbr/cbz/cb7
-- comics + manga + graphic-novel ebooks). The user-facing label
-- "Graphic Novel/Comic/Manga" matches the artist-platform use
-- case directly; epub/mobi/azw stay in this bucket because on
-- this platform they're overwhelmingly illustrated.
--
-- +goose Up
UPDATE asset_types SET name = 'Image',                       icon = 'image'     WHERE ref = 1;
UPDATE asset_types SET name = 'Graphic Novel/Comic/Manga',   icon = 'book-open' WHERE ref = 8;

-- Move every Illustration (9) asset back into Image (1) so the
-- type row can be dropped without violating the asset FK.
UPDATE assets SET asset_type = 1 WHERE asset_type = 9;
DELETE FROM asset_types WHERE ref = 9;

-- +goose Down
INSERT INTO asset_types (ref, name, icon, order_by)
  VALUES (9, 'Illustration', 'palette', 90)
  ON CONFLICT (ref) DO NOTHING;
-- We can't reliably reverse the asset-side merge (the original
-- illustrations and the originally-photo assets are now
-- indistinguishable in asset_type=1); leave them as Image and
-- let admins reclassify by extension if they really need it back.
UPDATE asset_types SET name = 'Photo', icon = 'image'    WHERE ref = 1;
UPDATE asset_types SET name = 'Book',  icon = 'book-open' WHERE ref = 8;
