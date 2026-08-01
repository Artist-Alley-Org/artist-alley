-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00024_shipped_field_vocabularies.sql
--
-- Give the two shipped fields that CANNOT HOLD A VALUE WITHOUT ONE a
-- starting vocabulary: `country` (tree) and `keywords` (multi_select),
-- both of which have shipped with options = '{}' since the v0.1
-- baseline (#820).
--
-- # Why this is a defect and not a preference
--
-- Seven of the nine shipped field definitions are typed so that an
-- empty options document costs nothing — a `text` field carries no
-- vocabulary, a `number` field carries at most min/max. Two are not:
--
--   * multi_select resolves a stored value by looking the slug up in
--     options.values. With no values there is nothing to select, and
--     metadata/handler.go rejects an empty value_options outright.
--   * tree is a HIERARCHY. `country` has shipped as a hierarchical
--     field with no hierarchy — an empty tree is not a small tree, it
--     is a field with no reachable state at all.
--
-- So these two were unusable on a fresh install, in principle rather
-- than by omission, and nothing said so: #812 fixed the CATALOGUE
-- divergence (`aa seed --reset` was deleting the shipped rows) and
-- left the DATA divergence. Empty is exactly the state that hid #778
-- (tree rotted three ways because nothing wrote it), #791 (boolean
-- read the wrong column because nobody set one) and #807/#808 (six
-- types silently discarded, invisible because no value existed to
-- discard). Each was found only when real data reached the type, and
-- these are the fields an OPERATOR meets first on a fresh install.
--
-- # Why the country leaf slugs are ISO 3166-1 alpha-2 codes
--
-- Not a style choice. ADR 0012's tree-storage amendment stores a tree
-- value as ONE LEAF SLUG — the slug is the value's whole identity, and
-- the ancestor path is reassembled at read time from the options
-- document. That makes the slug the thing a federation peer receives.
-- A hand-invented `united-kingdom` would be ours alone and would mean
-- whatever the peer's own catalogue happened to call it; `gb` is an
-- international standard and means the same thing on both ends.
--
-- Tree-wide slug uniqueness — the invariant that makes a bare leaf a
-- complete address, enforced by metadata.NormalizeOptionsDoc via
-- collectSlugs — then comes for free rather than being something to
-- police: every alpha-2 code is exactly two letters and every branch
-- slug is a word, so no branch can collide with a leaf.
--
-- The BRANCHES are not ISO. There is no alpha-2 code for a continent,
-- so `europe` / `americas` / `asia` / `africa` / `oceania` are ours:
-- a display grouping, deliberately one level deep, so the portable
-- half of the tree is exactly the half a value is stored as.
--
-- # A STARTING vocabulary, not an authority
--
-- 24 countries across 5 continents, and 17 generic media keywords.
-- Neither list is complete and neither is meant to be. The ~250-entry
-- ISO table is not a better default — it is a worse one, because an
-- operator curating their own list would have to delete 230 rows
-- first. What ships is a set large enough to EXERCISE the type
-- (nested branches, multi-level resolution, multi-value selection)
-- and small enough to be replaced. Options are never hard-deleted
-- (ADR 0012), so an operator retires what they do not want and adds
-- what they do.
--
-- # Heal, don't clobber
--
-- Both UPDATEs are guarded on the field having NO vocabulary today:
-- absent `values` key, a non-array `values`, or an empty array. An
-- operator who has already curated `country` on a running install
-- keeps every term. The merge is `options || jsonb_build_object(...)`
-- rather than a wholesale assignment so any other key in the document
-- (a future min/max, a UI hint) survives.
--
-- The documents written here are in the shape
-- metadata.NormalizeOptionsDoc accepts and re-encodes unchanged:
-- {"values": [{"value": <slug>, "label": <human>, "children": [...]}]}.
-- That is asserted directly, against the migrated database, by
-- TestShippedFieldVocabularies_NormalizeRoundTrip — a migration that
-- wrote a document the admin edit path would reject is a document an
-- operator cannot save after opening.
--
-- Plain DML, so no StatementBegin/End markers — those exist for
-- plpgsql bodies whose semicolons goose would otherwise split on.

-- +goose Up

-- country: continent → country, leaves are ISO 3166-1 alpha-2.
UPDATE public.field_definition
   SET options = COALESCE(options, '{}'::jsonb) || jsonb_build_object('values', '[
        {"value": "africa", "label": "Africa", "children": [
            {"value": "eg", "label": "Egypt"},
            {"value": "ke", "label": "Kenya"},
            {"value": "ma", "label": "Morocco"},
            {"value": "ng", "label": "Nigeria"},
            {"value": "za", "label": "South Africa"}
        ]},
        {"value": "americas", "label": "Americas", "children": [
            {"value": "ar", "label": "Argentina"},
            {"value": "br", "label": "Brazil"},
            {"value": "ca", "label": "Canada"},
            {"value": "mx", "label": "Mexico"},
            {"value": "us", "label": "United States"}
        ]},
        {"value": "asia", "label": "Asia", "children": [
            {"value": "cn", "label": "China"},
            {"value": "in", "label": "India"},
            {"value": "jp", "label": "Japan"},
            {"value": "kr", "label": "South Korea"},
            {"value": "sg", "label": "Singapore"}
        ]},
        {"value": "europe", "label": "Europe", "children": [
            {"value": "fr", "label": "France"},
            {"value": "de", "label": "Germany"},
            {"value": "it", "label": "Italy"},
            {"value": "nl", "label": "Netherlands"},
            {"value": "es", "label": "Spain"},
            {"value": "se", "label": "Sweden"},
            {"value": "gb", "label": "United Kingdom"}
        ]},
        {"value": "oceania", "label": "Oceania", "children": [
            {"value": "au", "label": "Australia"},
            {"value": "nz", "label": "New Zealand"}
        ]}
   ]'::jsonb),
       -- The shipped description promised "Country / region / city",
       -- three levels, of a field that had none. Two is what ships.
       description = 'Where the work was made or the subject is located. Continent, then country; country slugs are ISO 3166-1 alpha-2 codes, so a value means the same thing on a federated peer. A starting vocabulary — extend or retire terms to suit.',
       updated_at = now()
 WHERE code = 'country'
   AND (options IS NULL
        OR jsonb_typeof(options -> 'values') IS DISTINCT FROM 'array'
        OR options -> 'values' = '[]'::jsonb);

