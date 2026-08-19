-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00057_vocabulary_capabilities.sql
--
-- Two capabilities for the two things an operator can do TO a
-- vocabulary, as opposed to WITH it (ADR 0092 §2 and §4, #789).
--
--   fields.vocabulary.extend — a value naming a term the field does
--                              not have CREATES it.
--   fields.vocabulary.merge  — one term is folded into another,
--                              rewriting stored values and leaving a
--                              permanent tombstone behind.
--
-- ## Why extend is not fields.admin
--
-- `fields.admin` is authority over the SCHEMA: create a field, retype
-- it, curate its option list, archive it. Extending an open vocabulary
-- is none of that — it is an ordinary artist typing "Canon EOS R5"
-- into a camera field and saving. #789 opens with exactly that case
-- and calls artist friction the wedge: requiring schema authority to
-- add a keyword is the round-trip the whole feature exists to remove.
--
-- ADR 0092 §2 states the requirement as a dial rather than a policy —
-- "an instance can let everyone extend a vocabulary, or restrict
-- extension to librarians while everyone else picks from what exists."
-- A dial needs a default, and the default here is EVERYONE, because
-- that is what the code did before this migration: minting has been
-- ungated since #830/#846, so seeding this to Admin alone would ship a
-- silent regression to every artist on every install under the banner
-- of adding a control. Operators who want the librarian model revoke
-- it from Base, which is one row.
--
-- Granted to `Base` (the role every fresh signup receives — see
-- sysconfig/auth.go's default_role, "Base") and to `Admin`, which does
-- not inherit Base's grants and therefore needs its own row. Anonymous
-- is deliberately absent: an anonymous caller cannot write a field
-- value at all, so granting it would describe an unreachable state.
--
-- ## Why merge is Admin-only
--
-- A merge REWRITES history: every asset and collection holding the
-- source term is updated to hold the target instead. That is the one
-- vocabulary operation that changes records their owners did not
-- touch, and #789's recorded research is unanimous that every
-- production system puts deliberate friction in front of it. The
-- capability is the first half of that friction; the required `reason`
-- and the dry-run preview on the endpoint are the second.
--
-- It is NOT granted to Base, and it is deliberately a separate code
-- from `fields.admin` rather than riding on it: curating an option
-- list writes only the field definition, while a merge writes
-- `asset_field_value` and `collection_field_value`. An operator may
-- reasonably delegate the first without the second.
--
-- ## No DDL
--
-- Aliases and tombstones both live inside `field_definition.options`,
-- the jsonb document ADR 0012 already defines, so this migration adds
-- no columns and `app/schema.sql` is unchanged. An alias is an extra
-- match key on an option; a tombstone is the `status`/`replaced_by`
-- pair the options lifecycle has carried since #737. Neither needed a
-- table, and inventing one would have put the vocabulary in two places.

-- +goose Up

INSERT INTO public.capabilities (code, description, created_at, required_license_feature)
VALUES (
    'fields.vocabulary.extend',
    'Create new terms in an open (extensible) field vocabulary by using them in a value. Does not confer any authority over the field definition itself.',
    now(),
    NULL
), (
    'fields.vocabulary.merge',
    'Fold one vocabulary term into another: rewrites every stored value naming the source term and leaves a permanent tombstone pointing at the target.',
    now(),
    NULL
);

-- Base: every signed-in user, preserving the pre-00057 behaviour where
-- minting was ungated. Admin: does not inherit Base, needs its own row.
INSERT INTO public.role_capabilities (role_id, capability_code)
VALUES
    ('80ec6003-7fd5-4dac-9415-d26d39169d42', 'fields.vocabulary.extend'),
    ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'fields.vocabulary.extend'),
    ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'fields.vocabulary.merge');

-- +goose Down

DELETE FROM public.role_capabilities
 WHERE capability_code IN ('fields.vocabulary.extend', 'fields.vocabulary.merge');

DELETE FROM public.user_capability_grants
 WHERE capability_code IN ('fields.vocabulary.extend', 'fields.vocabulary.merge');

DELETE FROM public.user_capability_revokes
 WHERE capability_code IN ('fields.vocabulary.extend', 'fields.vocabulary.merge');

DELETE FROM public.capabilities
 WHERE code IN ('fields.vocabulary.extend', 'fields.vocabulary.merge');
