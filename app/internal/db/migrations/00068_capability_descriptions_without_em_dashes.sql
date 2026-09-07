-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00068_capability_descriptions_without_em_dashes.sql
--
-- `capabilities.description` is an OPERATOR-FACING string: the admin
-- capability list renders it verbatim beside each code, and it is the
-- only place an administrator is told what granting a code actually
-- does. The standing house rule is that no visible em dash appears on
-- any surface a person reads, so twelve of them across ten rows had to
-- go.
--
-- WHAT THIS DOES NOT DO. It does not reword anything. Each description
-- keeps its sentences, its issue references, its backticks and its
-- capitalisation; only the dash itself becomes ordinary punctuation, a
-- colon where the dash introduced an explanation or a list, and a pair
-- of parentheses where two dashes bracketed an aside. Meaning is
-- unchanged in all ten, which is what makes the Down below able to
-- restore the previous text verbatim.
--
-- WHY ALL TEN AND NOT JUST THE NEW ONE. Only
-- `assets.metadata.bulk_edit` arrived with 00066; the other nine are
-- older. They render in the same column, under the same rule, and
-- correcting one while leaving nine standing would be a worse state
-- than either fixing all of them or none.
--
-- ⛔ 00066 AND THE OTHER SEEDING MIGRATIONS ARE NOT EDITED. An applied
-- migration is a historical record of what ran, so the correction is a
-- new forward step. A database that has never run 00066 is unaffected
-- by that file changing; one that has would never re-run it.
--
-- DRIFT AND IDEMPOTENCE. Each statement keys on `code` and sets an
-- absolute value, so running it twice is the same as running it once,
-- and a row that is absent matches nothing and is not an error. There
-- is no application write path to this column (it is seeded by
-- migrations only), so an absolute set cannot clobber an operator edit.

-- +goose Up

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'Manage assets belonging to other users: metadata edit, soft-delete and restore. Scope it to a team via user_capability_grants.team_id or a team-scoped role and it covers that team and every descendant. Does NOT confer publication: changing an asset''s status needs the owner, system.admin, or the matching publication verb (assets.publish, assets.archive or assets.unarchive) (#930, #938).'
 WHERE code = 'assets.admin';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'Archive an asset: set status to `archived`, from either draft or active. Retiring content only removes reach, so this confers no power to publish: a holder of assets.archive alone cannot move any asset into active. Team-scopable like the others.'
 WHERE code = 'assets.archive';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'Use the batch metadata editor to change one field across many assets at once. Team-scope aware: a scoped grant reaches only assets in that team, and a team-less asset requires a global holding. Composes with (and never replaces) the ordinary per-asset mutation rule and the field''s own read and write capabilities.'
 WHERE code = 'assets.metadata.bulk_edit';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'Publish an asset: make it publicly reachable by setting status to `active`. Required for EVERY transition INTO active (from draft, and together with assets.unarchive from archived), and for the retraction that reverses it (active → draft). This is the disclosure lever: visibility requires status = ''active'' before an anonymous reader may see an asset. Scope it to a team via user_capability_grants.team_id or a team-scoped role and it covers that team and every descendant.'
 WHERE code = 'assets.publish';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'UNENFORCED: granting this confers nothing (#951). It names a `pending_review` status, and the live constraint on assets.status permits only draft, active and archived. Whether the review state gets built (#895/#896/#897) or this code is removed is decided in #951; until then it is a no-op and should not be granted in the belief that it delegates anything.'
 WHERE code = 'assets.review';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'UNENFORCED: granting this confers nothing (#951). It names a `pending_review` status, and the live constraint on assets.status permits only draft, active and archived. Whether the review state gets built (#895/#896/#897) or this code is removed is decided in #951; until then it is a no-op and should not be granted in the belief that it delegates anything.'
 WHERE code = 'assets.submit';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'Manage collections belonging to other users: edit, delete, membership and grants. Read by canMutateCollection since the package was written; never seeded until now, so until this migration it could not be granted at all (#930).'
 WHERE code = 'collections.admin';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'Marker written by the request-access workflow (#881) when an owner approves a request. Confers no read access on its own: per-asset unlocking is #912.'
 WHERE code = 'content.access.request';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'Manage posts belonging to other users: edit, delete, membership and grants. Held globally it is the instance moderator role; scoped to a team it covers that team and every descendant, but does NOT confer a change of the post''s visibility (#930). Read by canMutatePost since the package was written; never seeded until now, so until this migration it could not be granted at all.'
 WHERE code = 'posts.admin';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'Superpower: bypasses every capability check'
 WHERE code = 'system.admin';
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'Manage assets belonging to other users — metadata edit, soft-delete and restore. Scope it to a team via user_capability_grants.team_id or a team-scoped role and it covers that team and every descendant. Does NOT confer publication: changing an asset''s status needs the owner, system.admin, or the matching publication verb — assets.publish, assets.archive or assets.unarchive (#930, #938).'
 WHERE code = 'assets.admin';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'Archive an asset — set status to `archived`, from either draft or active. Retiring content only removes reach, so this confers no power to publish: a holder of assets.archive alone cannot move any asset into active. Team-scopable like the others.'
 WHERE code = 'assets.archive';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'Use the batch metadata editor to change one field across many assets at once. Team-scope aware: a scoped grant reaches only assets in that team, and a team-less asset requires a global holding. Composes with — and never replaces — the ordinary per-asset mutation rule and the field''s own read and write capabilities.'
 WHERE code = 'assets.metadata.bulk_edit';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'Publish an asset — make it publicly reachable by setting status to `active`. Required for EVERY transition INTO active (from draft, and together with assets.unarchive from archived), and for the retraction that reverses it (active → draft). This is the disclosure lever: visibility requires status = ''active'' before an anonymous reader may see an asset. Scope it to a team via user_capability_grants.team_id or a team-scoped role and it covers that team and every descendant.'
 WHERE code = 'assets.publish';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'UNENFORCED — granting this confers nothing (#951). It names a `pending_review` status, and the live constraint on assets.status permits only draft, active and archived. Whether the review state gets built (#895/#896/#897) or this code is removed is decided in #951; until then it is a no-op and should not be granted in the belief that it delegates anything.'
 WHERE code = 'assets.review';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'UNENFORCED — granting this confers nothing (#951). It names a `pending_review` status, and the live constraint on assets.status permits only draft, active and archived. Whether the review state gets built (#895/#896/#897) or this code is removed is decided in #951; until then it is a no-op and should not be granted in the belief that it delegates anything.'
 WHERE code = 'assets.submit';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'Manage collections belonging to other users — edit, delete, membership and grants. Read by canMutateCollection since the package was written; never seeded until now, so until this migration it could not be granted at all (#930).'
 WHERE code = 'collections.admin';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'Marker written by the request-access workflow (#881) when an owner approves a request. Confers no read access on its own — per-asset unlocking is #912.'
 WHERE code = 'content.access.request';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'Manage posts belonging to other users — edit, delete, membership and grants. Held globally it is the instance moderator role; scoped to a team it covers that team and every descendant, but does NOT confer a change of the post''s visibility (#930). Read by canMutatePost since the package was written; never seeded until now, so until this migration it could not be granted at all.'
 WHERE code = 'posts.admin';
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE public.capabilities
   SET description = 'Superpower — bypasses every capability check'
 WHERE code = 'system.admin';
-- +goose StatementEnd
