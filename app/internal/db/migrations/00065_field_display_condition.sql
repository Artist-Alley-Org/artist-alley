-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00065_field_display_condition.sql
--
-- When a field should be OFFERED at all (#1173, #1119, ADR 0099).
--
-- # What was missing
--
-- Thirty-two columns said what a field is, what a value of it may be, and
-- where it sits on a form. None of them said WHEN the field should appear.
-- An operator whose `commission_deadline` only means anything on a
-- `work_type` of `Commission` could show it to everybody always, or not
-- create the field. 00058's `edit_tab` covered the coarser half of the same
-- problem and shipped with ZERO consumers; this column is the other half,
-- and both get their first reader in the same sprint.
--
-- # It is a FORM HINT, not an access rule
--
-- This is the decision the column encodes and the one that is easy to get
-- backwards. `display_condition` decides whether a CONTROL is drawn. It
-- decides nothing about access, filtering, indexing or write validity: a
-- hidden field keeps its values, keeps its read_capability and
-- write_capability, and can still be written through
-- PUT /assets/{id}/fields/{field_id} exactly as before. It sits with
-- display_order, display_group, show_on_card, show_in_advanced_search,
-- show_on_upload and edit_tab, and a client that ignores it entirely is
-- still correct, merely plainer.
--
-- Recorded loudly because the opposite reading is the one reached for
-- first, and a composition rule mistaken for an access rule is a security
-- hole with an innocent-looking configuration screen in front of it.
--
-- # The shape, and why the CHECK is this specific
--
-- A JSON ARRAY OF BARE `<code><op><value>` STRINGS, with NO `field:`
-- prefix stored, combined with AND. The terms use the EXISTING search term
-- grammar (facet.SplitFieldTerm) and THE SEARCH GRAMMAR IS NOT EXTENDED.
--
-- SQL NULL is the canonical unset and the CHECK makes it the ONLY one:
-- `[]`, `{}`, `""` and JSON `null` are all refused. This is 00058's
-- reasoning (`edit_tab` refuses the blank string) and 00064's
-- (`regexp_filter` refuses it too) applied to a third column. If an empty
-- array were storable, "this field has no condition" would have two
-- representations and every reader would have to know both, including the
-- evaluator, where a second spelling of "unset" is a second code path that
-- can disagree about whether a field is visible.
--
-- The predicate is written with `jsonb_path_exists` rather than an
-- EXISTS subquery over `jsonb_array_elements`, because a CHECK constraint
-- may not contain a subquery at all. It is also written inline rather than
-- through an IMMUTABLE helper function so that the constraint has no
-- dependency of its own to be dumped and restored in the right order.
--   * `$[*] ? (@.type() <> "string")`  matches any non-string element,
--     which covers numbers, booleans, objects, arrays and JSON null.
--   * `$[*] ? (@ like_regex "^\\s*$")` matches an empty or whitespace-only
--     string. Whitespace-only is refused here and NOT stored verbatim,
--     which is the opposite of `regexp_filter`'s deliberate divergence in
--     00064: a pattern's leading whitespace is meaningful, whereas a term
--     made entirely of spaces cannot parse into a code and an operator at
--     all. The Go validator refuses it first with a sentence, so an
--     operator gets a 400 rather than a 500, which is the same division of
--     labour 00045 and 00064 already have.
--
-- # No index, no default, no backfill
--
-- Deliberate, for 00058's and 00064's reason: nothing queries BY this
-- column. It is read from a definition the caller already loaded, and the
-- one place that scans it (the cycle walk in UpdateField) reads the whole
-- graph for a subject kind, which is a sequential scan of a table with
-- tens of rows on the largest installs we have seen. Every existing
-- definition reads NULL, so every existing form composes exactly as it did.
--
-- # WHY THE CYCLE INVARIANT NEEDS MORE THAN THIS FILE
--
-- Worth stating in the migration because the column is where somebody will
-- come looking. A condition names OTHER definitions, so the set of
-- conditions on one subject kind is a directed GRAPH, and the invariant
-- "that graph is acyclic" is not a property of any single row. No CHECK
-- constraint, no UNIQUE index and no trigger on this column can express
-- it: `A -> B` and `B -> A` are each individually valid rows.
--
-- So the invariant lives in the handler, and it is only an invariant
-- because the handler takes a TRANSACTION-SCOPED ADVISORY LOCK keyed on
-- the subject kind before it reads the graph and holds it until the write
-- commits (ADR 0099 §8). Without that lock, two operators writing `A -> B`
-- and `B -> A` each validate against a graph in which the other's edge is
-- not yet visible; both pass, both commit, and the graph now has a cycle
-- no single write could have created.
--
-- # Federation
--
-- Classified IN under ADR 0083 (amendment of this date): it names the
-- FIELD, not the server, which is the same test `required`,
-- `show_on_card` and `regexp_filter` pass. It is the first property in
-- that list to REFERENCE A SECOND FIELD, which is why that amendment adds
-- a reference sub-rule: terms hold federation-stable field CODES, a
-- missing referent is preserved verbatim and fails open at runtime, and
-- cycle participation plus applicability contribution are DEFERRED for an
-- unresolved edge. Nothing is implemented; federation still transports no
-- metadata at all.
--
-- Plain DDL, so no StatementBegin/End markers.

-- +goose Up

ALTER TABLE public.field_definition
    ADD COLUMN display_condition jsonb;

-- NULL is the one canonical "no condition". A non-empty array of
-- non-empty strings is the only other accepted shape; [], {}, "" and JSON
-- null are all refused rather than stored as a second spelling of unset.
-- No subquery: a CHECK may not contain one, so the element predicates are
-- jsonpath.
ALTER TABLE public.field_definition
    ADD CONSTRAINT field_definition_display_condition_shape_check
    CHECK (
        display_condition IS NULL
        OR (
                jsonb_typeof(display_condition) = 'array'
            AND jsonb_array_length(display_condition) > 0
            AND NOT jsonb_path_exists(display_condition, '$[*] ? (@.type() <> "string")')
            AND NOT jsonb_path_exists(display_condition, '$[*] ? (@ like_regex "^\\s*$")')
        )
    );

COMMENT ON COLUMN public.field_definition.display_condition IS
    'When this field should be OFFERED on a composition surface (#1173, #1119, ADR 0099). NULL (the default) = always, and NULL is the ONLY representation of that: the CHECK refuses [], {}, "" and JSON null, so no reader has to know a second spelling of unset. Otherwise a JSON array of bare <code><op><value> strings with NO field: prefix, combined with AND, parsed by the EXISTING search term grammar (facet.SplitFieldTerm) — the search grammar is NOT extended. A FORM HINT and never authorization: it decides whether a CONTROL is drawn and nothing about access, filtering, indexing or write validity, so a hidden field keeps its values and can still be written through PUT /assets/{id}/fields/{field_id}. Hiding a field emits no Set, no Clear and no empty row, and revealing it restores the persisted value byte for byte. Update-only on the API (display_condition + clear_display_condition on FieldDefinitionUpdate, neither on FieldDefinitionCreate), because a create body cannot reference a graph that does not exist yet. At runtime the condition is CONJUNCTIVE, and if ANY term is unevaluable — controller definition missing or unresolvable, or unreadable by this caller on this subject — the WHOLE condition fails open and the dependent is SHOWN; a readable controller with genuinely no value is a real FALSE and still hides. Configuration refuses malformed terms, unsupported operator/type pairings, unknown or mirrored or already-archived controllers, mirrored dependents, subject-kind mismatches, cycles walked across the WHOLE subject-kind graph, an empty N-way applies_to intersection, and distinct = literals on one single-valued controller. Archiving a controller later does NOT rewrite or clear a stored condition; the dependent fails open and ordinary evaluation resumes if the controller is restored. The acyclicity invariant is NOT expressible as a constraint on this column and is held by a transaction-scoped advisory lock in UpdateField. FEDERATES with the definition (ADR 0083, amendment 2026-09-03): it names the field rather than the server, and it is the first such property to reference a SECOND field, so a missing referent is preserved verbatim and its cycle and applicability checks are deferred.';

-- +goose Down

ALTER TABLE public.field_definition
    DROP CONSTRAINT IF EXISTS field_definition_display_condition_shape_check;
ALTER TABLE public.field_definition DROP COLUMN display_condition;
