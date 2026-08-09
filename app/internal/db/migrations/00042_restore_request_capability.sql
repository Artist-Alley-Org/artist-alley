-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00042_restore_request_capability.sql
--
-- Give the restoration APPEAL (#931) a marker capability of its own,
-- and widen `resource_request` from asset-only to the three
-- soft-deletable kinds it now has to carry.
--
-- ## Why a second marker code rather than reusing content.access.request
--
-- 00035 seeded `content.access.request` for #881 and explained why the
-- code has to be narrow: `requested_capability` is requester-controlled
-- input, so a decide gate widened to a non-operator must be scoped to a
-- code that confers nothing. That reasoning applies again here, and it
-- applies to a DIFFERENT decider.
--
--   content.access.request → the asset's OWNER may decide it.
--   content.restore.request → the target's DELETER (or system.admin,
--                             per auth.CanRestoreDeleted) may decide it.
--
-- Those two authorities are not the same people and must not be
-- interchangeable. If an appeal reused the access marker, the owner
-- disjunct in requests/http.go would accept it — and the owner of a
-- moderated item is precisely the person the moderation was against.
-- They would approve their own appeal. A separate code is what lets
-- each gate name exactly one payload, which is the #881 lesson stated
-- as schema rather than as a comment.
--
-- Note what is NOT symmetric: `share.grant` decides an access request
-- but does NOT decide an appeal. Authority over sharing is not
-- authority over moderation, and the restore rule (auth.CanRestoreDeleted)
-- deliberately turns on WHO DELETED the row rather than on the caller's
-- standing rank.
--
-- ## What it confers: NOTHING
--
-- Same as 00035, and for a stronger reason. A granted appeal performs
-- the restore directly — requests.Handler.Grant calls
-- softdelete.Restore<Kind> and writes NO user_capability_grants row at
-- all on this branch. The code exists to TYPE the request row so the
-- gate can recognise it; nobody ever holds it. Not seeded onto any
-- role.
--
-- ## target_kind, and the rename
--
-- `resource_request` was born asset-only: one uuid column named
-- `target_asset_id`, no FK (the baseline never had one), no kind
-- discriminator. An appeal can name a post or a collection, so the row
-- needs to say which table its uuid belongs to.
--
-- The column is renamed to `target_id` in the same breath. Keeping
-- `target_asset_id` next to a `target_kind` that can read 'collection'
-- would ship a column name that is false for two of its three values —
-- and the next person to write a JOIN would believe the name. Nothing
-- outside this repository consumes the field (pre-MVP, no operators),
-- so there is no compatibility argument for carrying the lie.
--
-- Postgres carries indexes through a column rename automatically, so
-- the two indexes on the old name are rebuilt here only to fix what
-- they SAY: `idx_resource_request_by_asset` becomes
-- `idx_resource_request_by_target` and gains `target_kind` as its lead
-- column, and the 00035 uniqueness rule gains `target_kind` because
-- "one pending ask" is now (requester, kind, id, capability) — a post
-- and an asset are different asks even in the (unlikely, but
-- unconstrained) event their uuids collide.

-- +goose Up

INSERT INTO public.capabilities (code, description, created_at, required_license_feature)
VALUES (
    'content.restore.request',
    'Marker written by the restoration-appeal workflow (#931) when an owner asks the deleter to undo a soft delete. Confers nothing; granting it performs the restore instead of writing a capability grant.',
    now(),
    NULL
);

ALTER TABLE public.resource_request
    ADD COLUMN target_kind text NOT NULL DEFAULT 'asset';

ALTER TABLE public.resource_request
    ADD CONSTRAINT resource_request_target_kind_check
    CHECK (target_kind = ANY (ARRAY['asset'::text, 'post'::text, 'collection'::text]));

ALTER TABLE public.resource_request
    RENAME COLUMN target_asset_id TO target_id;

DROP INDEX IF EXISTS public.idx_resource_request_by_asset;
CREATE INDEX idx_resource_request_by_target
    ON public.resource_request USING btree (target_kind, target_id);

DROP INDEX IF EXISTS public.resource_request_one_pending_per_ask;
CREATE UNIQUE INDEX resource_request_one_pending_per_ask
    ON public.resource_request (requester_user_ref, target_kind, target_id, requested_capability)
    WHERE (state = 'pending');

-- +goose Down

-- Rows naming the appeal capability go first: the FK from
-- resource_request.requested_capability is ON DELETE RESTRICT, so the
-- capability cannot be removed while any request still names it.
DELETE FROM public.resource_request WHERE requested_capability = 'content.restore.request';

-- Non-asset targets cannot survive the column drop. The pre-00042
-- schema has one uuid column that means "an asset", and leaving a post
-- id sitting in it would be worse than losing the row: every reader
-- downstream joins it to `assets` and would silently find nothing.
DELETE FROM public.resource_request WHERE target_kind <> 'asset';

DROP INDEX IF EXISTS public.resource_request_one_pending_per_ask;
DROP INDEX IF EXISTS public.idx_resource_request_by_target;

ALTER TABLE public.resource_request
    RENAME COLUMN target_id TO target_asset_id;

ALTER TABLE public.resource_request
    DROP CONSTRAINT IF EXISTS resource_request_target_kind_check;

ALTER TABLE public.resource_request DROP COLUMN target_kind;

CREATE INDEX idx_resource_request_by_asset
    ON public.resource_request USING btree (target_asset_id);

CREATE UNIQUE INDEX resource_request_one_pending_per_ask
    ON public.resource_request (requester_user_ref, target_asset_id, requested_capability)
    WHERE (state = 'pending');

-- Marker grants confer nothing, so dropping them changes no caller's
-- authority. In practice there are none — the appeal branch never
-- writes a grant — but 00035's Down does the same and an appeal that
-- somehow took the access path should not wedge a rollback.
DELETE FROM public.user_capability_grants WHERE capability_code = 'content.restore.request';

DELETE FROM public.capabilities WHERE code = 'content.restore.request';
