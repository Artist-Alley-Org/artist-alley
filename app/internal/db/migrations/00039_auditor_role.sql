-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00039_auditor_role.sql
--
-- Create the seeded read-only administrator role, and hand it the admin
-- READ capabilities that nothing could hold (#958).
--
-- ## The gap
--
-- 00003 seeded seven capability codes and granted none of them to
-- anything, and nothing in internal/seed/ touches role_capabilities
-- either. On a freshly migrated instance the roles table is exactly
-- three real rows — Base (10 caps), Admin (24, parent Base), Anonymous
-- (1) — plus the folded test_* fixtures. `Admin` holds `system.admin`,
-- which short-circuits every `Identity.Can` check, so it did not need
-- the seven and did not get them. There was no tier between Base and
-- Admin, so the codes were reachable by nobody.
--
-- They are consumed for real. `federation.read` alone gates five
-- federation admin packages (federation/{directory,peer,userkeys,p2p,
-- shares}/admin.go, each `capRead = "federation.read"`), and the admin
-- tile grid gates thirteen live tiles on individual read caps rather
-- than on `system.admin` (web/src/lib/admin/sections.ts). The admin
-- shell documents its own purpose as "a gate that stops a read-only
-- role seeing a bare 'no permission'" (web/src/routes/admin/
-- +layout.svelte). The UI was built for a tier the data could not
-- express.
--
-- ## The name
--
-- `Auditor` is the codebase's own noun for this user, not an invention:
--
--   internal/db/migrations/00003 — "no way to build a read-only auditor role"
--   internal/activities/admin.go — "the feed is viewable by a read-only auditor"
--   internal/requests/http.go    — "a read-only auditor (requests.read)"
--
-- and it is one capitalised word, matching Base / Admin / Anonymous.
-- Nothing in the tree hardcodes a role name other than "Admin"
-- (bootstrap.go, setup/handler.go), "Base" (the default-signup config
-- default) and "Anonymous" (auth/middleware.go), so there is no clash.
--
-- ## Why the parent is Base — worked out, not copied from Admin
--
-- Capability resolution walks roles.parent_id recursively
-- (auth/queries.sql.go, EffectiveCapabilitiesForUser), so the parent
-- decides what this role implicitly holds. Base carries ten codes:
--
--   caps.read  roles.read  teams.read            ← admin READ surfaces
--   profile.update_self                          ← any human account needs this
--   assets.submit posts.comment posts.like
--   comments.delete.own ai.use mcp.client.use    ← ordinary participant actions
--
-- The first four settle it. `roles.read` and `teams.read` are the caps
-- the /admin/roles and /admin/teams tiles gate on, and `caps.read` backs
-- the capability catalogue those pages read — they are admin read
-- surfaces that live in Base for historical reasons. A parentless
-- Auditor would be a read-only administrator that cannot open the roles
-- or groups page, which is incoherent. `profile.update_self` is what
-- lets the account change its own password; without it the role is not
-- usable by a person.
--
-- The remaining six are writes, but every one of them is an ordinary
-- participant action on the holder's OWN content — submit an asset,
-- comment, like, delete your own comment, call the AI/MCP client. None
-- of them touches an admin surface. "Read-only" here is a claim about
-- the ADMIN plane, and it holds: this role gets no admin write cap, and
-- `system.admin` is not in its chain, so every write handler still
-- refuses it.
--
-- Descending from Base is also the shape the one existing derived role
-- already has (Admin ← Base), so the hierarchy stays a single spine
-- rather than growing a second root.
--
-- ## Why `share.grant` is NOT granted — six codes, not seven
--
-- `share.grant` is the one non-read code 00003 seeded, and it does not
-- belong on a read-only role. Two independent reasons:
--
--  1. It is not needed for any read. `ListAdminRequests`
--     (requests/http.go) accepts `requests.read` OR `share.grant` OR
--     `system.admin`; the Auditor's `requests.read` already opens
--     /admin/requests. Adding `share.grant` widens nothing readable.
--
--  2. It is a privilege-granting WRITE. `DecideAdminRequest` lets a
--     `share.grant` holder "decide anything", and approving inserts a
--     `user_capability_grants` row whose capability_code is
--     `resource_request.requested_capability` — requester-controlled
--     input, FK-constrained to capabilities(code) but otherwise
--     unconstrained. 00035 spells the hazard out: "nothing stops a
--     request naming system.admin". A read-only role holding
--     `share.grant` could therefore be walked into granting
--     `system.admin` by anyone willing to file the request. That is an
--     escalation route out of the read-only tier, which is the one
--     property the tier exists to have.
--
-- 00003's own comment already reached this conclusion — it says seeding
-- share.grant "keeps request decisions out of reach of a read-only role
-- (which holds only *.read)". Granting it here would contradict the
-- migration that created it. It stays catalogue-only: a code an
-- operator hands to a named approver, never a tier default.
--
-- ## What this does NOT do
--
-- No capability changes what it gates. This migration only changes who
-- can hold what. It also does not assign the role to anybody — role
-- assignment is an operator action (admin/users/[ref]), and resolved
-- capability sets are cached per identity, so a live assignment needs
-- the usual cache invalidation. Applying this at boot is fine: the
-- migration runs before the server serves.

