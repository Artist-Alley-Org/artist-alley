-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00028_open_vocabulary.sql
--
-- Give a controlled vocabulary a sanctioned way to GROW, and wire
-- `keywords` to the IPTC tag that has always been its source (#830).
--
-- # What the flag means
--
-- #824 closed the write path: a slug that is not a term of the field is
-- refused, always. That is the right rule for `country` — a catalogue
-- does not acquire a new country because someone's camera spelled one
-- oddly — and the wrong rule for `keywords`, which is exactly the field
-- whose vocabulary is supposed to grow from the material. Real
-- catalogues grow keywords two ways: an operator types a term the field
-- does not have yet, and a file arrives carrying IPTC 2:25 tags nobody
-- has ever entered. Both were unreachable: the only way to add a term
-- was the admin options editor, one field at a time.
--
-- `open_vocabulary` is the sanctioned way past the gate. On a field
-- carrying it, an incoming term that matches nothing is CREATED rather
-- than refused: the trimmed input becomes the label, its slugified form
-- becomes the stored value. Everything else about the field is
-- unchanged — storage, rendering, search, federation. An open
-- vocabulary is a multi_select that differs from a closed one in its
-- WRITE POLICY and in nothing else, which is why this is a boolean and
-- not a twelfth type in field_definition.type's CHECK.
--
-- # Why no CHECK ties it to a type
--
-- The column is legal on every type and HONOURED ONLY FOR
-- multi_select this sprint. A CHECK constraint restricting it would
-- have to be dropped and rewritten the first time an operator wants an
-- open `select` (a plausible next step — see #831), and a constraint
-- that exists to be relaxed is a migration tax on a decision nobody has
-- made yet. The gate enforces the narrowing in Go instead
-- (openVocabularyApplies, app/internal/metadata/open_vocabulary.go), so
-- setting the flag on a `text` field is inert rather than an error.
--
-- # Why `keywords` is wired HERE
--
-- Same precedent as 00025: extraction wiring is data, and the migration
-- that makes a wiring safe is the migration that turns it on. 00025
-- deliberately left `keywords` unwired with the reason recorded in its
-- own comment — "the applier cannot write it... there is no path from
-- extraction to value_options" — and pointed at #789. That path ships
-- with this migration: the applier now carries a multi-value kind,
-- splits IPTC 2:25's comma-joined set, resolves each term against the
-- field's vocabulary, and creates the ones that miss because the field
-- is open. Wiring without the flag would refuse every unknown keyword
-- into the failure queue; the flag without the wiring would leave the
-- richest source of keywords unread. They are one change.
--
-- Plain DDL + DML, so no StatementBegin/End markers — those exist for
-- plpgsql bodies whose semicolons goose would otherwise split on.

-- +goose Up

ALTER TABLE public.field_definition
    ADD COLUMN open_vocabulary boolean DEFAULT false NOT NULL;

COMMENT ON COLUMN public.field_definition.open_vocabulary IS
    'When true, a write naming a term this field does not have CREATES the term instead of being refused. Honoured for multi_select only (#830).';

-- `keywords` becomes the first open vocabulary, and gets the IPTC
-- mapping 00025 recorded and could not act on. Guarded on the state
-- 00024/00025 left so an operator who has already re-pointed the field
-- keeps their choice.
UPDATE public.field_definition
   SET open_vocabulary   = true,
       extraction_source = 'iptc_keywords',
       updated_at        = now()
 WHERE code = 'keywords'
   AND subject_kind = 'asset'
   AND extraction_source = '';

-- +goose Down

-- Unwire only what the Up wired: an operator who re-pointed `keywords`
-- at a different canonical made a choice, and discarding it would
-- silently un-configure a field they deliberately set.
UPDATE public.field_definition
   SET extraction_source = '', updated_at = now()
 WHERE code = 'keywords'
   AND subject_kind = 'asset'
   AND extraction_source = 'iptc_keywords';

-- Terms created through the flag are ordinary options and survive the
-- rollback — they are values assets already carry, and dropping the
-- column is not a reason to orphan them.
ALTER TABLE public.field_definition DROP COLUMN open_vocabulary;