-- keywords: flat, generic, media-catalogue terms.
UPDATE public.field_definition
   SET options = COALESCE(options, '{}'::jsonb) || jsonb_build_object('values', '[
        {"value": "abstract", "label": "Abstract"},
        {"value": "animation", "label": "Animation"},
        {"value": "architecture", "label": "Architecture"},
        {"value": "archival", "label": "Archival"},
        {"value": "editorial", "label": "Editorial"},
        {"value": "event", "label": "Event"},
        {"value": "illustration", "label": "Illustration"},
        {"value": "landscape", "label": "Landscape"},
        {"value": "logo", "label": "Logo"},
        {"value": "nature", "label": "Nature"},
        {"value": "people", "label": "People"},
        {"value": "photograph", "label": "Photograph"},
        {"value": "portrait", "label": "Portrait"},
        {"value": "product", "label": "Product"},
        {"value": "promotional", "label": "Promotional"},
        {"value": "reference", "label": "Reference"},
        {"value": "texture", "label": "Texture"}
   ]'::jsonb),
       description = 'Multi-value tagging. A starting vocabulary — extend or retire terms to suit.',
       updated_at = now()
 WHERE code = 'keywords'
   AND (options IS NULL
        OR jsonb_typeof(options -> 'values') IS DISTINCT FROM 'array'
        OR options -> 'values' = '[]'::jsonb);

-- +goose Down
--
-- Removes the `values` key, restoring options to the '{}' the baseline
-- shipped — but ONLY where the vocabulary is still byte-for-byte the
-- one this migration wrote. An operator who added, retired or
-- relabelled a term has a curated vocabulary, and a down migration
-- that deleted it would take their data's meaning with it: every
-- stored slug would stop resolving and would render raw.
--
-- The descriptions are restored to the baseline's wording on the same
-- guard, so an up/down/up cycle is a genuine round trip.
UPDATE public.field_definition
   SET options = options - 'values',
       description = 'Country / region / city tree.',
       updated_at = now()
 WHERE code = 'country'
   AND options -> 'values' = '[
        {"value": "africa", "label": "Africa", "children": [
            {"value": "eg", "label": "Egypt"},
            {"value": "ke", "label": "Kenya"},
            {"value": "ma", "label": "Morocco"},
            {"value": "ng", "label": "Nigeria"},
            {"value": "za", "label": "South Africa"}
        ]},
        {"value": "americas", "label": "Americas", "children": [
            {"value": "ar", "label": "Argentina"},
            {"value": "br", "label": "Brazil"},
            {"value": "ca", "label": "Canada"},
            {"value": "mx", "label": "Mexico"},
            {"value": "us", "label": "United States"}
        ]},
        {"value": "asia", "label": "Asia", "children": [
            {"value": "cn", "label": "China"},
            {"value": "in", "label": "India"},
            {"value": "jp", "label": "Japan"},
            {"value": "kr", "label": "South Korea"},
            {"value": "sg", "label": "Singapore"}
        ]},
        {"value": "europe", "label": "Europe", "children": [
            {"value": "fr", "label": "France"},
            {"value": "de", "label": "Germany"},
            {"value": "it", "label": "Italy"},
            {"value": "nl", "label": "Netherlands"},
            {"value": "es", "label": "Spain"},
            {"value": "se", "label": "Sweden"},
            {"value": "gb", "label": "United Kingdom"}
        ]},
        {"value": "oceania", "label": "Oceania", "children": [
            {"value": "au", "label": "Australia"},
            {"value": "nz", "label": "New Zealand"}
        ]}
   ]'::jsonb;

UPDATE public.field_definition
   SET options = options - 'values',
       description = 'Multi-value tagging.',
       updated_at = now()
 WHERE code = 'keywords'
   AND options -> 'values' = '[
        {"value": "abstract", "label": "Abstract"},
        {"value": "animation", "label": "Animation"},
        {"value": "architecture", "label": "Architecture"},
        {"value": "archival", "label": "Archival"},
        {"value": "editorial", "label": "Editorial"},
        {"value": "event", "label": "Event"},
        {"value": "illustration", "label": "Illustration"},
        {"value": "landscape", "label": "Landscape"},
        {"value": "logo", "label": "Logo"},
        {"value": "nature", "label": "Nature"},
        {"value": "people", "label": "People"},
        {"value": "photograph", "label": "Photograph"},
        {"value": "portrait", "label": "Portrait"},
        {"value": "product", "label": "Product"},
        {"value": "promotional", "label": "Promotional"},
        {"value": "reference", "label": "Reference"},
        {"value": "texture", "label": "Texture"}
   ]'::jsonb;