-- +goose Up

-- Fixed UUID rather than gen_random_uuid(), matching how the baseline
-- pins Base/Admin/Anonymous: the id is referenced by tests and is the
-- stable identity of this role across every instance, which matters
-- once roles carry origin_server_id across a federation.
INSERT INTO public.roles (id, parent_id, name, description)
VALUES (
    'c7a1f2e0-3b5d-4a6c-9e18-2f7b4d0a1c63',
    '80ec6003-7fd5-4dac-9415-d26d39169d42', -- Base
    'Auditor',
    'Read-only administrator; can open the admin read surfaces but cannot change anything'
)
ON CONFLICT (name) DO NOTHING;

-- Keyed on the role NAME, not the literal id, so the grants still land
-- if the row above lost the ON CONFLICT race with a hand-made role of
-- the same name.
INSERT INTO public.role_capabilities (role_id, capability_code)
SELECT r.id, c.code
FROM public.roles r
CROSS JOIN (VALUES
    ('featured.read'),
    ('federation.read'),
    ('requests.read'),
    ('system.activities.read'),
    ('system.license.read'),
    ('system.metadata_extraction.read')
) AS c(code)
WHERE r.name = 'Auditor'
ON CONFLICT (role_id, capability_code) DO NOTHING;

-- +goose Down

-- Drop the grants first, then the role. Deleting the role would cascade
-- them anyway (role_capabilities.role_id is ON DELETE CASCADE), but
-- doing it explicitly keeps the Down readable as "undo the two inserts
-- above".
--
-- Keyed on the fixed id, NOT the name, which is deliberately narrower
-- than the Up. If an operator had already hand-made a role called
-- `Auditor`, the Up's ON CONFLICT left their row alone and attached the
-- six grants to it; this Down then removes nothing, which is the right
-- answer — a rollback must not delete a role this migration did not
-- create. Re-running the Up re-attaches the grants harmlessly.
--
-- Deleting the role also cascades user_roles rows for anyone assigned
-- to it and fires acl_sweep_on_role_delete / asset_type_acl_sweep_on_
-- role_delete, dropping ACL entries that named it. That is correct for
-- a rollback — the role is going away, so grants of it must go too —
-- but it is not reversible by re-running Up, which recreates the role
-- with no members.
DELETE FROM public.role_capabilities
 WHERE role_id = 'c7a1f2e0-3b5d-4a6c-9e18-2f7b4d0a1c63';

DELETE FROM public.roles
 WHERE id = 'c7a1f2e0-3b5d-4a6c-9e18-2f7b4d0a1c63';
