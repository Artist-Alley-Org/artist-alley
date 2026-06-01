-- artist-alley migration 00032 — reclassify camera-proxy + broadcast
-- video formats that were landing as Photo (or Document) instead of
-- Video. The Go assetTypeFor() in this commit covers new uploads;
-- this migration sweeps the existing dataset.
--
-- Affected: .lrv (GoPro / DJI low-res proxies), .insv (Insta360),
-- .mts / .m2ts (AVCHD), .vob (DVD), .mxf (broadcast), .f4v (Flash),
-- .m4v + .ts (MPEG-4 variants that historically slipped through).
--
-- +goose Up
UPDATE assets
SET asset_type = 3
WHERE LOWER(file_extension) IN (
    'lrv','insv','mts','m2ts','vob','mxf','f4v','m4v','ts'
)
  AND asset_type != 3;

-- +goose Down
-- Best-effort revert: push back to Photo (1) since that was the
-- bucket most of these were sitting in pre-migration. Forward-only
-- migrations are preferred for taxonomy fixes; this Down is here
-- for completeness, not as a recommended path.
UPDATE assets
SET asset_type = 1
WHERE LOWER(file_extension) IN (
    'lrv','insv','mts','m2ts','vob','mxf','f4v','m4v','ts'
)
  AND asset_type = 3;
