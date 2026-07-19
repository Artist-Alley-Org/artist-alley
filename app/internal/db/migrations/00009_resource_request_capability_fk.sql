-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00009_resource_request_capability_fk.sql
--
-- Constrain resource_request.requested_capability to the real
-- capability vocabulary (#434).
--
-- The column is requester-controlled free text used in an
-- authorisation flow. `state` two lines below it in the baseline has a
-- CHECK; the security-relevant column had nothing, so any account
-- could submit a request naming any string at all.
--
-- ADR 0064 deferred the grant path precisely because of this: if the
-- content checker ever honours a granted capability, the requester
-- picks the value being matched, and the only thing between that and
-- the file is an admin approving a string the requester chose. Making
-- the field referentially honest is what unblocks revisiting that.
--
-- A foreign key rather than a CHECK or an enum on purpose: public
-- capabilities IS the registry, and every cap-seed migration inserts
-- into it (00003, 00005, 00006). A hand-maintained list would be a
-- second vocabulary that silently drifts every time a migration adds a
-- capability; the FK cannot drift.
--
-- ON DELETE RESTRICT: a capability with outstanding requests must not
-- vanish silently. CASCADE would delete the audit trail of who asked
-- for what as a side effect of tidying the registry, and these rows
-- are the record of an access decision. Deleting a capability that
-- still has requests should be a deliberate act that fails loudly and
-- makes the operator resolve them first.
--
-- Applied while resource_request is empty (verified), so no backfill
-- or cleanup step is needed.
--
-- WHAT THIS DOES NOT FIX: an FK narrows the field from "any string the
-- attacker chooses" to "any valid capability code the attacker
-- chooses". Nothing here stops a request naming system.admin. WHICH
-- capabilities are legitimately requestable is undecided and belongs
-- to the grant path (ADR 0064) — it must be settled before any grant
-- is allowed to unlock content. Do not add an allowlist, a
-- `requestable` flag, or prefix filtering here on the assumption that
-- this migration made the field safe to trust.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE public.resource_request
    ADD CONSTRAINT resource_request_requested_capability_fkey
    FOREIGN KEY (requested_capability)
    REFERENCES public.capabilities (code)
    ON DELETE RESTRICT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE public.resource_request
    DROP CONSTRAINT IF EXISTS resource_request_requested_capability_fkey;
-- +goose StatementEnd
