-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00037_asset_admin_capability_and_deleted_by.sql
--
-- Two halves of one fix (#930, #931).
--
-- ## Half one: `assets.admin`
--
-- `UpdateAsset` and `DeleteAsset` checked only that the caller was
-- authenticated. Any signed-in account could rewrite the metadata of,
-- or soft-delete, every asset in the instance — and only `system.admin`
-- could restore, so one ordinary account could remove a studio's
-- library with nobody below super-admin able to undo it. Posts have
-- gated on `posts.admin` and collections on `collections.admin` since
-- they were written; assets were the outlier.
--
-- Closing that hole and building "a concept art director may manage a
-- file belonging to someone on their team" are the same edit: you
-- cannot express the second without first establishing that a plain
-- team member may NOT edit a colleague's asset. So the fix is a
-- capability rather than a bare owner check.
--
-- Named `assets.admin` to read alike beside `posts.admin` and
-- `collections.admin`, which are the two capabilities it behaves like.
--
-- ## What `assets.admin` deliberately does NOT confer
--
-- Publication. The dangerous permission is never "edit the thing", it
-- is "change who can reach the thing" — the same separation Kubernetes
-- draws with its `escalate` / `bind` verbs, where permission-modifying
-- verbs are withheld unless strictly required because their holder can
-- widen access past what they were granted.
--
-- On an asset that lever is `status`: `visibility/predicate.go` demands
-- `status = 'active'` before an anonymous reader may see a row, so
-- flipping a colleague's `draft` asset to `active` publishes their
-- unfinished work to the open internet. `assets.admin` therefore does
-- not authorise a `status` change; the owner and `system.admin` do (see
-- `canMutateAsset` / `UpdateAsset` in assets/handler.go).
--
-- The vocabulary already anticipated this: `assets.publish`,
-- `assets.archive`, `assets.review`, `assets.submit` and
-- `assets.unarchive` are separate workflow verbs seeded in 00002.
-- (None is wired to a gate yet — that is the workflow-state arc, not
-- this fix.) Publication being unbundled already is exactly why
-- `assets.admin` can slot beside them without absorbing it.
--
-- Not seeded onto any role. Grant it per-team to the people who should
-- hold it; `user_capability_grants.team_id` scopes it, and the closure
-- expansion in `EffectiveScopedCapabilitiesForUser` already makes a
-- grant on a parent team cover every descendant.
--
-- ## `posts.admin` and `collections.admin` were never grantable
--
-- Found while wiring the above: both exist ONLY as Go string constants
-- (posts.CapPostsAdmin, collections.CapCollectionsAdmin). Neither has
-- ever been a row in `capabilities`. Both
-- `user_capability_grants.capability_code` and
-- `role_capabilities.capability_code` are FK-constrained to
-- `capabilities(code)` — so granting either one, to a user or to a
-- role, fails with 23503. The two moderator gates that read them
-- (`canMutatePost`, `canMutateCollection`) could only ever be satisfied
-- by `system.admin`.
--
-- They are seeded here because #930 makes `canMutatePost` team-scope
-- aware, and a scope-aware check on a capability nobody can hold is
-- still nothing. Seeding a code confers nothing by itself: no role
-- gains it and no grant is written. It only makes the existing gates
-- reachable by an operator who chooses to grant them.
--
-- ## Half two: `deleted_by_user_ref`
--
-- `assets`, `posts` and `collections` all carried `deleted_at` and
-- `deleted_reason` and no record of WHO deleted them. #931's rule turns
-- entirely on that fact: "users should be able to recover their own
-- deleted files, unless deleted by an admin. Then they would need to
-- request for restoration."
--
-- The audit log does capture the actor, but deciding an authorisation
-- branch by querying the audit trail would make the audit log
-- load-bearing for access control, which it is not built to be — it is
-- a record of what happened, retained on its own schedule, not a
-- permission store.
--
-- NULL means "we do not know who deleted this": a row deleted before
-- this migration, or a system-scheduled retention delete
-- (`scheduled_actions.created_by` is itself nullable for
-- system-scheduled rows). Both resolve the same way — nobody may
-- self-restore it, only `system.admin` can. Fail closed.
--
-- NO foreign key to `"user"`, deliberately, and this is the odd-looking
-- decision in the file. `assets.owner_user_ref`,
-- `collections.owner_user_ref` and `posts.author_user_ref` — the three
-- user references already on these exact tables — carry no FK either,
-- and a new column that constrains harder than the OWNER column beside
-- it is a constraint nobody expects. It also breaks in the one
-- direction that matters: a deleter row that has since been removed
-- would take the delete record with it (CASCADE) or need a SET NULL
-- rule (an extra write on user deletion), where a dangling ref simply
-- fails to match any caller and the row falls back to system.admin —
-- the same answer NULL already gives. Fail-closed either way.

-- +goose Up

INSERT INTO public.capabilities (code, description, created_at, required_license_feature)
VALUES
    (
        'assets.admin',
        'Manage assets belonging to other users — metadata edit, soft-delete and restore. Scope it to a team via user_capability_grants.team_id and it covers that team and every descendant. Does NOT confer publication: changing an asset''s status is reserved to the owner and system.admin (#930).',
        now(),
        NULL
    ),
    (
        'posts.admin',
        'Manage posts belonging to other users — edit, delete, membership and grants. Held globally it is the instance moderator role; scoped to a team it covers that team and every descendant, but does NOT confer a change of the post''s visibility (#930). Read by canMutatePost since the package was written; never seeded until now, so until this migration it could not be granted at all.',
        now(),
        NULL
    ),
    (
        'collections.admin',
        'Manage collections belonging to other users — edit, delete, membership and grants. Read by canMutateCollection since the package was written; never seeded until now, so until this migration it could not be granted at all (#930).',
        now(),
        NULL
    );

ALTER TABLE public.assets      ADD COLUMN deleted_by_user_ref bigint;
ALTER TABLE public.posts       ADD COLUMN deleted_by_user_ref bigint;
ALTER TABLE public.collections ADD COLUMN deleted_by_user_ref bigint;

-- +goose Down

ALTER TABLE public.collections DROP COLUMN IF EXISTS deleted_by_user_ref;
ALTER TABLE public.posts       DROP COLUMN IF EXISTS deleted_by_user_ref;
ALTER TABLE public.assets      DROP COLUMN IF EXISTS deleted_by_user_ref;

-- The FK from user_capability_grants.capability_code is ON DELETE
-- RESTRICT, and role_capabilities likewise, so clear every reference
-- before removing the codes themselves.
DELETE FROM public.role_capabilities
 WHERE capability_code IN ('assets.admin', 'posts.admin', 'collections.admin');
DELETE FROM public.user_capability_grants
 WHERE capability_code IN ('assets.admin', 'posts.admin', 'collections.admin');

DELETE FROM public.capabilities
 WHERE code IN ('assets.admin', 'posts.admin', 'collections.admin');
