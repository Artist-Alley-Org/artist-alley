-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00029_site_text_overrides.sql
--
-- Operator overrides of shipped UI strings (#794, ADR 0081 §1).
--
-- One row per (i18n key, language). The dotted key IS the identity —
-- deliberately not the page+name composite the prior art carries, which
-- exists only because it has no global string namespace and ours does
-- (`admin.collection_fields.section_title`). `language` is part of the
-- key because translation is live (#289): a locale-blind override would
-- silently apply an English string to every locale.
--
-- DELIBERATELY A TABLE, NOT A JSONB DOCUMENT IN system_config. The
-- shipped catalogue is ~2,150 overridable keys; one document holding
-- them would make every single-string edit a whole-document
-- read-modify-write, which is exactly the last-write-wins race #737
-- was filed for. Per-row upserts touching one primary key cannot
-- collide with each other.
--
-- No `specific_to_group` column: per-group site text is explicitly not
-- built (ADR 0081 §1). A column is cheap to add later and expensive to
-- remove once written against.
--
-- Nothing here federates. Site text is instance identity — how THIS
-- installation speaks — so there is no origin/remote bookkeeping on the
-- table, by design rather than by omission.
--
-- Plain DDL, so no StatementBegin/End markers — those exist for plpgsql
-- bodies whose semicolons goose would otherwise split on.

-- +goose Up

CREATE TABLE IF NOT EXISTS public.site_text (
    key                 text        NOT NULL,
    language            text        NOT NULL,
    value               text        NOT NULL,
    updated_at          timestamptz NOT NULL DEFAULT now(),
    -- SET NULL rather than CASCADE: deleting the operator who wrote an
    -- override must not silently delete the operator's wording.
    updated_by_user_ref bigint      REFERENCES public."user"(ref) ON DELETE SET NULL,
    CONSTRAINT site_text_pkey PRIMARY KEY (key, language)
);

-- No secondary index, deliberately. The only read is "the whole map",
-- rebuilt wholesale on invalidation and served from the process cache
-- thereafter (ADR 0013) — a sequential scan of a table an operator
-- populates by hand is cheaper than the index that would serve a
-- filtered read this API does not have.

-- +goose Down

DROP TABLE IF EXISTS public.site_text;
