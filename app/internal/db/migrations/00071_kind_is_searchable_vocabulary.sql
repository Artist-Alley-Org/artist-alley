-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00071_kind_is_searchable_vocabulary.sql
--
-- #1417, sprint 24: a kind is searchable vocabulary.
--
-- A person typing `ebook`, `sprite` or `video` into the ordinary search
-- box could not find a post that contains that kind unless somebody had
-- written the word into a title, description, tag or field. The
-- structured `kind:` filter found it; free text did not. The resolved
-- kind of an asset (the badge its card draws) was not indexed at all.
--
-- ## What changes
--
-- 1. THE DERIVATION BECOMES DATABASE-RESIDENT, ONCE. `asset_view_kind`
--    is the SQL twin of viewkind.ForAsset, and its body is the text
--    viewkind.KindSQL("") renders, spliced VERBATIM. Nothing here is a
--    second taxonomy: app/internal/viewkind stays the sole authority,
--    and two tests pin this copy to it:
--    posts.TestKindVocabulary_ResidentDerivationMatchesGo drives the
--    whole vocabulary through this function, through KindSQL in the
--    same session and through ForAsset and requires all three to
--    agree; posts.TestKindVocabulary_ResidentDerivationTextIsKindSQL
--    compares the stored body to the live rendering byte for byte. A
--    vocabulary edit in Go without a migration therefore fails CI
--    rather than drifting.
--
-- 2. THE ASSET DOCUMENT GAINS THE KIND AT WEIGHT D.
--    rebuild_asset_search_text appends the derived kind as one more ingredient beside
--    the searchable field text. Eleven kinds are emitted as typed:
--    image, video, pdf, audio, font, sprite, 3d, ebook, doc, audiobook,
--    archive. `placeholder` emits NOTHING: it is the resolver's "I could
--    not tell", not a word anyone searches for. `sequence` is never
--    produced for a single asset. Under the `english` configuration
--    every emitted kind is one stable lexeme that round-trips through
--    plainto_tsquery (image -> imag, archive -> archiv, the rest
--    unchanged), so the document holds exactly what the query asks.
--
-- 3. THE POST DOCUMENT INHERITS IT THROUGH THE EXISTING FOLD, AND THE
--    FOLD IS CORRECTED. `rebuild_post_search_text` used to SERIALISE
--    each eligible member's tsvector to text and re-tokenise it:
--
--        string_agg(a.search_text::text, ' ')  ->  to_tsvector(...)
--
--    The text form of a tsvector carries position and weight markers
--    (`'alpha':1A 'beta':2A 'gamma':3`), and the tokeniser turned those
--    markers into LEXEMES: `1a`, `2a` and the bare position `3`. Every
--    post with a folded member carried junk words nobody wrote, and a
--    search for `1a` returned posts for no reason a reader could see.
--    (Weight D is the default and is never printed, so a D-weight
--    lexeme at position 3 produced `3`, not `3d`; the junk is the A/B/C
--    markers and bare positions.) The fold is now a tsvector
--    concatenation, `tsvector_agg`, an aggregate over the built-in
--    tsvector_concat, ordered by member id so a rebuild is
--    deterministic and inert to membership order and cover choice. The
--    aggregate is re-weighted to D as before, so member material,
--    including the inherited kind, lands where it always did.
--
-- 4. THE ASSET TRIGGER ALSO REFRESHES ON asset_type / file_extension.
--    The document now derives from those two columns. Nothing on the
--    wire updates them today (they are create-only), but a document that
--    depends on a column must follow that column, not an assumption
--    about its callers.
--
-- ## The disclosure boundary does not move
--
-- The post document stays caller-independent and folds only members
-- that are public, active and ready (#883, 00034). A restricted member
-- contributes nothing, kind included: the card withholds a restricted
-- member's kind (facet/selection.go), so the kind is withheld content
-- and belongs in the gated asset document (ADR 0056 4c), where the
-- owner reaches it through visibility.AssetSearchMatchSQL and a
-- stranger does not. An authorised caller finds their restricted
-- member's kind through `kind:` (per caller, #1190) and through direct
-- asset search; not through the post's shared free text.
--
-- ## Backfill, both directions, inline
--
-- Up rebuilds EVERY asset document under the new builder and then every
-- post document under the corrected fold, in one pass each, ascending
-- by id. The per-asset propagation trigger is suppressed for the pass
-- (the transaction-local flag 00067 introduced) so a post with N members
-- is rebuilt once rather than N times, and the explicit post pass is
-- what discharges the debt that suppression creates. No install needs
-- the admin reindex.
--
-- Down restores the prior asset builder (00001), the prior post builder
-- (00067, entry lock included) and the prior asset trigger, drops the
-- aggregate and the derivation, and rebuilds every asset and post
-- document under the restored functions. That brings the OLD FOLD
-- SEMANTICS BACK, marker junk included, deliberately: stored documents
-- must agree with the functions that maintain them, and a database at
-- 00070 with 00071's documents would be a mixed-version state nothing
-- can reason about. After Down no row retains a kind lexeme or a
-- corrected-fold document. Precedent: 00034.
--
-- Ordering inside Down: the builders are restored FIRST, so that the
-- DROPs that follow remove objects nothing references any more, and the
-- backfill that follows runs under the restored definitions.

-- +goose Up

-- The resident derivation. The body below is viewkind.KindSQL("")
-- spliced verbatim: `asset_type` and `file_extension` are the function's
-- parameters, which is what an empty alias renders. Do not edit it by
-- hand; change app/internal/viewkind and cut a migration, and the
-- byte-equality test will tell you when the two disagree.
--
-- IMMUTABLE: lower, btrim and regexp_replace are immutable and the
-- branch table is constant. Not STRICT: a NULL asset_type with a real
-- extension must still resolve through the extension, and NULL/NULL must
-- resolve to 'placeholder', exactly as ForAsset does.
-- +goose StatementBegin
CREATE FUNCTION public.asset_view_kind(asset_type bigint, file_extension text) RETURNS text
    LANGUAGE sql IMMUTABLE
    AS $$
SELECT CASE
            WHEN asset_type = 6 THEN 'archive'
            WHEN asset_type = 11 THEN 'audiobook'
            WHEN asset_type = 13 THEN 'sprite'
            ELSE COALESCE(CASE regexp_replace(lower(btrim(file_extension)), '^\.', '')
              WHEN 'epub' THEN 'ebook'
              WHEN 'm4b' THEN 'audiobook'
              WHEN 'aax' THEN 'audiobook'
              WHEN 'jpg' THEN 'image'
              WHEN 'jpeg' THEN 'image'
              WHEN 'png' THEN 'image'
              WHEN 'gif' THEN 'image'
              WHEN 'webp' THEN 'image'
              WHEN 'bmp' THEN 'image'
              WHEN 'tiff' THEN 'image'
              WHEN 'tif' THEN 'image'
              WHEN 'avif' THEN 'image'
              WHEN 'heic' THEN 'image'
              WHEN 'heif' THEN 'image'
              WHEN 'svg' THEN 'image'
              WHEN 'hdr' THEN 'image'
              WHEN 'exr' THEN 'image'
              WHEN 'pic' THEN 'image'
              WHEN 'cr2' THEN 'image'
              WHEN 'nef' THEN 'image'
              WHEN 'dng' THEN 'image'
              WHEN 'arw' THEN 'image'
              WHEN 'rw2' THEN 'image'
              WHEN 'eps' THEN 'image'
              WHEN 'ps' THEN 'image'
              WHEN 'psd' THEN 'image'
              WHEN 'psb' THEN 'image'
              WHEN 'mobi' THEN 'image'
              WHEN 'cbz' THEN 'image'
              WHEN 'cbr' THEN 'image'
              WHEN 'cb7' THEN 'image'
              WHEN 'mp4' THEN 'video'
              WHEN 'mov' THEN 'video'
              WHEN 'mkv' THEN 'video'
              WHEN 'webm' THEN 'video'
              WHEN 'avi' THEN 'video'
              WHEN 'wmv' THEN 'video'
              WHEN 'mpg' THEN 'video'
              WHEN 'mpeg' THEN 'video'
              WHEN '3gp' THEN 'video'
              WHEN 'flv' THEN 'video'
              WHEN 'm4v' THEN 'video'
              WHEN 'ts' THEN 'video'
              WHEN 'lrv' THEN 'video'
              WHEN 'insv' THEN 'video'
              WHEN 'mts' THEN 'video'
              WHEN 'm2ts' THEN 'video'
              WHEN 'vob' THEN 'video'
              WHEN 'f4v' THEN 'video'
              WHEN 'mxf' THEN 'video'
              WHEN 'mp3' THEN 'audio'
              WHEN 'wav' THEN 'audio'
              WHEN 'flac' THEN 'audio'
              WHEN 'ogg' THEN 'audio'
              WHEN 'oga' THEN 'audio'
              WHEN 'm4a' THEN 'audio'
              WHEN 'aac' THEN 'audio'
              WHEN 'opus' THEN 'audio'
              WHEN 'pdf' THEN 'pdf'
              WHEN 'ttf' THEN 'font'
              WHEN 'otf' THEN 'font'
              WHEN 'ttc' THEN 'font'
              WHEN 'otc' THEN 'font'
              WHEN 'woff' THEN 'font'
              WHEN 'woff2' THEN 'font'
              WHEN 'glb' THEN '3d'
              WHEN 'gltf' THEN '3d'
              WHEN 'obj' THEN '3d'
              WHEN 'fbx' THEN '3d'
              WHEN 'blend' THEN '3d'
              WHEN 'mview' THEN '3d'
              WHEN 'dae' THEN '3d'
              WHEN 'ply' THEN '3d'
              WHEN 'stl' THEN '3d'
              WHEN '3ds' THEN '3d'
              WHEN 'x3d' THEN '3d'
              WHEN 'wrl' THEN '3d'
              WHEN 'usd' THEN '3d'
              WHEN 'usda' THEN '3d'
              WHEN 'usdc' THEN '3d'
              WHEN 'usdz' THEN '3d'
              WHEN 'abc' THEN '3d'
              WHEN 'md2' THEN '3d'
              WHEN 'md3' THEN '3d'
              WHEN 'mdl' THEN '3d'
              WHEN 'ms3d' THEN '3d'
              WHEN 'mb' THEN '3d'
              WHEN 'ma' THEN '3d'
              WHEN 'max' THEN '3d'
              WHEN 'txt' THEN 'doc'
              WHEN 'log' THEN 'doc'
              WHEN 'csv' THEN 'doc'
              WHEN 'tsv' THEN 'doc'
              WHEN 'md' THEN 'doc'
              WHEN 'markdown' THEN 'doc'
              WHEN 'mdx' THEN 'doc'
              WHEN 'rst' THEN 'doc'
              WHEN 'adoc' THEN 'doc'
              WHEN 'org' THEN 'doc'
              WHEN 'json' THEN 'doc'
              WHEN 'jsonc' THEN 'doc'
              WHEN 'yaml' THEN 'doc'
              WHEN 'yml' THEN 'doc'
              WHEN 'toml' THEN 'doc'
              WHEN 'ini' THEN 'doc'
              WHEN 'cfg' THEN 'doc'
              WHEN 'conf' THEN 'doc'
              WHEN 'env' THEN 'doc'
              WHEN 'properties' THEN 'doc'
              WHEN 'sh' THEN 'doc'
              WHEN 'bash' THEN 'doc'
              WHEN 'zsh' THEN 'doc'
              WHEN 'fish' THEN 'doc'
              WHEN 'ps1' THEN 'doc'
              WHEN 'makefile' THEN 'doc'
              WHEN 'mk' THEN 'doc'
              WHEN 'dockerfile' THEN 'doc'
              WHEN 'gitignore' THEN 'doc'
              WHEN 'gitattributes' THEN 'doc'
              WHEN 'py' THEN 'doc'
              WHEN 'pyi' THEN 'doc'
              WHEN 'rb' THEN 'doc'
              WHEN 'lua' THEN 'doc'
              WHEN 'pl' THEN 'doc'
              WHEN 'pm' THEN 'doc'
              WHEN 'js' THEN 'doc'
              WHEN 'mjs' THEN 'doc'
              WHEN 'cjs' THEN 'doc'
              WHEN 'jsx' THEN 'doc'
              WHEN 'tsx' THEN 'doc'
              WHEN 'go' THEN 'doc'
              WHEN 'rs' THEN 'doc'
              WHEN 'java' THEN 'doc'
              WHEN 'kt' THEN 'doc'
              WHEN 'kts' THEN 'doc'
              WHEN 'scala' THEN 'doc'
              WHEN 'swift' THEN 'doc'
              WHEN 'dart' THEN 'doc'
              WHEN 'c' THEN 'doc'
              WHEN 'h' THEN 'doc'
              WHEN 'cpp' THEN 'doc'
              WHEN 'cc' THEN 'doc'
              WHEN 'cxx' THEN 'doc'
              WHEN 'hpp' THEN 'doc'
              WHEN 'hh' THEN 'doc'
              WHEN 'm' THEN 'doc'
              WHEN 'mm' THEN 'doc'
              WHEN 'cs' THEN 'doc'
              WHEN 'php' THEN 'doc'
              WHEN 'hs' THEN 'doc'
              WHEN 'erl' THEN 'doc'
              WHEN 'ex' THEN 'doc'
              WHEN 'exs' THEN 'doc'
              WHEN 'clj' THEN 'doc'
              WHEN 'cljs' THEN 'doc'
              WHEN 'edn' THEN 'doc'
              WHEN 'html' THEN 'doc'
              WHEN 'htm' THEN 'doc'
              WHEN 'css' THEN 'doc'
              WHEN 'scss' THEN 'doc'
              WHEN 'sass' THEN 'doc'
              WHEN 'less' THEN 'doc'
              WHEN 'vue' THEN 'doc'
              WHEN 'svelte' THEN 'doc'
              WHEN 'sql' THEN 'doc'
              WHEN 'graphql' THEN 'doc'
              WHEN 'gql' THEN 'doc'
              WHEN 'xml' THEN 'doc'
              WHEN 'plist' THEN 'doc'
              WHEN 'patch' THEN 'doc'
              WHEN 'diff' THEN 'doc'
              WHEN 'zip' THEN 'archive'
              WHEN 'jar' THEN 'archive'
              WHEN 'war' THEN 'archive'
              WHEN 'ear' THEN 'archive'
              WHEN 'apk' THEN 'archive'
              WHEN 'ipa' THEN 'archive'
              WHEN '7z' THEN 'archive'
              WHEN 'rar' THEN 'archive'
              WHEN 'tar' THEN 'archive'
              WHEN 'tgz' THEN 'archive'
              WHEN 'tbz2' THEN 'archive'
              WHEN 'txz' THEN 'archive'
            END, 'placeholder')
          END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON FUNCTION public.asset_view_kind(bigint, text) IS
    'The SQL twin of viewkind.ForAsset (#1417, sprint 24): resolves an asset row to the badge kind its card draws, from asset_type and file_extension. The body is viewkind.KindSQL("") spliced verbatim by migration 00071 and pinned to the Go authority by two tests (a full-vocabulary oracle and a byte-equality drift guard). Change the vocabulary in app/internal/viewkind and cut a migration; never edit this body by hand. Consumed by rebuild_asset_search_text only; the two kind: filter arms render KindSQL inline.';
-- +goose StatementEnd

-- The corrected fold's aggregate: tsvector concatenation over a set.
-- Postgres ships the binary `||` (tsvector_concat) and no aggregate
-- form of it; this is that aggregate and nothing more. Concatenation
-- offsets the right-hand positions past the left-hand maximum, so a
-- lexeme keeps its weight and gains a position after everything before
-- it, which is what setweight(..., 'D') needs to re-weight the whole
-- fold.
-- +goose StatementBegin
CREATE AGGREGATE public.tsvector_agg(tsvector) (
    SFUNC = tsvector_concat,
    STYPE = tsvector
);
-- +goose StatementEnd

-- The asset document: title A, description B, C empty, searchable active
-- field text D, derived kind D. `placeholder` is turned into the empty
-- string before tokenising, so it contributes no lexeme.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.rebuild_asset_search_text(p_asset_id uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE field_text TEXT;
BEGIN
    SELECT COALESCE(STRING_AGG(CASE WHEN v.value_text IS NOT NULL THEN v.value_text WHEN v.value_options IS NOT NULL THEN array_to_string(v.value_options, ' ') ELSE NULL END, ' '), '')
    INTO field_text
    FROM asset_field_value v JOIN field_definition f ON f.id = v.field_id
    WHERE v.asset_id = p_asset_id AND f.searchable = TRUE AND f.status = 'active';
    UPDATE assets SET search_text =
        setweight(to_tsvector('english', COALESCE(title, '')), 'A') ||
        setweight(to_tsvector('english', COALESCE(description, '')), 'B') ||
        setweight(to_tsvector('english', ''), 'C') ||
        setweight(to_tsvector('english', COALESCE(field_text, '')), 'D') ||
        -- #1417: the resolved kind is vocabulary. One lexeme at D, or
        -- nothing at all when the resolver could not tell.
        setweight(to_tsvector('english',
            COALESCE(NULLIF(public.asset_view_kind(asset_type, file_extension), 'placeholder'), '')), 'D')
     WHERE id = p_asset_id;
END; $$;
-- +goose StatementEnd

-- The post document. Everything from 00067 stays: the entry lock is the
-- first statement, the eligibility filter is #883's. What changes is
-- the fold, which is now a tsvector concatenation rather than a text
-- round trip.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.rebuild_post_search_text(p_post_id uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE member_docs TSVECTOR; post_tag_text TEXT;
BEGIN
    -- THE ENTRY LOCK (00067). First statement, before any aggregate: the
    -- document below is computed from three reads, and a row lock taken
    -- after them would order the writes while still letting the value
    -- be built from a world that had already moved.
    PERFORM 1 FROM public.posts WHERE id = p_post_id FOR NO KEY UPDATE;

    -- #1417: fold the member DOCUMENTS, not their text form. Serialising
    -- a tsvector and re-tokenising it turns its position and weight
    -- markers into lexemes (`1a`, `2a`, bare `3`); concatenation keeps
    -- each lexeme as the lexeme it is. Ordered by member id so a rebuild
    -- is deterministic whatever the membership order or the cover.
    SELECT COALESCE(public.tsvector_agg(COALESCE(a.search_text, ''::tsvector) ORDER BY a.id), ''::tsvector)
      INTO member_docs
      FROM post_assets pa JOIN assets a ON a.id = pa.asset_id
     WHERE pa.post_id = p_post_id
       AND a.deleted_at IS NULL
       -- #883: only members every caller could see standalone
       -- contribute their words to the shared post document.
       AND a.sensitivity = 'public'
       AND a.status = 'active'
       AND a.processing_status = 'ready';
    SELECT COALESCE(string_agg(tag, ' '), '') INTO post_tag_text FROM post_tags WHERE post_id = p_post_id;
    UPDATE posts SET search_text =
        setweight(to_tsvector('english', COALESCE(title, '')), 'A') ||
        setweight(to_tsvector('english', COALESCE(description, '')), 'B') ||
        setweight(to_tsvector('english', COALESCE(post_tag_text, '')), 'C') ||
        setweight(member_docs, 'D')
     WHERE id = p_post_id;
END; $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.asset_changed_trigger() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF (NEW.title IS DISTINCT FROM OLD.title)
       OR (NEW.description IS DISTINCT FROM OLD.description)
       -- #1417: the document derives its kind from these two columns.
       OR (NEW.asset_type IS DISTINCT FROM OLD.asset_type)
       OR (NEW.file_extension IS DISTINCT FROM OLD.file_extension)
       OR (OLD.search_text IS NULL) THEN
        PERFORM rebuild_asset_search_text(NEW.id);
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- The backfill. Assets first, under the new builder, with the per-asset
-- post propagation suppressed; then every post, once, under the
-- corrected fold. Both ascending by id, which is the order 00067 fixed
-- for the trigger chain. The flag is transaction-local (set_config with
-- is_local = true) and is cleared again at the end regardless.
-- +goose StatementBegin
DO $$
DECLARE r RECORD;
BEGIN
    PERFORM set_config('aa.suppress_asset_post_search', 'on', true);
    FOR r IN SELECT id FROM public.assets ORDER BY id LOOP
        PERFORM public.rebuild_asset_search_text(r.id);
    END LOOP;
    FOR r IN SELECT id FROM public.posts ORDER BY id LOOP
        PERFORM public.rebuild_post_search_text(r.id);
    END LOOP;
    PERFORM set_config('aa.suppress_asset_post_search', 'off', true);
END $$;
-- +goose StatementEnd

-- +goose Down

-- 1. The prior asset builder (00001): title A, description B, C empty,
--    field text D. No kind.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.rebuild_asset_search_text(p_asset_id uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE field_text TEXT;
BEGIN
    SELECT COALESCE(STRING_AGG(CASE WHEN v.value_text IS NOT NULL THEN v.value_text WHEN v.value_options IS NOT NULL THEN array_to_string(v.value_options, ' ') ELSE NULL END, ' '), '')
    INTO field_text
    FROM asset_field_value v JOIN field_definition f ON f.id = v.field_id
    WHERE v.asset_id = p_asset_id AND f.searchable = TRUE AND f.status = 'active';
    UPDATE assets SET search_text =
        setweight(to_tsvector('english', COALESCE(title, '')), 'A') ||
        setweight(to_tsvector('english', COALESCE(description, '')), 'B') ||
        setweight(to_tsvector('english', ''), 'C') ||
        setweight(to_tsvector('english', COALESCE(field_text, '')), 'D')
     WHERE id = p_asset_id;
END; $$;
-- +goose StatementEnd

-- 2. The prior post builder, exactly as 00067 left it: entry lock first,
--    then the text-form fold. This is the fold that re-tokenises marker
--    text; it comes back on purpose so that stored documents agree with
--    the function that maintains them.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.rebuild_post_search_text(p_post_id uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE asset_search TEXT; post_tag_text TEXT;
BEGIN
    -- THE ENTRY LOCK. First statement, before any aggregate: the
    -- document below is computed from three reads, and a row lock taken
    -- after them would order the writes while still letting the value
    -- be built from a world that had already moved.
    PERFORM 1 FROM public.posts WHERE id = p_post_id FOR NO KEY UPDATE;

    SELECT COALESCE(string_agg(COALESCE(a.search_text::text, ''), ' '), '') INTO asset_search
      FROM post_assets pa JOIN assets a ON a.id = pa.asset_id
     WHERE pa.post_id = p_post_id
       AND a.deleted_at IS NULL
       -- #883: only members every caller could see standalone
       -- contribute their words to the shared post document.
       AND a.sensitivity = 'public'
       AND a.status = 'active'
       AND a.processing_status = 'ready';
    SELECT COALESCE(string_agg(tag, ' '), '') INTO post_tag_text FROM post_tags WHERE post_id = p_post_id;
    UPDATE posts SET search_text =
        setweight(to_tsvector('english', COALESCE(title, '')), 'A') ||
        setweight(to_tsvector('english', COALESCE(description, '')), 'B') ||
        setweight(to_tsvector('english', COALESCE(post_tag_text, '')), 'C') ||
        setweight(to_tsvector('english', COALESCE(asset_search, '')), 'D')
     WHERE id = p_post_id;
END; $$;
-- +goose StatementEnd

-- 3. The prior asset trigger (00001): title, description, or a document
--    that was never built.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.asset_changed_trigger() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF (NEW.title IS DISTINCT FROM OLD.title)
       OR (NEW.description IS DISTINCT FROM OLD.description)
       OR (OLD.search_text IS NULL) THEN
        PERFORM rebuild_asset_search_text(NEW.id);
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- 4. The Sprint 24 objects, now unreferenced.
-- +goose StatementBegin
DROP AGGREGATE IF EXISTS public.tsvector_agg(tsvector);
-- +goose StatementEnd

-- +goose StatementBegin
DROP FUNCTION IF EXISTS public.asset_view_kind(bigint, text);
-- +goose StatementEnd

-- 5. Every document back under the restored builders. Same shape as Up.
-- +goose StatementBegin
DO $$
DECLARE r RECORD;
BEGIN
    PERFORM set_config('aa.suppress_asset_post_search', 'on', true);
    FOR r IN SELECT id FROM public.assets ORDER BY id LOOP
        PERFORM public.rebuild_asset_search_text(r.id);
    END LOOP;
    FOR r IN SELECT id FROM public.posts ORDER BY id LOOP
        PERFORM public.rebuild_post_search_text(r.id);
    END LOOP;
    PERFORM set_config('aa.suppress_asset_post_search', 'off', true);
END $$;
-- +goose StatementEnd
