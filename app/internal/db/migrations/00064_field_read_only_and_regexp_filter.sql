-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00064_field_read_only_and_regexp_filter.sql
--
-- Two field-configuration settings an operator could not reach, because
-- they did not exist (#1173, ADR 0012).
--
-- # What was missing
--
-- `field_definition` carried thirty columns and none of them said "a
-- person may not edit this value" or "a person's value has to look like
-- this". Every field was writable by any caller who held its
-- `write_capability`, and every value was accepted as long as it landed
-- in the right typed column. An operator with a `shot_code` field whose
-- values are `AAA_0010` had no way to say so, and one with a
-- `pipeline_id` written only by extraction had no way to stop a person
-- overwriting it.
--
-- # `read_only` is about PEOPLE, not about the row
--
-- This is the decision the column encodes, and it is the one that is
-- easy to get wrong. `read_only` refuses a HUMAN write. It does not
-- freeze the value: upload defaults, the extraction pipeline and the
-- mirror filler all keep writing, because a field an operator marks
-- read-only is usually a field they mean the SYSTEM to own.
--
-- The seam is the CALL SITE and it is not forgeable. The four handlers
-- that enforce it (SetAssetFieldValue, ClearAssetFieldValue,
-- SetCollectionFieldValue, ClearCollectionFieldValue) begin with
-- `auth.IdentityFromContext` and 401 without one. The writers that do
-- not enforce it — ApplyAssetDefaults, the extraction adapter,
-- mirrorFill — are distinct Go functions with no OpenAPI operation and
-- no route, unreachable over HTTP at all. There is deliberately no
-- caller-selectable "this write is a system write" flag: a boolean a
-- caller can set is a boolean a caller can lie about, and `set_by`
-- records provenance as evidence after the fact rather than deciding
-- anything.
--
-- Two consequences worth stating plainly. A stored value CAN fail a
-- rule a person would be held to, because the system wrote it — that is
-- accepted, and it is what "human input validation" means. And on the
-- ASSET side the refusal starts the moment the flag is set, even where
-- no value exists yet: `AssetCreate.metadata` is a free-form JSONB blob
-- on `assets` and creates no field-value rows, so there is no human
-- first-write seam to leave open. The COLLECTION side has one — the
-- create body seeds values inside the create transaction — so a
-- collection's initial human value is allowed and every later write is
-- refused.
--
-- # `regexp_filter` is a PATTERN, and NULL is the only "no constraint"
--
-- `text NULL`, no default, and the empty string is refused by the CHECK
-- below. This is `edit_tab`'s reasoning (00058) applied to a second
-- column: if `''` were storable, "this field has no pattern" would have
-- two representations and every reader would have to know both.
-- Clearing therefore travels as an explicit `clear_regexp_filter: true`
-- on the update body, exactly as `clear_edit_tab` does.
--
-- ⚠️ ONE DELIBERATE DIVERGENCE FROM `edit_tab`, and it is why this
-- CHECK is `<> ''` rather than `btrim(...) <> ''`. `edit_tab` trims
-- before its blank check because a tab named " " is a tab nobody can
-- navigate to. A PATTERN is not a label: leading and trailing
-- whitespace is meaningful inside one, and under the whole-value
-- semantics described below `\A(?:   )\z` legitimately matches exactly
-- three spaces. Only the GENUINELY EMPTY string is refused here; a
-- whitespace-only pattern is a valid configuration and is stored
-- verbatim. Trimming would silently corrupt it.
--
-- The pattern is Go's `regexp` (RE2), and the SERVER anchors it: the
-- value must match `\A(?:<pattern>)\z`. Operators are not asked to
-- write `^…$` themselves, because `^` and `$` are LINE anchors once a
-- pattern turns on `(?m)`, and because a top-level alternation would
-- otherwise bind the anchors to only its first and last branch. The
-- non-capturing group is what keeps `a|b` meaning "the whole value is
-- a, or the whole value is b". Nothing rewrites or trims what the
-- operator wrote; the wrapping happens at match time.
--
-- RE2 has no backtracking, so a pattern cannot be made to run for
-- exponential time. That is the reason a free-text regex is safe to
-- accept from an operator here at all.
--
-- # Which types honour a pattern lives in Go, not in a CHECK
--
-- `text` and `longtext` only, and the narrowing is one Go function
-- (`regexpFilterApplies`) rather than a constraint — the same choice
-- `open_vocabulary` made in 00028 and for the same reason: widening it
-- later should be a decision, not a migration. `rich_text` is excluded
-- even though it stores in `value_text`, because what lands in that
-- column is SANITISED HTML (`richtext.SanitizeValueText` runs before
-- every write), so a pattern would be matched against markup rather
-- than against anything the operator can see. `^[A-Z]` fails on every
-- rich-text value ever stored. The typed columns exclude the rest.
--
-- # The mirrored exclusion IS in a CHECK
--
-- `title` and `description` declare `mirrors_column` (00044) and are
-- therefore views onto columns of `assets`, which carry a SECOND,
-- parallel human write plane: `POST /assets` sets the title, `PATCH
-- /assets/{id}` mutates both. Enforcing either setting on the field
-- plane alone would let one plane create a state the other calls
-- invalid, which is exactly the divergence 00044 exists to prevent.
--
-- So the exclusion is a constraint rather than a Go rule, following
-- 00044's own reasoning: a path that has not learned the rule fails
-- loudly instead of quietly storing a setting half the writers obey.
-- The handler refuses first with a sentence, so an operator gets a 400
-- rather than a 500 — the same division of labour 00045 has between the
-- `show_on_card` gate and its CHECK.
--
-- # No index
--
-- Deliberate, for 00058's reason: nothing queries BY these columns.
-- Both are read from a field definition the caller already loaded by id.
--
-- # `regexp_filter` federates; `read_only` does not
--
-- ADR 0083's criterion leaves a property out of a schema envelope when
-- it "names something that exists only on the sender". A pattern
-- describes the FIELD — what a value of it looks like — so it travels,
-- like `required`. `read_only` describes who may write here, which is
-- the same class as `read_capability` / `write_capability`, and 0083
-- keeps those out because importing one silently widens or narrows
-- access on the receiver. Recorded in the ADR 0083 amendment; nothing
-- is implemented, since federation still carries no metadata at all.
--
-- Plain DDL, so no StatementBegin/End markers.

-- +goose Up

ALTER TABLE public.field_definition
    ADD COLUMN read_only boolean DEFAULT false NOT NULL;

ALTER TABLE public.field_definition
    ADD COLUMN regexp_filter text;

-- NULL is the one canonical "no constraint". The empty string would be
-- a second one, and it is refused rather than stored. NOT btrim() — a
-- whitespace-only pattern is meaningful; see the note above.
ALTER TABLE public.field_definition
    ADD CONSTRAINT field_definition_regexp_filter_nonempty_check
    CHECK (regexp_filter IS NULL OR regexp_filter <> '');

-- A mirrored field is a view onto a column of `assets` that has its own
-- human write plane. Neither setting may be configured on one, because
-- only one of the two planes would obey it.
ALTER TABLE public.field_definition
    ADD CONSTRAINT field_definition_mirrored_input_rules_check
    CHECK (mirrors_column IS NULL OR (read_only IS FALSE AND regexp_filter IS NULL));

COMMENT ON COLUMN public.field_definition.read_only IS
    'Refuse HUMAN writes to this field''s values (#1173, ADR 0012). FALSE by default, which is today''s behaviour. Enforced on the four identity-bearing value handlers: setting and clearing an asset value, and setting and clearing a collection value. It is NOT a freeze on the row — upload defaults, the extraction pipeline and the mirror filler still write, because a read-only field is normally one the SYSTEM owns; those writers are distinct Go functions with no HTTP route, so the exemption is a property of the call site rather than a flag any caller can send. On an ASSET the refusal applies immediately, including where no value exists yet, because asset creation writes no field-value rows and so has no human first-write seam. On a COLLECTION the create body MAY seed an initial value, and every later set or clear is refused. Refused on a field declaring mirrors_column (see the CHECK): those carry a second human write plane on the assets row that would not obey it. Does NOT federate — it is an access rule, the same class ADR 0083 keeps out of a schema envelope.';

COMMENT ON COLUMN public.field_definition.regexp_filter IS
    'Pattern a HUMAN-supplied value of this field must match (#1173, ADR 0012). NULL (the default) = no constraint, and NULL is the ONLY representation of that: the empty string is refused by the CHECK, and removing a pattern travels as an explicit clear_regexp_filter on the update body, exactly as clear_edit_tab does for 00058''s tab. Go RE2, anchored by the SERVER as \A(?:<pattern>)\z so it always matches the WHOLE value — operators do not write ^…$, which would be line anchors under (?m) and would bind to only the outer branches of an alternation. Stored verbatim: never trimmed, because whitespace inside a pattern is meaningful and a whitespace-only pattern is a legitimate configuration. Honoured for `text` and `longtext` only; the narrowing lives in Go (regexpFilterApplies) so widening it stays a decision rather than a migration. `rich_text` is excluded deliberately even though it shares value_text: that column holds sanitised HTML, so a pattern would match markup rather than anything the operator can see. Validates HUMAN INPUT, not the stored row — system writers (defaults, extraction, mirror fill) are not checked, so a stored value may legitimately fail it. Refused on a field declaring mirrors_column (see the CHECK). FEDERATES with the definition: it names the field, not the server.';

-- +goose Down

ALTER TABLE public.field_definition
    DROP CONSTRAINT IF EXISTS field_definition_mirrored_input_rules_check;
ALTER TABLE public.field_definition
    DROP CONSTRAINT IF EXISTS field_definition_regexp_filter_nonempty_check;
ALTER TABLE public.field_definition DROP COLUMN regexp_filter;
ALTER TABLE public.field_definition DROP COLUMN read_only;
