-- artist-alley migration 00028 — rename resource_type → asset_type.
--
-- Last holdout of the original "resource"-as-domain-language from
-- the ResourceSpace baseline. We've moved to "asset" as the canonical
-- entity name everywhere else (the table is `assets`, the package is
-- `internal/assets`, the API endpoints are /assets, the frontend
-- components are AssetCard / AssetViewer / AssetPlaylist); this is
-- the last column + table still wearing the old name.
--
-- Scope:
--   1. Lookup table:  resource_type  →  asset_types
--   2. FK column:     assets.resource_type  →  assets.asset_type
--   3. FK constraint: assets_resource_type_fkey  →  assets_asset_type_fkey
--
-- Out of scope:
--   - RS legacy tables resource_type_field / _field_resource_type stay
--     as-is. Those drive the RS PHP admin UI's old metadata system; our
--     Go metadata code uses field_definition (different table).
--   - RS legacy resource table + its resource_type column stay as-is.
--     That's the RS-side asset entity, separate from our assets table.
--
-- ALTER COLUMN RENAME auto-updates indexes that reference the column
-- (assets_type_idx survives the rename without rebuild) and the FK
-- constraint auto-tracks the renamed parent table; the explicit
-- constraint rename below is for cosmetic consistency only.

-- +goose Up

ALTER TABLE resource_type RENAME TO asset_types;

ALTER TABLE assets RENAME COLUMN resource_type TO asset_type;
ALTER TABLE assets RENAME CONSTRAINT assets_resource_type_fkey TO assets_asset_type_fkey;

-- +goose Down

ALTER TABLE assets RENAME CONSTRAINT assets_asset_type_fkey TO assets_resource_type_fkey;
ALTER TABLE assets RENAME COLUMN asset_type TO resource_type;

ALTER TABLE asset_types RENAME TO resource_type;
