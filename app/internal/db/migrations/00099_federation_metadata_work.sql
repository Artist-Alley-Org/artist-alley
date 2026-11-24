-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00099_federation_metadata_work.sql
--
-- #1577 (spec v1 §18.3): durable, local obligations to send asset metadata
-- Updates to peers, processed in bounded pages (assetsend.MetadataWork).
--
-- Two kinds:
--
--   - 'asset': send asset_id's current metadata to every peer it is
--     shared with. peer_cursor is the last peer a committed page handled;
--     the next page starts after it.
--   - 'all': a policy change that concerns every local asset. Each page
--     adds one 'asset' row per local asset after asset_cursor; the asset
--     rows are then independent of it.
--
-- A row holds UUID references, its kind, its cursor and when it was
-- created, nothing else: no metadata value, claim, payload, capability or
-- omission reason. A new row is added for every change, never merged
-- into one already in progress, so a change made after a page passed a
-- peer is not lost. A row is deleted once its last page commits.
--
-- No foreign key, on purpose: a deleted asset's work completes as a no-op
-- instead of a source DELETE cascading into a row a processor holds while
-- it waits for that asset's lock; a peer cursor is a position, not a
-- reference; and an 'all' row's completion must not take its 'asset' rows
-- with it. Nothing schedules or produces work in this migration.

-- +goose Up

CREATE TABLE IF NOT EXISTS public.federation_metadata_work (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    kind text NOT NULL,
    asset_id uuid,
    asset_cursor uuid,
    peer_cursor uuid,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT federation_metadata_work_pkey PRIMARY KEY (id),
    CONSTRAINT federation_metadata_work_kind_check CHECK (kind = ANY (ARRAY['asset'::text, 'all'::text])),
    CONSTRAINT federation_metadata_work_shape_check CHECK (
        (kind = 'asset' AND asset_id IS NOT NULL AND asset_cursor IS NULL)
        OR (kind = 'all' AND asset_id IS NULL AND peer_cursor IS NULL))
);

CREATE INDEX IF NOT EXISTS federation_metadata_work_oldest_idx
    ON public.federation_metadata_work USING btree (created_at, id);

COMMENT ON TABLE public.federation_metadata_work IS 'Durable local obligations to send asset metadata Updates (#1577, spec v1 §18.3): ''asset'' rows (send asset_id''s current metadata to its peers, from peer_cursor on) and ''all'' rows (add an ''asset'' row for every local asset, from asset_cursor on). UUID references and cursors only, no metadata values. A new row per change; deleted when its last page commits. No foreign keys, on purpose.';

-- +goose Down

-- Down refuses while an obligation is pending, rather than discard work a
-- change committed: once none is left, it drops the table. The ACCESS
-- EXCLUSIVE lock is taken BEFORE the check and held by this migration's
-- transaction through the drop: it waits for any enqueue still in
-- progress and excludes new ones, so the check sees every row the drop
-- would remove.
LOCK TABLE public.federation_metadata_work IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.federation_metadata_work) THEN
        RAISE EXCEPTION '00099 down: metadata work is pending; Down would discard what committed changes require (#1577)'
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP TABLE IF EXISTS public.federation_metadata_work;
