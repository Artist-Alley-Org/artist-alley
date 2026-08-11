-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00038_publication_capability_descriptions.sql
--
-- Corrects the five workflow-verb capability descriptions seeded in
-- 00001 (#938). No schema change, no new code, no new grant — this
-- migration only rewrites `capabilities.description`, which is the text
-- an operator reads in the admin capability list at the moment they
-- decide whether to grant something.
--
-- ## Why it is worth a migration
--
-- All five were written against a `draft → pending_review → published →
-- archived` state machine that was never built. The live constraint is
--
--     CONSTRAINT assets_status_check
--       CHECK (status = ANY (ARRAY['draft', 'active', 'archived']))
--
-- so there is no `pending_review` and no `published`. Every one of the
-- five descriptions therefore named at least one state that does not
-- exist, and three of them described the ONLY transitions they govern
-- in terms of it. An operator granting `assets.publish` was told it
-- moved an asset to `published`; it moves it to `active`, and `active`
-- is the exact value `visibility/predicate.go` requires before an
-- anonymous reader may see the row. Wrong text on a disclosure lever is
-- not cosmetic.
--
-- ## The three that are now enforced
--
-- #938 wires `assets.publish`, `assets.archive` and `assets.unarchive`
-- to `UpdateAsset`'s status gate (see canTransitionAssetStatus in
-- assets/handler.go for the full six-pair mapping and its reasoning).
-- In short: entering `active` ALWAYS requires `assets.publish` and
-- accepts no substitute, because `→ active` is the disclosure act and a
-- second route into it would silently make some other verb a
-- publication right. Everything else is governed by the verb that names
-- it — which is why `archived → active` requires publish AND unarchive,
-- and why `active → draft` (retraction, the publish decision pointing
-- the other way) requires publish.
--
-- ## The two that are not
--
-- `assets.submit` and `assets.review` are left UNENFORCED and are said
-- so out loud. They gate the exit from `pending_review`, and there is
-- no `pending_review` to exit; adding one is a schema and product
-- decision belonging to the workflow-state arc (#895/#896/#897), and
-- #951 carries the choice between building it and deleting the two
-- codes outright.
--
-- Fixing three of five and leaving two describing a fictional machine
-- would be worse than fixing none: the reader would have no way to tell
-- which half of the table to believe. So both are rewritten to state
-- plainly that holding them confers nothing today.
--
-- ## And `assets.admin`
--
-- 00037's text ends "Does NOT confer publication: changing an asset's
-- status is reserved to the owner and system.admin (#930)". The first
-- clause is still true and is the point; the second stopped being true
-- the moment the three verbs were wired, so it is restated here. Same
-- rule as above — a description that was accurate when written and is
-- not accurate now is exactly the trap this migration exists to remove.
--
-- The `workflow_transitions` rows seeded in 00001 also reference these
-- five codes. They belong to a different subsystem (`workflow.Transition`
-- moves `state_id`, not `status`) and are deliberately untouched here.
--
-- Down restores the previous text verbatim, wrong states and all, so a
-- rollback lands on exactly the rows the previous migration left.

-- +goose Up

UPDATE public.capabilities
   SET description = 'Publish an asset — make it publicly reachable by setting status to `active`. Required for EVERY transition INTO active (from draft, and together with assets.unarchive from archived), and for the retraction that reverses it (active → draft). This is the disclosure lever: visibility requires status = ''active'' before an anonymous reader may see an asset. Scope it to a team via user_capability_grants.team_id or a team-scoped role and it covers that team and every descendant.'
 WHERE code = 'assets.publish';

UPDATE public.capabilities
   SET description = 'Archive an asset — set status to `archived`, from either draft or active. Retiring content only removes reach, so this confers no power to publish: a holder of assets.archive alone cannot move any asset into active. Team-scopable like the others.'
 WHERE code = 'assets.archive';

UPDATE public.capabilities
   SET description = 'Take an asset out of the archive. On its own it performs archived → draft, returning the work to its owner for rework. Restoring an archived asset all the way to `active` is also a publication, so it additionally requires assets.publish. Team-scopable like the others.'
 WHERE code = 'assets.unarchive';

UPDATE public.capabilities
   SET description = 'UNENFORCED — granting this confers nothing (#951). It names a `pending_review` status, and the live constraint on assets.status permits only draft, active and archived. Whether the review state gets built (#895/#896/#897) or this code is removed is decided in #951; until then it is a no-op and should not be granted in the belief that it delegates anything.'
 WHERE code = 'assets.submit';

UPDATE public.capabilities
   SET description = 'UNENFORCED — granting this confers nothing (#951). It names a `pending_review` status, and the live constraint on assets.status permits only draft, active and archived. Whether the review state gets built (#895/#896/#897) or this code is removed is decided in #951; until then it is a no-op and should not be granted in the belief that it delegates anything.'
 WHERE code = 'assets.review';

UPDATE public.capabilities
   SET description = 'Manage assets belonging to other users — metadata edit, soft-delete and restore. Scope it to a team via user_capability_grants.team_id or a team-scoped role and it covers that team and every descendant. Does NOT confer publication: changing an asset''s status needs the owner, system.admin, or the matching publication verb — assets.publish, assets.archive or assets.unarchive (#930, #938).'
 WHERE code = 'assets.admin';

-- +goose Down

UPDATE public.capabilities
   SET description = 'Manage assets belonging to other users — metadata edit, soft-delete and restore. Scope it to a team via user_capability_grants.team_id and it covers that team and every descendant. Does NOT confer publication: changing an asset''s status is reserved to the owner and system.admin (#930).'
 WHERE code = 'assets.admin';

UPDATE public.capabilities
   SET description = 'Submit an asset for review (draft → pending_review)'
 WHERE code = 'assets.submit';

UPDATE public.capabilities
   SET description = 'Approve or reject an asset in review (pending_review → published)'
 WHERE code = 'assets.review';

UPDATE public.capabilities
   SET description = 'Publish an asset directly without review (draft → published)'
 WHERE code = 'assets.publish';

UPDATE public.capabilities
   SET description = 'Archive a published asset (published → archived)'
 WHERE code = 'assets.archive';

UPDATE public.capabilities
   SET description = 'Restore an archived asset (archived → published)'
 WHERE code = 'assets.unarchive';
