-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00018_users_pii_read_capability.sql
--
-- Same data class, same bar (#573).
--
-- 00011 split audit actor IPs out of system.audit.read into a dedicated
-- system.audit.pii.read, on the reasoning that a raw IP identifies an
-- individual and approximates their location, so "may read this admin
-- surface" must not silently also mean "may read personal data about
-- every user on it".
--
-- GET /admin/users/{ref}/sessions returned exactly the same data class —
-- a user's raw client IP — behind users.read alone. #567 removed IPs
-- from the self-service /account/sessions view entirely, but left the
-- admin view untouched and flagged the asymmetry: one raw-IP surface
-- gated by a dedicated PII capability, another riding on the ordinary
-- read capability for its area. Two bars for one data class is how the
-- next IP-bearing surface ends up inventing a third.
--
--   users.read      — the session list WITHOUT ip
--   users.pii.read  — additionally returns the session ip
--
-- WHY RAISE SESSIONS RATHER THAN LOWER AUDIT: an operator who wants a
-- read-only user-support role (see who is logged in, revoke a stale
-- device) has no need for the addresses, and it is the strictly safer
-- direction — a grant an operator forgets to make withholds data, where
-- the reverse leaks it. It also keeps the deliberate control 00011
-- established rather than weakening it to match the looser surface.
--
-- WHY A SECOND PII CAPABILITY RATHER THAN REUSING system.audit.pii.read:
-- read access here is carved by AREA (users.read, system.audit.read,
-- system.jobs.read, system.storage.read), and the PII capability is
-- additive to the area capability it extends. Granting audit's PII
-- capability to get session IPs would mean handing out a capability
-- naming a surface the holder may not even be admitted to. The rule the
-- next surface should follow is `<area>.pii.read`, additive to
-- `<area>.read` — see ADR 0072.
--
-- Mirrors 00003 / 00005 / 00006 / 00011: this migration only DEFINES the
-- capability. Granting it to a role is a provisioning step, not schema —
-- resolved capability sets are cached per identity, so a grant needs a
-- cache invalidation or restart anyway.
--
-- Deliberately NOT granted to anything here, including system.admin,
-- which does not need it: system.admin is a wildcard in Identity.Can and
-- satisfies every capability check without holding a row. An existing
-- users.read holder therefore stops seeing session IPs the moment this
-- lands, which is the point — pre-v0.1.0, there are no operators to
-- carry through a grace period for (see the pre-release posture in
-- CONTRIBUTING).
--
-- SCOPE NOTE: this gates the raw `ip` on SessionRow only. The session's
-- user-agent stays on both views — it is what labels a device, carries
-- no address, and removing it would make the revoke UI unusable.

-- +goose Up
-- +goose StatementBegin
INSERT INTO public.capabilities (code, description) VALUES
    ('users.pii.read',
     'Read personal data on user admin surfaces: session IP addresses. Additive to users.read, which returns sessions without them')
ON CONFLICT (code) DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM public.capabilities WHERE code = 'users.pii.read';
-- +goose StatementEnd
