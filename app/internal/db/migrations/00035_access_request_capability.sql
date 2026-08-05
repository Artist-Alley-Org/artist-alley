-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00035_access_request_capability.sql
--
-- Give the "request access" affordance (#881) a capability it is SAFE
-- for a non-operator to grant, and make a second pending ask for the
-- same thing impossible at the storage layer.
--
-- ## Why a new capability code
--
-- #881 puts a "Request access" button on the restricted placeholder and
-- lets the ASSET'S OWNER decide the request, without holding
-- `share.grant`. Granting inserts a `user_capability_grants` row whose
-- `capability_code` is whatever the requester named — and
-- `resource_request.requested_capability` is requester-controlled input,
-- FK-constrained to `capabilities(code)` since 00009 but otherwise
-- unconstrained. ADR 0064 already names the hazard: "nothing stops a
-- request naming system.admin … the only thing between that and the
-- file is an administrator clicking grant on a free-text value the
-- requester chose."
--
-- Widening the decide gate to asset owners turns that from a hazard
-- into an escalation route: any artist could be talked into granting
-- `system.admin` from a friendly-looking panel on their own work. So
-- the owner disjunct in requests/http.go decides ONLY requests naming
-- this code, and this code is the one the UI submits.
--
-- ## What it confers: NOTHING
--
-- Deliberately. `visibility.ContentReadable` consults exactly two codes
-- (`system.admin`, `content.read.all`) and no gate anywhere reads this
-- one. A granted request therefore means "the owner agreed", not "and
-- now you can see it" — per-asset unlocking is #912, and ADR 0064's
-- "Why the grant path is deferred" is still in force. The UI says so in
-- as many words; a granted request that silently changed nothing would
-- be worse than no button.
--
-- Not seeded onto any role. Nobody needs to HOLD it — it is a marker
-- the request workflow writes, not a permission anyone exercises.
--
-- ## Why the partial unique index
--
-- Submit coalesces a repeat ask onto the caller's existing pending row
-- rather than filing a second one (see requests/handler.go). A SELECT-
-- then-INSERT loses that race under concurrency, and the loser would
-- put a duplicate in the approver's queue. The index makes the database
-- the arbiter: the losing INSERT gets 23505 and the handler re-reads
-- the winner. Partial on `state = 'pending'` because a DECIDED request
-- must not block a fresh one — denial is terminal for the ROW, not for
-- the person (state.go: "admin tooling re-issues with a new
-- resource_request row rather than walking a row backwards").

-- +goose Up

INSERT INTO public.capabilities (code, description, created_at, required_license_feature)
VALUES (
    'content.access.request',
    'Marker written by the request-access workflow (#881) when an owner approves a request. Confers no read access on its own — per-asset unlocking is #912.',
    now(),
    NULL
);

CREATE UNIQUE INDEX resource_request_one_pending_per_ask
    ON public.resource_request (requester_user_ref, target_asset_id, requested_capability)
    WHERE (state = 'pending');

-- +goose Down

DROP INDEX IF EXISTS public.resource_request_one_pending_per_ask;

-- The FK from user_capability_grants.capability_code is ON DELETE
-- RESTRICT, so drop any marker grants this workflow wrote before
-- removing the code itself. They confer nothing, so dropping them
-- changes no caller's authority.
DELETE FROM public.user_capability_grants WHERE capability_code = 'content.access.request';

DELETE FROM public.capabilities WHERE code = 'content.access.request';
