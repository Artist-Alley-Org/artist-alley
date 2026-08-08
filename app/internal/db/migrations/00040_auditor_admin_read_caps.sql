-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00040_auditor_admin_read_caps.sql
--
-- Hand the `Auditor` role the three admin READ capabilities whose own
-- seeding migrations name it as the intended holder (#961).
--
-- ## What 00039 left behind
--
-- 00039 created `Auditor` and granted it six codes — the ones 00003
-- seeded and nothing held. It did not touch the three read caps that
-- arrived later, in 00005 / 00006 / 00011, even though each of those
-- migrations names this exact role in its own header:
--
--   00005 — `system.jobs.read` "gates a read-only view of the queue
--           (jobs by status/type, active workers + lease, live status
--           counts) so an auditor role can watch the pipeline without
--           the `system.admin` wildcard."
--   00006 — `system.storage.read` "gates a read-only view of those
--           aggregates so an auditor role can answer 'what is using the
--           disk' without the `system.admin` wildcard."
--   00011 — of `system.audit.read`: "That capability exists so an
--           operator can create a read-only auditor role."
--
-- Each of the three deferred the grant with the same sentence —
-- "granting it to a role is a provisioning step, not schema". That was
-- true when the only roles were Base / Admin / Anonymous and there was
-- no tier to grant them to. 00039 built the tier; this is the
-- provisioning step, now that there is somewhere for it to land.
--
-- ## The UI was already built for this role
--
-- Eleven live admin tiles gate on these three codes
-- (web/src/lib/admin/sections.ts): six under `jobs`
-- (queue/workers/kinds/failed/schedules/live), four under `storage`
-- (usage/variants/orphans/checksums), and `audit` under `automation`.
-- Without this migration those eleven tiles are visible to `system.admin`
-- and to nobody else, which makes the per-tile `cap` on them decorative.
--
-- The storage sweep pages go further: StorageSweepPanel.svelte hides its
-- trigger control behind `auth.can('system.admin')` with the comment
-- "Reads gate on system.storage.read; triggering gates on system.admin
-- server-side, so the trigger control is hidden (not just disabled) for
-- read-cap holders". That component was written for a read-cap holder
-- who could not exist. This creates them.
--
-- ## Why these three are safe on a read-only tier
--
-- All three are reads, and each is the READ half of a split whose write
-- half stays on `system.admin`:
--
--   - jobs: requeue / cancel / concurrency edits are `system.admin`
--     (00005: "no write path is exposed under this cap").
--   - storage: orphan sweep, checksum re-verify and reimport are
--     `system.admin` (00006, and the panel above).
--   - audit: the log is read-only by construction, and 00011 split the
--     personal data out of it precisely so this grant would be safe.
--     `system.audit.pii.read` — the actor IP — is NOT granted here and
--     stays exactly where 00011 put it: attached to nothing, including
--     `system.admin`. An Auditor reads the log with the `ip` field
--     omitted (audit/handler.go resolves the PII cap separately).
--
-- The 00011 case is worth spelling out because it is the one with a
-- history: the public demo granted `system.audit.read` to a published
-- account and leaked visitor IPs. That leak is what motivated the split,
-- and the split is what makes this grant a read of "what happened"
-- rather than "who, from where".
--
-- ## What this does NOT do
--
-- No capability changes what it gates — this migration only changes who
-- can hold what. It does not assign the role to anybody, and it does not
-- touch the other three codes #958 found unreachable
-- (`system.asset_types.admin`, `users.approve`, `users.password.reset`).
-- Those are admin WRITES over other people's accounts and definitions;
-- they are recorded in capability_reachability_test.go as operator
-- hand-outs, not tier defaults, with the sources that say so.

-- +goose Up

-- Keyed on the role NAME, matching 00039: the grants still land if the
-- role row there lost its ON CONFLICT race with a hand-made role of the
-- same name.
INSERT INTO public.role_capabilities (role_id, capability_code)
SELECT r.id, c.code
FROM public.roles r
CROSS JOIN (VALUES
    ('system.audit.read'),
    ('system.jobs.read'),
    ('system.storage.read')
) AS c(code)
WHERE r.name = 'Auditor'
ON CONFLICT (role_id, capability_code) DO NOTHING;

-- +goose Down

-- Narrowed to the three codes AND the fixed id 00039 pins, so a
-- rollback removes exactly what the Up added:
--
--   - by code, because deleting every row for the role would also
--     remove 00039's six, which this migration does not own;
--   - by id rather than by name, because if an operator had hand-made a
--     role called `Auditor` the Up attached to their row and a rollback
--     must not strip capabilities off a role this migration set did not
--     create. That mirrors 00039's Down exactly.
DELETE FROM public.role_capabilities
 WHERE role_id = 'c7a1f2e0-3b5d-4a6c-9e18-2f7b4d0a1c63'
   AND capability_code IN (
       'system.audit.read',
       'system.jobs.read',
       'system.storage.read'
   );
