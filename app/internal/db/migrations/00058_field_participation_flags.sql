-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00058_field_participation_flags.sql
--
-- A field can now SAY where it appears (ADR 0092 §3, #1173).
--
-- # The defect
--
-- Every surface that renders fields guesses which ones belong to it,
-- and each guesses differently:
--
--   * /search/advanced takes `searchable <> false`, `status <>
--     'archived'` and `type IN (select, multi_select, tree)`;
--   * the upload composer takes every active field for the asset type;
--   * the collection editor takes every active collection field.
--
-- Two of those three tests are about something else. `searchable` has
-- meant "this field's text feeds the search index" since the 00001
-- baseline — `rebuild_asset_search_text()` reads it, and it is the
-- reason a field's values are FINDABLE at all. `type` is what a value
-- looks like. Neither answers "does the operator want this field on
-- this page", so today nobody can answer it: an install with 200
-- fields gets 200 filters, and the person who knows which twelve
-- matter has no way to say so.
--
-- ADR 0092 §3 states the rule this migration makes expressible:
-- "surfaces read the flags; they do not infer participation from a
-- field's type or from whether it happens to have values."
--
-- # Why `searchable` is not reused
--
-- It would have been one fewer column, and it would have been wrong in
-- a way that is expensive to undo. "This field's values are indexed"
-- and "this field's control appears on the advanced form" are
-- independent answers: an operator with an internal `production_notes`
-- field wants it to keep answering text queries while nobody has to
-- scroll past it on the form, and conversely a field can be worth
-- filtering on without its raw text belonging in the free-text index.
-- Overloading one boolean with two questions makes the second one
-- unaskable — which is the failure ADR 0090 exists to warn about. The
-- boundary this migration must hold is therefore: turning
-- `show_in_advanced_search` off removes a CONTROL and changes no index,
-- no query result, and no `field:` filter a caller composes by hand.
--
-- # Why there is no `simple_search` flag
--
-- #1173's epic text names one, and the owner's scope ruling the same
-- day removed it: the simple search bar is untouched by this arc. A
-- column nothing may consult is a promise the product has not made, so
-- it is not added — the search surface named here is the advanced page
-- and nothing else. When a second search surface earns a flag, it gets
-- its own column with its own default, and the pair still reads as two
-- independent answers rather than a bitmask nobody can grep for.
--
-- # `status` already covers "retire without delete"
--
-- ADR 0092 §3's fourth clause — "whether it is visible at all (retired
-- without deletion)" — is ALREADY BUILT and gets no new column.
-- `status` is `active | deprecated | archived`;
-- `ArchiveFieldDefinition` sets `archived` while keeping the row and
-- every historic value; `ListFieldDefinitions` excludes archived rows
-- unless a caller asks for them explicitly; `DELETE /fields/{id}` is
-- that soft archive. Adding an `active` boolean beside it would be a
-- second statement of the same fact, and the two would disagree the
-- first time anyone wrote only one of them.
--
-- # THE DEFAULTS ARE TODAY'S BEHAVIOUR, AND THAT DECIDES THEM
--
-- Both booleans default TRUE, and the reason is a property rather than
-- a preference: an install that never touches these columns must
-- render exactly as it renders now. Every existing row is a field that
-- currently appears on the advanced page (if its type has a control)
-- and at upload, so the value that leaves those rows unchanged is
-- TRUE. Defaulting FALSE would empty both surfaces on the deploy that
-- ran this migration — a data-loss-shaped regression delivered as a
-- feature, on a mid-release version bump.
--
-- The consequence is that participation is OPT-OUT, and ADR 0092's
-- consequence section says so in the same words: "existing installs
-- are unaffected until an operator sets a flag: absent flags mean
-- today's behaviour." The 200-field install gets its short form by the
-- operator turning 188 fields off, which is the direction that cannot
-- surprise anyone.
--
-- `edit_tab` is NULL for the same reason: today no surface has tabs,
-- and NULL is the value that says "this field has not been assigned
-- one", distinct from a field an operator deliberately put in a tab
-- named "". The CHECK below refuses that empty string outright so the
-- distinction cannot be blurred by a form posting a blank input.
--
-- # No CHECK against `required`
--
-- `show_on_upload = false` on a `required` field would be a trap if
-- required-ness were enforced at asset creation — the operator would
-- hide a field the uploader must fill. It is not: `required` is
-- enforced only on the value-write path (SetAssetFieldValue refuses an
-- empty value, ClearAssetFieldValue refuses a clear). Asset creation
-- demands nothing, so the combination is merely "this field is not
-- offered at upload and cannot be blanked afterwards", which is
-- coherent. If required-at-save lands in the edit-parity work (#1119),
-- THAT is where the pair becomes a contradiction and where the
-- constraint belongs — stated here so it is not re-derived from
-- scratch.
--
-- # No CHECK against `read_capability` either
--
-- 00045 refuses `show_on_card` on a gated field because a card renders
-- on browse, where the server has evaluated no per-field capability —
-- the setting would be a leak or would be silently stripped. Neither
-- applies here. The advanced page's field list is already filtered by
-- the caller's capabilities on the way out (client-side display) and
-- `facet.Selection.Authorize` refuses a `field:` term naming an
-- unreadable field regardless of what any client sends (server-side,
-- load-bearing). So a gated field marked for the advanced page is
-- offered to the callers who may read it and to nobody else, which is
-- exactly the behaviour an operator asking for it wants. The gate
-- composes ON TOP of participation; it is not in tension with it.
--
-- # They FEDERATE
--
-- ADR 0083's exclusion criterion leaves a property out of a schema
-- envelope when it "names something that exists only on the sender".
-- All three name the FIELD — where it belongs, which tab it sits in —
-- not this server's schema or its storage, so they travel with the
-- definition exactly as `show_on_card` does (ADR 0012 amendment
-- 2026-08-10). Contrast `mirrors_column` (00044), which names a column
-- of the sender's own `assets` table and therefore stays out.
--
-- # No index
--
-- Deliberate. Nothing queries BY these columns: `GET /fields` returns
-- the catalogue ordered by group and the surfaces filter the rows they
-- were already given. 00045 earned its partial index because the card
-- path asks the database for "the fields marked at-a-glance" and never
-- for one field by id; there is no such query here, and an index for a
-- query that does not exist is a write cost with no reader. When
-- sprint 6 pushes the participation filter server-side, the index
-- arrives with the query that needs it.
--
-- Plain DDL, so no StatementBegin/End markers.

-- +goose Up

ALTER TABLE public.field_definition
    ADD COLUMN show_in_advanced_search boolean DEFAULT true NOT NULL;

ALTER TABLE public.field_definition
    ADD COLUMN show_on_upload boolean DEFAULT true NOT NULL;

ALTER TABLE public.field_definition
    ADD COLUMN edit_tab text;

-- A tab named "" is neither "unassigned" nor a tab anyone can navigate
-- to. NULL is the only way to say "no tab", so the empty string — and
-- any string that is only whitespace — is refused rather than stored
-- as a third, ambiguous state.
ALTER TABLE public.field_definition
    ADD CONSTRAINT field_definition_edit_tab_nonblank_check
    CHECK (edit_tab IS NULL OR btrim(edit_tab) <> '');

COMMENT ON COLUMN public.field_definition.show_in_advanced_search IS
    'Participation flag (ADR 0092 §3, #1173): offer this field as a filter control on the advanced search page. TRUE by default, because every field appeared there before this column existed and an install that never sets it must render unchanged. It governs the CONTROL only — it does not touch `searchable` (which decides whether the field''s text feeds the search index), does not change any query result, and does not stop a caller composing `filter=field:<code>=<value>` by hand. The read capability still gates on top: a flag can never offer a field the caller may not read. FEDERATES with the definition: it names the field, not the server (ADR 0083 exclusion criterion).';

COMMENT ON COLUMN public.field_definition.show_on_upload IS
    'Participation flag (ADR 0092 §3, #1173): offer this field on the upload / create surface. TRUE by default for the same reason as show_in_advanced_search — the upload composer rendered every active field for the asset type before this column existed. Consumed by the create/edit work (#1119); this column is the declaration, the surfaces obey it there. Not constrained against `required`, because required-ness is enforced on the value-write path and not at asset creation. FEDERATES with the definition.';

COMMENT ON COLUMN public.field_definition.edit_tab IS
    'Participation flag (ADR 0092 §3, #1173): which tab of the edit surface this field sits in. NULL (the default) = unassigned, which is today''s behaviour — no surface has tabs yet, and fields group by display_group. Distinct from an empty string, which the CHECK constraint refuses so that "no tab" has exactly one representation. A coarser grouping than display_group, not a replacement for it: a tab holds groups. FEDERATES with the definition.';

-- +goose Down

ALTER TABLE public.field_definition
    DROP CONSTRAINT IF EXISTS field_definition_edit_tab_nonblank_check;
ALTER TABLE public.field_definition DROP COLUMN edit_tab;
ALTER TABLE public.field_definition DROP COLUMN show_on_upload;
ALTER TABLE public.field_definition DROP COLUMN show_in_advanced_search;
