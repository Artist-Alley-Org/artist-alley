-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00044_field_definition_mirrors_column.sql
--
-- A field definition can now DECLARE that it is a view onto a column of
-- `assets`, and the declaration is the only place that fact is written
-- down (#822).
--
-- # The defect
--
-- `assets.title` and `assets.description` are real columns with 93 Go
-- references between them. `title` and `description` are ALSO shipped
-- field definitions, active on every install. Nothing expressed that
-- they are the same thing, so the field rows were a second, independent
-- storage for a value the column already held — a rule expressed in two
-- places instead of obtained from one, which is the shape epic #665
-- closed five visibility leaks over.
--
-- It was LATENT only because no `asset_field_value` row had ever named
-- either field. It was one write away from real: the upload modal
-- already renders every active field for the asset type, `title` and
-- `description` among them, and PUTs each one to
-- /assets/{id}/fields/{field_id}.
--
-- # The shape chosen, and the two rejected
--
-- Option 1 of #822: a mapping column. `assets.title` stays the storage;
-- the field definition points at it. Removing the definitions (option 2)
-- would have dropped title/description out of the field-driven surfaces
-- an operator reasonably expects them on — search config, display
-- groups, extraction targets (#800 needs `title` because IPTC ObjectName
-- maps to it). Promoting the columns into fields (option 3) is
-- unaffordable against 93 call sites.
--
-- # Why the guard triggers exist
--
-- "The Go paths now route mirrored writes to the column" is a rule
-- expressed in Go — exactly the thing this issue is about. So the
-- database refuses the divergent copy outright: a mirrored field can
-- hold no row in `asset_field_value` or `asset_field_value_history`, on
-- ANY path, including the seed loader, `psql`, a future import and a
-- Go path nobody has taught. A path that has not learned to route
-- fails loudly at the constraint instead of quietly writing a second
-- copy, which is the failure mode #822 exists to prevent.
--
-- The second guard closes the door from the other side: DECLARING a
-- mirror on a field that already carries values would manufacture the
-- divergence the first guard prevents, so it is refused until those
-- values are cleared.
--
-- # Why the column pointer is resolved by dynamic SQL
--
-- `asset_mirror_read` / `asset_mirror_write` / `asset_mirror_fill` use
-- format('%I') rather than a CASE over 'title' / 'description'. A CASE
-- would be a THIRD statement of which columns are mirrorable, alongside
-- the CHECK constraint and the seeded rows, and it is the one that would
-- be forgotten when a fourth column is added. The CHECK constraint below
-- is the single authority on what may be mirrored; the functions carry
-- no list at all. format('%I') quotes the identifier, so a value that
-- somehow evaded the CHECK yields "column does not exist", never
-- injected SQL.
--
-- plpgsql bodies below, so StatementBegin/End markers are load-bearing:
-- without them goose splits the function bodies on their internal
-- semicolons and a fresh database fails to migrate.

-- +goose Up

ALTER TABLE public.field_definition
    ADD COLUMN mirrors_column text;

-- The single authority on what is mirrorable. Adding a column to this
-- list is the whole change; nothing else enumerates them.
ALTER TABLE public.field_definition
    ADD CONSTRAINT field_definition_mirrors_column_check
    CHECK (mirrors_column IS NULL
           OR mirrors_column = ANY (ARRAY['title'::text, 'description'::text]));

-- A mirror names a column of `assets`, so only an asset field can hold
-- one. Without this a collection field could declare a mirror that
-- resolves against a table it has nothing to do with.
ALTER TABLE public.field_definition
    ADD CONSTRAINT field_definition_mirrors_column_subject_check
    CHECK (mirrors_column IS NULL OR subject_kind = 'asset');

COMMENT ON COLUMN public.field_definition.mirrors_column IS
    'When set, this field is a VIEW onto that column of `assets` rather than storage of its own: reads project the column and writes update it, gated by the column''s own mutation rule. A mirrored field can hold no asset_field_value / _history row — the triggers below refuse one — so the field and the column cannot disagree. NULL (the default) = ordinary field-owned storage. Local declaration: it names a column of THIS server''s schema, so per ADR 0083''s exclusion criterion it does NOT travel in a federated field-schema envelope (#822).';

-- The two shipped definitions become views onto the columns they had
-- always been a second copy of. Guarded on the shipped state so an
-- operator who has re-purposed either row keeps their choice.
UPDATE public.field_definition
   SET mirrors_column = 'title', updated_at = now()
 WHERE code = 'title' AND subject_kind = 'asset' AND type = 'text';

UPDATE public.field_definition
   SET mirrors_column = 'description', updated_at = now()
 WHERE code = 'description' AND subject_kind = 'asset' AND type = 'longtext';

-- ---------------------------------------------------------------------
-- The mirror accessors. One home for "resolve the pointer".
-- ---------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION public.asset_mirror_read(p_asset uuid, p_column text)
RETURNS text
LANGUAGE plpgsql
STABLE
AS $$
DECLARE
    v text;
BEGIN
    IF p_asset IS NULL OR p_column IS NULL THEN
        RETURN NULL;
    END IF;
    EXECUTE format('SELECT %I FROM public.assets WHERE id = $1 AND deleted_at IS NULL', p_column)
        INTO v
        USING p_asset;
    RETURN v;
END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION public.asset_mirror_read(uuid, text) IS
    'Projects the column a mirrored field declares. Soft-deleted assets read NULL, which is exactly ListAssetFieldValues'' existing visibility rule for the row plane (ADR 0063/0064).';

-- +goose StatementBegin
CREATE FUNCTION public.asset_mirror_write(p_asset uuid, p_column text, p_value text)
RETURNS TABLE (mirrored_value text, mirrored_at timestamptz)
LANGUAGE plpgsql
AS $$
BEGIN
    IF p_asset IS NULL OR p_column IS NULL THEN
        RETURN;
    END IF;
    -- The columns are NOT NULL DEFAULT '' — clearing a mirrored field is
    -- the empty string, never a NULL the column would reject.
    RETURN QUERY EXECUTE format(
        'UPDATE public.assets SET %I = $1, updated_at = now() '
        || 'WHERE id = $2 AND deleted_at IS NULL RETURNING %I, updated_at',
        p_column, p_column)
        USING coalesce(p_value, ''), p_asset;
END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION public.asset_mirror_write(uuid, text, text) IS
    'Writes the column a mirrored field declares, returning the persisted value and the row''s new updated_at. Returns no row when the asset is absent or soft-deleted, which the caller reports as 404.';

-- +goose StatementBegin
CREATE FUNCTION public.asset_mirror_fill(p_asset uuid, p_column text, p_value text)
RETURNS TABLE (mirrored_value text, mirrored_at timestamptz)
LANGUAGE plpgsql
AS $$
BEGIN
    IF p_asset IS NULL OR p_column IS NULL THEN
        RETURN;
    END IF;
    RETURN QUERY EXECUTE format(
        'UPDATE public.assets SET %I = $1, updated_at = now() '
        || 'WHERE id = $2 AND deleted_at IS NULL AND %I = '''' RETURNING %I, updated_at',
        p_column, p_column, p_column)
        USING coalesce(p_value, ''), p_asset;
END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION public.asset_mirror_fill(uuid, text, text) IS
    'The if-absent counterpart of asset_mirror_write, for the upload-defaults pass: writes only when the column is still empty. Returns no row when something was already there, which is InsertAssetFieldValueIfAbsent''s zero-rows answer in the mirrored plane.';

-- ---------------------------------------------------------------------
-- The guards. Divergence is refused by the database, not by a rule in
-- Go that every future write path has to remember.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION public.refuse_mirrored_field_value()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    col text;
BEGIN
    SELECT mirrors_column INTO col
      FROM public.field_definition
     WHERE id = NEW.field_id;
    IF col IS NOT NULL THEN
        RAISE EXCEPTION
            'field % is a view onto assets.% and stores no value of its own; write the column',
            NEW.field_id, col
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER asset_field_value_refuse_mirrored
    BEFORE INSERT OR UPDATE ON public.asset_field_value
    FOR EACH ROW EXECUTE FUNCTION public.refuse_mirrored_field_value();

CREATE TRIGGER asset_field_value_history_refuse_mirrored
    BEFORE INSERT OR UPDATE ON public.asset_field_value_history
    FOR EACH ROW EXECUTE FUNCTION public.refuse_mirrored_field_value();

-- +goose StatementBegin
CREATE FUNCTION public.refuse_mirror_over_existing_values()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    n bigint;
BEGIN
    IF NEW.mirrors_column IS NULL THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.mirrors_column IS NOT DISTINCT FROM NEW.mirrors_column THEN
        RETURN NEW;
    END IF;
    SELECT count(*) INTO n FROM public.asset_field_value WHERE field_id = NEW.id;
    IF n > 0 THEN
        RAISE EXCEPTION
            'field % already holds % stored value(s); clear them before declaring it a view onto assets.%',
            NEW.id, n, NEW.mirrors_column
            USING ERRCODE = '23514';
    END IF;
    SELECT count(*) INTO n FROM public.asset_field_value_history WHERE field_id = NEW.id;
    IF n > 0 THEN
        RAISE EXCEPTION
            'field % already holds % history row(s); clear them before declaring it a view onto assets.%',
            NEW.id, n, NEW.mirrors_column
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER field_definition_refuse_mirror_over_values
    BEFORE INSERT OR UPDATE ON public.field_definition
    FOR EACH ROW EXECUTE FUNCTION public.refuse_mirror_over_existing_values();

-- Mirrored fields are read on every GET /assets/{id}/fields, and there
-- are two of them among the nine shipped definitions. A partial index
-- keeps that lookup off a sequential scan of the whole catalogue.
CREATE INDEX field_definition_mirrors_column_idx
    ON public.field_definition (mirrors_column)
    WHERE mirrors_column IS NOT NULL;

-- +goose Down

DROP INDEX IF EXISTS public.field_definition_mirrors_column_idx;
DROP TRIGGER IF EXISTS field_definition_refuse_mirror_over_values ON public.field_definition;
DROP TRIGGER IF EXISTS asset_field_value_history_refuse_mirrored ON public.asset_field_value_history;
DROP TRIGGER IF EXISTS asset_field_value_refuse_mirrored ON public.asset_field_value;
DROP FUNCTION IF EXISTS public.refuse_mirror_over_existing_values();
DROP FUNCTION IF EXISTS public.refuse_mirrored_field_value();
DROP FUNCTION IF EXISTS public.asset_mirror_fill(uuid, text, text);
DROP FUNCTION IF EXISTS public.asset_mirror_write(uuid, text, text);
DROP FUNCTION IF EXISTS public.asset_mirror_read(uuid, text);

ALTER TABLE public.field_definition
    DROP CONSTRAINT IF EXISTS field_definition_mirrors_column_subject_check;
ALTER TABLE public.field_definition
    DROP CONSTRAINT IF EXISTS field_definition_mirrors_column_check;
ALTER TABLE public.field_definition DROP COLUMN mirrors_column;
