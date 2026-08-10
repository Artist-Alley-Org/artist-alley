-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00045_field_definition_show_on_card.sql
--
-- An operator can now choose which fields appear at a glance on a card
-- (#552, split from #515 slice 4b).
--
-- # What it is
--
-- A DISPLAY HINT, in exactly the class ADR 0012 already annotates
-- `display_order` / `display_group` with: *"UI may use; do not gate logic
-- on these."* Nothing may branch on it for access, filtering or
-- correctness. A client that ignores it entirely must still be correct,
-- merely plainer — which is the property the card's fallback preserves:
-- with no field marked, the details footer renders exactly what it
-- rendered before.
--
-- Default FALSE, and no row is seeded true. "Sensible default" here means
-- the card's own default, not a set of fields chosen for every operator on
-- every install — a shipped choice would be one more thing an operator has
-- to undo before they can make their own.
--
-- # It FEDERATES (ADR 0012 amendment 2026-08-10)
--
-- ADR 0083's exclusion criterion is that a property is left out "because it
-- names something that exists only on the sender". A card-display flag
-- names the FIELD, not the server, so it travels with the definition — and
-- the consequence is the point: a peer's fields render the way that peer
-- meant them to. Contrast `mirrors_column` (00044), which names a column of
-- the sender's own schema and therefore stays out.
--
-- (The envelope itself is still unbuilt — ADR 0083 records the requirement
-- deliberately unimplemented. This records which side of the criterion the
-- flag falls on, so the exchange, when built, does not relitigate it.)
--
-- # Why a gated field may not be an at-a-glance field
--
-- The CHECK constraint below refuses `show_on_card` on a field carrying a
-- `read_capability`. A card is rendered on browse, for a page of assets, to
-- callers including anonymous ones — the surface where the server has NOT
-- evaluated a per-field capability. There were two honest options: strip
-- gated fields silently at render time, or refuse the combination outright.
-- Silent stripping makes an operator's setting do nothing with no error
-- anywhere, which is the "accepted but inert" defect class; refusing says so
-- at the moment the operator asks for it. The constraint holds in BOTH
-- directions, so adding a read capability to a field already on the card is
-- refused too, rather than quietly turning the card into a leak.
--
-- Plain DDL, so no StatementBegin/End markers.

-- +goose Up

ALTER TABLE public.field_definition
    ADD COLUMN show_on_card boolean DEFAULT false NOT NULL;

ALTER TABLE public.field_definition
    ADD CONSTRAINT field_definition_show_on_card_ungated_check
    CHECK (NOT (show_on_card AND coalesce(read_capability, '') <> ''));

COMMENT ON COLUMN public.field_definition.show_on_card IS
    'Display hint (#552): render this field at a glance on an asset card. Same class as display_order / display_group — UI may use it, nothing may gate access, filtering or correctness on it, and a client that ignores it must still be correct, merely plainer. FEDERATES with the definition: it names the field, not the server (ADR 0012 amendment 2026-08-10, against ADR 0083''s exclusion criterion). Refused on a field carrying a read_capability, because the card renders on browse where no per-field capability has been evaluated.';

-- The card query asks for "the fields marked at-a-glance", never for one
-- field by id. A partial index on the flag is the whole access pattern.
CREATE INDEX field_definition_show_on_card_idx
    ON public.field_definition (display_group, display_order, code)
    WHERE show_on_card;

-- +goose Down

DROP INDEX IF EXISTS public.field_definition_show_on_card_idx;
ALTER TABLE public.field_definition
    DROP CONSTRAINT IF EXISTS field_definition_show_on_card_ungated_check;
ALTER TABLE public.field_definition DROP COLUMN show_on_card;
