-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00073_iiif_manifest_invalidation_on_field_value.sql
--
-- #196: a field-value write drops that asset's cached IIIF manifests.
--
-- An asset's IIIF manifest is built from the asset row AND from its
-- asset_field_value rows: the canvas size (pixel_width / pixel_height,
-- #196), the navPlace point (gps_latitude / gps_longitude) and the
-- metadata pairs. The manifest is cached per process in the
-- `iiif.presentation` domain under two keys per asset,
-- `asset::<id>::anon` and `asset::<id>::auth`
-- (iiif/presentation/cache.go, Cache.Key), and nothing invalidated those
-- keys when a field value changed.
--
-- The preview pipeline records the canvas size through
-- asset/pixeldims.Record, AFTER upload, and again on every re-render. So
-- a manifest requested before the preview finished (an authenticated
-- caller may read a still-processing asset), or before a replace-file
-- re-render, kept the 1200x900 placeholder or the old size until some
-- unrelated asset write or LRU eviction dropped it. That defeats #196 for
-- the ordinary preview timing.
--
-- The fix is here, in the trigger every asset_field_value write already
-- passes through, rather than in Go at each writer: Record is one of
-- several writers (metadata edits, the extractor, the seeder, bulk
-- import), and the trigger cannot be bypassed by a new one. It adds two
-- KEYED notifications on the existing `cache_invalidate` channel, in the
-- exact cache.Payload wire shape ({"domain","key","op":"upsert"}) the
-- Registry dispatches, so every app process drops exactly this asset's
-- two manifest entries and nothing else; no domain-wide purge.
--
-- pg_notify inside a trigger is transactional: Postgres delivers the
-- notification only when the writing transaction COMMITS (and drops it
-- on rollback), so a reader that refills the cache after the
-- notification sees the committed value. The existing body is
-- unchanged: rebuild_asset_search_text and the `asset_by_id`
-- notification stay exactly as in the baseline.
--
-- Keys follow the trigger's existing target rule: NEW.asset_id on
-- INSERT / UPDATE, OLD.asset_id on DELETE.

-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.asset_field_value_trigger() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    target_asset UUID;
BEGIN
    IF (TG_OP = 'DELETE') THEN
        target_asset := OLD.asset_id;
    ELSE
        target_asset := NEW.asset_id;
    END IF;

    PERFORM rebuild_asset_search_text(target_asset);

    -- Broadcast invalidation so app-instance LRUs can drop their copy.
    PERFORM pg_notify(
        'cache_invalidate',
        json_build_object(
            'domain', 'asset_by_id',
            'key',    target_asset::TEXT,
            'op',     'upsert'
        )::TEXT
    );

    -- #196: the asset's IIIF manifests (canvas size, navPlace, metadata
    -- pairs) are built from these rows. Drop both cached variants.
    PERFORM pg_notify(
        'cache_invalidate',
        json_build_object(
            'domain', 'iiif.presentation',
            'key',    'asset::' || target_asset::TEXT || '::anon',
            'op',     'upsert'
        )::TEXT
    );
    PERFORM pg_notify(
        'cache_invalidate',
        json_build_object(
            'domain', 'iiif.presentation',
            'key',    'asset::' || target_asset::TEXT || '::auth',
            'op',     'upsert'
        )::TEXT
    );

    IF (TG_OP = 'DELETE') THEN RETURN OLD; ELSE RETURN NEW; END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.asset_field_value_trigger() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    target_asset UUID;
BEGIN
    IF (TG_OP = 'DELETE') THEN
        target_asset := OLD.asset_id;
    ELSE
        target_asset := NEW.asset_id;
    END IF;

    PERFORM rebuild_asset_search_text(target_asset);

    -- Broadcast invalidation so app-instance LRUs can drop their copy.
    PERFORM pg_notify(
        'cache_invalidate',
        json_build_object(
            'domain', 'asset_by_id',
            'key',    target_asset::TEXT,
            'op',     'upsert'
        )::TEXT
    );

    IF (TG_OP = 'DELETE') THEN RETURN OLD; ELSE RETURN NEW; END IF;
END;
$$;
-- +goose StatementEnd
