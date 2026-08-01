-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00021_upload_defaults.sql
--
-- Upload defaults (#793), ADR 0081 §3 as amended 2026-07-31.
--
-- Three changes, and the third is the load-bearing one.
--
-- 1. field_definition.default_value — a declarative default, applied
--    when an asset is created. Exactly two shapes, both validated on
--    write in Go (metadata/defaults.go):
--
--      {"kind":"literal","value_text":"greybox"}
--      {"kind":"context","context":"uploading_user"}
--
--    There is deliberately no macro column and no expression language.
--    The prior art stores executable PHP in `autocomplete_macro` /
--    `onchange_macro`; that cannot be validated on write, it is a
--    code-injection surface, and it makes the field definition
--    unportable across a federation boundary. A literal or a name from
--    a closed set has none of those properties.
--
--    jsonb rather than five typed default_* columns because the value
--    is polymorphic in the same way asset_field_value is, and five more
--    nullable columns would be a fourth place to get the
--    type→column mapping wrong (#778, #791). The Go writer runs the
--    default through the SAME buildUpsertParams the manual write path
--    uses, so the column a default lands in cannot diverge from the
--    column a manual value lands in — see metadata/valuecolumn_test.go.
--
-- 2. field_default_override — a team may override a field's default for
--    its own uploads. Team-scoped, and deliberately NOT federated:
--    defaults travel with their field because they are part of the
--    field definition; teams are local, so their overrides are too.
--    Real FKs with ON DELETE CASCADE, because a no-FK table outlives a
--    TRUNCATE ... CASCADE and that is exactly how orphan rows have got
--    in before (#566/#569).
--
-- 3. asset_field_value.set_by gains 'default'.
--
--    THIS IS THE ONE THAT MAKES THE PRECEDENCE CHAIN POSSIBLE. ADR 0081
--    §3 specifies `extracted > team default > field default > empty`,
--    and "a default never overwrites an extracted one". The applier
--    (asset/metadata/apply.go) implements skip_if_set as a check on
--    PRESENCE, not on provenance:
--
--        if fc.Mode == ExtractionModeSkipIfSet && present { skip }
--
--    Write a default at asset creation and the value is present, so
--    extraction skips it — the default beats extraction, which is the
--    exact inverse of what the ADR specifies. Thirteen of the fifteen
--    live field definitions are skip_if_set, so that inversion would
--    have been the normal case rather than the corner.
--
--    Adding a provenance value the applier can recognise is what lets
--    the skip distinguish "a human typed this" from "a default is
--    sitting here waiting to be improved on". The alternative — apply
--    defaults AFTER extraction — was rejected: extraction is an async
--    job enqueued post-commit and only for six image extensions, so a
--    default on a .glb or a .pdf would never be applied at all, and the
--    upload modal could not truthfully show what an asset is about to
--    carry. The whole point of a default is that it is there when
--    nothing else says otherwise, which includes "nothing else ever
--    runs".
--
-- asset_field_value_history.set_by carries no CHECK constraint, so it
-- needs no change; collection_field_value is untouched because defaults
-- are an asset-creation feature and collections are not created by
-- upload.
--
-- Plain DDL, so no StatementBegin/End markers — those exist for plpgsql
-- bodies whose semicolons goose would otherwise split on.

-- +goose Up

ALTER TABLE public.field_definition
    ADD COLUMN default_value jsonb;

COMMENT ON COLUMN public.field_definition.default_value IS
    'Declarative default applied at asset creation. Either {"kind":"literal", value_*: …} or {"kind":"context","context":…} naming a member of a closed server-resolved set. Never an expression. NULL = no default. Validated on write against the field''s type and, for vocabulary types, against the live options document — a default naming a deprecated or archived option is rejected. Federates with the field definition.';

CREATE TABLE public.field_default_override (
    field_id uuid NOT NULL,
    team_id uuid NOT NULL,
    default_value jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_by_user_ref bigint
);

COMMENT ON TABLE public.field_default_override IS
    'Per-team override of field_definition.default_value, applied to that team''s uploads. Same document shape and same validation as the field default. Does NOT federate: a field definition travels to a peer, a team does not.';

ALTER TABLE ONLY public.field_default_override
    ADD CONSTRAINT field_default_override_pkey PRIMARY KEY (field_id, team_id);

ALTER TABLE ONLY public.field_default_override
    ADD CONSTRAINT field_default_override_field_id_fkey
    FOREIGN KEY (field_id) REFERENCES public.field_definition(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.field_default_override
    ADD CONSTRAINT field_default_override_team_id_fkey
    FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;

-- The apply path looks up (field, team) by the PK. This index serves
-- the admin direction — "what does this team override" — and the
-- cascade on team delete.
CREATE INDEX field_default_override_team_idx
    ON public.field_default_override USING btree (team_id);

ALTER TABLE public.asset_field_value
    DROP CONSTRAINT asset_field_value_set_by_check;

ALTER TABLE public.asset_field_value
    ADD CONSTRAINT asset_field_value_set_by_check
    CHECK ((set_by = ANY (ARRAY[
        'manual'::text, 'exif'::text, 'iptc'::text, 'xmp'::text,
        'api'::text, 'import'::text, 'computed'::text, 'default'::text
    ])));

-- +goose Down

ALTER TABLE public.asset_field_value
    DROP CONSTRAINT asset_field_value_set_by_check;

-- Rows written by the defaults path have no home in the old vocabulary.
-- They are deleted rather than relabelled: calling a default 'manual'
-- would tell the applier a human chose it, which is the precedent this
-- migration exists to break.
DELETE FROM public.asset_field_value WHERE set_by = 'default';

ALTER TABLE public.asset_field_value
    ADD CONSTRAINT asset_field_value_set_by_check
    CHECK ((set_by = ANY (ARRAY[
        'manual'::text, 'exif'::text, 'iptc'::text, 'xmp'::text,
        'api'::text, 'import'::text, 'computed'::text
    ])));

DROP TABLE public.field_default_override;

ALTER TABLE public.field_definition DROP COLUMN default_value;
