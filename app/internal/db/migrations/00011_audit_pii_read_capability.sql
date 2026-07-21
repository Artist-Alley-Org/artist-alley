-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00011_audit_pii_read_capability.sql
--
-- Separate the audit log from the personal data inside it (#425).
--
-- /admin/audit returned actor IP addresses to any holder of
-- system.audit.read. That capability exists so an operator can create a
-- read-only auditor role — but an IP identifies an individual and
-- approximates their location, so "may read the audit log" was silently
-- also granting "may read personal data about every user who has logged
-- in".
--
-- This was not theoretical. The public demo granted system.audit.read
-- to demo-viewer, and the demo credentials are published, so anyone on
-- the internet could read the IP of every other visitor who had signed
-- in. That was mitigated demo-side by removing the grant and wiping the
-- column, but the exposure is structural: any operator who creates a
-- read-only auditor role reproduces it.
--
--   system.audit.read      — the audit log WITHOUT ip
--   system.audit.pii.read  — additionally returns actor ip
--
-- WHY A DEDICATED CAPABILITY RATHER THAN GATING ON system.admin:
-- gating on system.admin would mean the only way to let somebody see
-- actor IPs is to make them a full administrator, which conflates "may
-- administer the system" with "may see personal data about users".
-- Those are different jobs in any organisation large enough to care —
-- a compliance officer or incident responder needs the second without
-- the first. It also matches how read access is already carved by area
-- (system.jobs.read, system.storage.read, system.audit.read).
--
-- Mirrors 00003 / 00005 / 00006: this migration only DEFINES the
-- capability. Granting it to a role is a provisioning step, not schema —
-- resolved capability sets are cached per identity, so a grant needs a
-- cache invalidation or restart anyway.
--
-- Deliberately NOT granted to anything here, including system.admin,
-- which does not need it: system.admin is a wildcard in Identity.Can
-- and satisfies every capability check without holding a row.
--
-- SCOPE NOTE: this gates the raw `ip` column only. It does not touch
-- the IP subnet HASH on lockout events (audit/events.go), which is a
-- deliberate privacy-preserving mechanism for grouping threat activity
-- and carries no recoverable address. Whether to retain raw IPs at all,
-- and whether to mask rather than omit them, is #426.

-- +goose Up
-- +goose StatementBegin
INSERT INTO public.capabilities (code, description) VALUES
    ('system.audit.pii.read',
     'Read personal data in audit events: actor IP addresses. Additive to system.audit.read, which returns the log without them')
ON CONFLICT (code) DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM public.capabilities WHERE code = 'system.audit.pii.read';
-- +goose StatementEnd
