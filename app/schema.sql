--
-- PostgreSQL database dump
--

\restrict BIawPaLNrAa1UYpG7hem3AQptIFpU8M7DvjZ4he0fDcFV12iKTSuRDviek2Uif9

-- Dumped from database version 16.13 (Debian 16.13-1.pgdg12+1)
-- Dumped by pg_dump version 16.13 (Debian 16.13-1.pgdg12+1)

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Name: public; Type: SCHEMA; Schema: -; Owner: -
--

-- *not* creating schema, since initdb creates it


--
-- Name: SCHEMA public; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON SCHEMA public IS '';


--
-- Name: pg_trgm; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;


--
-- Name: EXTENSION pg_trgm; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON EXTENSION pg_trgm IS 'text similarity measurement and index searching based on trigrams';


--
-- Name: pgcrypto; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;


--
-- Name: EXTENSION pgcrypto; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON EXTENSION pgcrypto IS 'cryptographic functions';


--
-- Name: uuid-ossp; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS "uuid-ossp" WITH SCHEMA public;


--
-- Name: EXTENSION "uuid-ossp"; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON EXTENSION "uuid-ossp" IS 'generate universally unique identifiers (UUIDs)';


--
-- Name: vector; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public;


--
-- Name: EXTENSION vector; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON EXTENSION vector IS 'vector data type and ivfflat and hnsw access methods';


--
-- Name: ai_embedding_modality; Type: DOMAIN; Schema: public; Owner: -
--

CREATE DOMAIN public.ai_embedding_modality AS text
	CONSTRAINT ai_embedding_modality_check CHECK ((VALUE = ANY (ARRAY['text'::text, 'image'::text, 'multimodal'::text])));


--
-- Name: acl_sweep_on_role_delete(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.acl_sweep_on_role_delete() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    DELETE FROM post_acls
     WHERE principal_type = 'role' AND principal_id = OLD.id::text;
    DELETE FROM collection_acls
     WHERE principal_type = 'role' AND principal_id = OLD.id::text;
    RETURN OLD;
END;
$$;


--
-- Name: acl_sweep_on_team_delete(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.acl_sweep_on_team_delete() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    DELETE FROM post_acls
     WHERE principal_type = 'team' AND principal_id = OLD.id::text;
    DELETE FROM collection_acls
     WHERE principal_type = 'team' AND principal_id = OLD.id::text;
    RETURN OLD;
END;
$$;


--
-- Name: asset_changed_trigger(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.asset_changed_trigger() RETURNS trigger
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


--
-- Name: asset_field_value_trigger(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.asset_field_value_trigger() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    target_asset UUID;
BEGIN
    IF (TG_OP = 'DELETE') THEN
        target_asset := OLD.asset_id;
    ELSE
        target_asset := NEW.asset_id;
    END IF;

    PERFORM rebuild_asset_search_text(target_asset);

    -- Broadcast invalidation so app-instance LRUs can drop their copy.
    PERFORM pg_notify(
        'cache_invalidate',
        json_build_object(
            'domain', 'asset_by_id',
            'key',    target_asset::TEXT,
            'op',     'upsert'
        )::TEXT
    );

    IF (TG_OP = 'DELETE') THEN RETURN OLD; ELSE RETURN NEW; END IF;
END;
$$;


--
-- Name: asset_type_acl_sweep_on_role_delete(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.asset_type_acl_sweep_on_role_delete() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    DELETE FROM asset_type_acls
     WHERE principal_type = 'role' AND principal_id = OLD.id::text;
    RETURN OLD;
END;
$$;


--
-- Name: asset_type_acl_sweep_on_team_delete(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.asset_type_acl_sweep_on_team_delete() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    DELETE FROM asset_type_acls
     WHERE principal_type = 'team' AND principal_id = OLD.id::text;
    RETURN OLD;
END;
$$;


--
-- Name: collections_search_text_trigger(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.collections_search_text_trigger() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN PERFORM rebuild_collection_search_text(NEW.id); RETURN NEW; END;
$$;


--
-- Name: comments_after_delete(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.comments_after_delete() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    -- Only decrement for comments that were live; soft-deleted ones
    -- were already decremented when soft-deleted.
    IF OLD.deleted_at IS NULL THEN
        IF OLD.target_kind = 'post' THEN
            UPDATE posts SET comment_count = GREATEST(comment_count - 1, 0) WHERE id = OLD.target_id;
        END IF;
    END IF;
    RETURN OLD;
END;
$$;


--
-- Name: comments_after_insert(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.comments_after_insert() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.target_kind = 'post' THEN
        UPDATE posts SET comment_count = comment_count + 1 WHERE id = NEW.target_id;
    END IF;
    -- asset/collection comment counters land with the columns themselves.
    RETURN NEW;
END;
$$;


--
-- Name: comments_after_update(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.comments_after_update() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    -- Transitioned from live to soft-deleted.
    IF OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL THEN
        IF NEW.target_kind = 'post' THEN
            UPDATE posts SET comment_count = GREATEST(comment_count - 1, 0) WHERE id = NEW.target_id;
        END IF;
    -- Transitioned the other way (un-delete) — unlikely in MVP but safe.
    ELSIF OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS NULL THEN
        IF NEW.target_kind = 'post' THEN
            UPDATE posts SET comment_count = comment_count + 1 WHERE id = NEW.target_id;
        END IF;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: federation_dispatch_notify(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.federation_dispatch_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('federation_dispatch_pending', NEW.id::text);
    RETURN NEW;
END;
$$;


--
-- Name: federation_inbox_dispatch_notify(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.federation_inbox_dispatch_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('federation_inbox_pending', NEW.id::text);
    RETURN NEW;
END;
$$;


--
-- Name: federation_outbox_dispatch_notify(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.federation_outbox_dispatch_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('federation_outbox_pending', NEW.id::text);
    RETURN NEW;
END;
$$;


--
-- Name: likes_after_delete(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.likes_after_delete() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.target_kind = 'post' THEN
        UPDATE posts SET like_count = GREATEST(like_count - 1, 0) WHERE id = OLD.target_id;
    ELSIF OLD.target_kind = 'comment' THEN
        UPDATE comments SET like_count = GREATEST(like_count - 1, 0) WHERE id = OLD.target_id;
    END IF;
    RETURN OLD;
END;
$$;


--
-- Name: likes_after_insert(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.likes_after_insert() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.target_kind = 'post' THEN
        UPDATE posts SET like_count = like_count + 1 WHERE id = NEW.target_id;
    ELSIF NEW.target_kind = 'comment' THEN
        UPDATE comments SET like_count = like_count + 1 WHERE id = NEW.target_id;
    END IF;
    -- 'asset' target lands when we add assets.like_count later.
    RETURN NEW;
END;
$$;


--
-- Name: post_assets_search_text_trigger(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.post_assets_search_text_trigger() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM rebuild_post_search_text(COALESCE(NEW.post_id, OLD.post_id));
    RETURN NULL;
END;
$$;


--
-- Name: post_tags_search_text_trigger(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.post_tags_search_text_trigger() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM rebuild_post_search_text(COALESCE(NEW.post_id, OLD.post_id));
    RETURN NULL;
END;
$$;


--
-- Name: posts_search_text_trigger(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.posts_search_text_trigger() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM rebuild_post_search_text(NEW.id);
    RETURN NEW;
END;
$$;


--
-- Name: rebuild_asset_search_text(uuid); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.rebuild_asset_search_text(p_asset_id uuid) RETURNS void
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


--
-- Name: rebuild_collection_search_text(uuid); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.rebuild_collection_search_text(p_collection_id uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
BEGIN
    UPDATE collections SET search_text =
        setweight(to_tsvector('english', COALESCE(name, '')), 'A') ||
        setweight(to_tsvector('english', COALESCE(description, '')), 'B')
     WHERE id = p_collection_id;
END; $$;


--
-- Name: rebuild_post_search_text(uuid); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.rebuild_post_search_text(p_post_id uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE asset_search TEXT; post_tag_text TEXT;
BEGIN
    SELECT COALESCE(string_agg(COALESCE(a.search_text::text, ''), ' '), '') INTO asset_search
      FROM post_assets pa JOIN assets a ON a.id = pa.asset_id
     WHERE pa.post_id = p_post_id AND a.deleted_at IS NULL;
    SELECT COALESCE(string_agg(tag, ' '), '') INTO post_tag_text FROM post_tags WHERE post_id = p_post_id;
    UPDATE posts SET search_text =
        setweight(to_tsvector('english', COALESCE(title, '')), 'A') ||
        setweight(to_tsvector('english', COALESCE(description, '')), 'B') ||
        setweight(to_tsvector('english', COALESCE(post_tag_text, '')), 'C') ||
        setweight(to_tsvector('english', COALESCE(asset_search, '')), 'D')
     WHERE id = p_post_id;
END; $$;


--
-- Name: social_sweep_on_post_delete(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.social_sweep_on_post_delete() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    DELETE FROM likes WHERE target_kind = 'post' AND target_id = OLD.id;
    -- Comments cascade via comments.target then their replies via the
    -- parent_id FK (ON DELETE CASCADE).
    DELETE FROM comments WHERE target_kind = 'post' AND target_id = OLD.id;
    RETURN OLD;
END;
$$;


--
-- Name: team_closure_rebuild(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.team_closure_rebuild() RETURNS void
    LANGUAGE plpgsql
    AS $$
BEGIN
    TRUNCATE team_closure;
    -- Self-rows for every team
    INSERT INTO team_closure (ancestor_id, descendant_id, depth)
    SELECT id, id, 0 FROM teams;
    -- Transitive pairs via recursive walk
    INSERT INTO team_closure (ancestor_id, descendant_id, depth)
    SELECT ancestor, descendant, MIN(depth)
      FROM (
        WITH RECURSIVE walk(ancestor, descendant, depth) AS (
            SELECT parent_id, child_id, 1 FROM team_parents
            UNION ALL
            SELECT w.ancestor, tp.child_id, w.depth + 1
              FROM walk w
              JOIN team_parents tp ON tp.parent_id = w.descendant
        )
        SELECT * FROM walk
      ) AS pairs
    GROUP BY ancestor, descendant
    ON CONFLICT DO NOTHING;
END;
$$;


--
-- Name: team_parents_after_delete(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.team_parents_after_delete() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM team_closure_rebuild();
    RETURN NULL;
END;
$$;


--
-- Name: team_parents_after_insert(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.team_parents_after_insert() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO team_closure (ancestor_id, descendant_id, depth)
    SELECT ca.ancestor_id, cd.descendant_id, ca.depth + cd.depth + 1
      FROM team_closure ca
     CROSS JOIN team_closure cd
     WHERE ca.descendant_id = NEW.parent_id
       AND cd.ancestor_id   = NEW.child_id
    ON CONFLICT DO NOTHING;
    RETURN NULL;
END;
$$;


--
-- Name: team_parents_before_insert(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.team_parents_before_insert() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM team_closure
        WHERE ancestor_id   = NEW.child_id
          AND descendant_id = NEW.parent_id
    ) THEN
        RAISE EXCEPTION
            'team_parents: cycle detected (child % is already an ancestor of parent %)',
            NEW.child_id, NEW.parent_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: teams_insert_self_closure(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.teams_insert_self_closure() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO team_closure (ancestor_id, descendant_id, depth)
    VALUES (NEW.id, NEW.id, 0)
    ON CONFLICT DO NOTHING;
    RETURN NEW;
END;
$$;


SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: activities; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.activities (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    activity_uri text NOT NULL,
    activity_type text NOT NULL,
    actor_uri text NOT NULL,
    actor_user_ref bigint,
    object_uri text,
    object_kind text,
    object_local_id text,
    target_uri text,
    to_uris jsonb DEFAULT '[]'::jsonb NOT NULL,
    cc_uris jsonb DEFAULT '[]'::jsonb NOT NULL,
    bto_uris jsonb DEFAULT '[]'::jsonb NOT NULL,
    bcc_uris jsonb DEFAULT '[]'::jsonb NOT NULL,
    audience_uris jsonb DEFAULT '[]'::jsonb NOT NULL,
    payload jsonb NOT NULL,
    signature_value text,
    signature_pubkey text,
    source text DEFAULT 'local'::text NOT NULL,
    published_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT activities_activity_type_check CHECK ((activity_type = ANY (ARRAY['Create'::text, 'Update'::text, 'Delete'::text, 'Follow'::text, 'Accept'::text, 'Reject'::text, 'Undo'::text, 'Like'::text, 'Announce'::text, 'Block'::text, 'Add'::text, 'Remove'::text, 'aa:Share'::text, 'aa:Unshare'::text, 'aa:RevokeShare'::text, 'aa:Approve'::text, 'aa:RequestChanges'::text, 'aa:MarkReviewed'::text, 'aa:Annotation'::text, 'aa:WorkflowTransition'::text, 'aa:AssetVersion'::text, 'aa:Subscribe'::text, 'aa:Mention'::text]))),
    CONSTRAINT activities_object_kind_check CHECK (((object_kind IS NULL) OR (object_kind = ANY (ARRAY['post'::text, 'comment'::text, 'asset'::text, 'user'::text, 'collection'::text, 'workspace'::text, 'brand_kit'::text, 'message'::text, 'activity'::text])))),
    CONSTRAINT activities_source_check CHECK (((source = 'local'::text) OR (source ~~ 'https://%'::text)))
);


--
-- Name: ai_provider_call; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.ai_provider_call (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    provider text NOT NULL,
    model text NOT NULL,
    concern text NOT NULL,
    prompt_template text,
    prompt_version text,
    asset_id uuid,
    job_id uuid,
    input_hash text,
    input_tokens integer,
    output_tokens integer,
    duration_ms integer NOT NULL,
    estimated_cost_usd_micros bigint,
    status text DEFAULT 'success'::text NOT NULL,
    error_message text,
    actor_user_ref bigint,
    triggered_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT ai_provider_call_concern_check CHECK ((concern = ANY (ARRAY['complete'::text, 'embed'::text, 'transcribe'::text, 'tag'::text, 'caption'::text]))),
    CONSTRAINT ai_provider_call_status_check CHECK ((status = ANY (ARRAY['success'::text, 'rate_limited'::text, 'transient_error'::text, 'permanent_error'::text, 'budget_blocked'::text, 'privacy_blocked'::text])))
);


--
-- Name: api_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.api_tokens (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_ref bigint NOT NULL,
    name text NOT NULL,
    token_hash bytea NOT NULL,
    scopes text[] DEFAULT '{}'::text[] NOT NULL,
    expires_at timestamp with time zone,
    last_used_at timestamp with time zone,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    origin_server_id uuid
);


--
-- Name: asset_alternates; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.asset_alternates (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    asset_id uuid NOT NULL,
    label text NOT NULL,
    kind text DEFAULT 'authored'::text NOT NULL,
    object_hash text NOT NULL,
    content_type text DEFAULT 'application/octet-stream'::text NOT NULL,
    size_bytes bigint NOT NULL,
    origin_server_id uuid,
    created_by_user_ref bigint,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    CONSTRAINT asset_alternates_kind_check CHECK (((length(kind) >= 1) AND (length(kind) <= 64))),
    CONSTRAINT asset_alternates_label_check CHECK (((length(label) >= 1) AND (length(label) <= 256))),
    CONSTRAINT asset_alternates_size_bytes_check CHECK ((size_bytes >= 0))
);


--
-- Name: asset_companions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.asset_companions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    asset_id uuid NOT NULL,
    companion_path text NOT NULL,
    object_hash text NOT NULL,
    content_type text DEFAULT 'application/octet-stream'::text NOT NULL,
    size_bytes bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT asset_companions_companion_path_check CHECK (((length(companion_path) >= 1) AND (length(companion_path) <= 512))),
    CONSTRAINT asset_companions_size_bytes_check CHECK ((size_bytes >= 0))
);


--
-- Name: asset_embedding_d768; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.asset_embedding_d768 (
    asset_id uuid NOT NULL,
    provider text NOT NULL,
    model text NOT NULL,
    modality public.ai_embedding_modality NOT NULL,
    embedding public.vector(768) NOT NULL,
    content_hash text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


CREATE TABLE public.asset_visual_embedding (
    asset_id uuid NOT NULL,
    embedding public.vector(768) NOT NULL,
    model text NOT NULL,
    checkpoint text NOT NULL,
    provider text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: asset_field_value; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.asset_field_value (
    asset_id uuid NOT NULL,
    field_id uuid NOT NULL,
    value_text text,
    value_num double precision,
    value_date timestamp with time zone,
    value_options text[],
    value_ref uuid,
    set_by text DEFAULT 'manual'::text NOT NULL,
    set_at timestamp with time zone DEFAULT now() NOT NULL,
    set_by_user_ref bigint,
    CONSTRAINT asset_field_value_set_by_check CHECK ((set_by = ANY (ARRAY['manual'::text, 'exif'::text, 'iptc'::text, 'xmp'::text, 'api'::text, 'import'::text, 'computed'::text])))
);


--
-- Name: COLUMN asset_field_value.value_ref; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.asset_field_value.value_ref IS 'UUID reference value for ref-typed fields. The _ref suffix follows the table''s local multi-type value-column convention (sibling to value_text / value_num / value_date / value_options), distinct from the schema-wide BIGINT-FK _ref rule.';


--
-- Name: asset_field_value_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.asset_field_value_history (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    asset_id uuid NOT NULL,
    field_id uuid NOT NULL,
    old_value jsonb,
    new_value jsonb,
    changed_at timestamp with time zone DEFAULT now() NOT NULL,
    changed_by_user_ref bigint,
    set_by text DEFAULT 'manual'::text NOT NULL
);


--
-- Name: asset_subtitle_tracks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.asset_subtitle_tracks (
    asset_id uuid NOT NULL,
    lang text NOT NULL,
    label text DEFAULT ''::text NOT NULL,
    file_hash text NOT NULL,
    source_format text NOT NULL,
    confidence real DEFAULT 1.0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT asset_subtitle_tracks_confidence_check CHECK (((confidence >= (0)::double precision) AND (confidence <= (1)::double precision))),
    CONSTRAINT asset_subtitle_tracks_lang_check CHECK (((lang ~ '^[A-Za-z]{1,8}(-[A-Za-z0-9]{1,8}){0,4}$'::text) OR (lang = 'und'::text))),
    CONSTRAINT asset_subtitle_tracks_source_format_check CHECK ((source_format = ANY (ARRAY['vtt'::text, 'srt'::text, 'ssa'::text, 'ass'::text, 'sub'::text, 'idx'::text, 'whisper'::text])))
);


--
-- Name: TABLE asset_subtitle_tracks; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.asset_subtitle_tracks IS 'Subtitle / caption tracks attached to video or audio assets. NOT first-class assets — excluded from asset counts. FK + CASCADE binds tracks to their parent. Phase 1.18.B-3.';


--
-- Name: COLUMN asset_subtitle_tracks.confidence; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.asset_subtitle_tracks.confidence IS 'Quality of the source-to-VTT conversion. 1.0 for text-based sources; lower for OCR''d bitmap sources (IDX). UI surfaces a warning below 0.8.';


--
-- Name: asset_tag; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.asset_tag (
    asset_id uuid NOT NULL,
    tag text NOT NULL,
    added_at timestamp with time zone DEFAULT now() NOT NULL,
    source text DEFAULT 'manual'::text NOT NULL,
    confidence real,
    created_by_provider text,
    created_by_model text,
    CONSTRAINT asset_tag_confidence_check CHECK (((confidence IS NULL) OR ((confidence >= (0.0)::double precision) AND (confidence <= (1.0)::double precision)))),
    CONSTRAINT asset_tag_source_check CHECK ((source = ANY (ARRAY['manual'::text, 'ai'::text, 'import'::text])))
);


--
-- Name: asset_type_acls; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.asset_type_acls (
    asset_type_ref bigint NOT NULL,
    principal_type text NOT NULL,
    principal_id text NOT NULL,
    permission text NOT NULL,
    granted_at timestamp with time zone DEFAULT now() NOT NULL,
    granted_by_user_ref bigint,
    expires_at timestamp with time zone,
    CONSTRAINT asset_type_acls_permission_check CHECK ((permission = ANY (ARRAY['read'::text, 'write'::text, 'admin'::text]))),
    CONSTRAINT asset_type_acls_principal_type_check CHECK ((principal_type = ANY (ARRAY['user'::text, 'role'::text, 'team'::text])))
);


--
-- Name: asset_types; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.asset_types (
    ref bigint NOT NULL,
    name character varying(200),
    allowed_extensions text,
    order_by bigint,
    config_options text,
    push_metadata integer,
    colour bigint,
    icon text,
    tab bigint,
    pull_images smallint
);


--
-- Name: assets; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.assets (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    asset_type bigint NOT NULL,
    owner_user_ref bigint,
    status text DEFAULT 'active'::text NOT NULL,
    file_hash text,
    file_extension text,
    file_size_bytes bigint,
    access integer DEFAULT 0 NOT NULL,
    has_image boolean DEFAULT false NOT NULL,
    is_transcoding boolean DEFAULT false NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    origin_server_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    deleted_reason text,
    search_text tsvector,
    state_id uuid,
    team_id uuid,
    processing_status text DEFAULT 'ready'::text NOT NULL,
    thumbhash bytea,
    processing_attempts integer DEFAULT 0 NOT NULL,
    processing_error text,
    processing_started_at timestamp with time zone,
    processing_finished_at timestamp with time zone,
    sensitivity text DEFAULT 'public'::text NOT NULL,
    page_count integer,
    CONSTRAINT assets_processing_status_check CHECK ((processing_status = ANY (ARRAY['pending'::text, 'processing'::text, 'ready'::text, 'failed'::text]))),
    CONSTRAINT assets_sensitivity_check CHECK ((sensitivity = ANY (ARRAY['public'::text, 'team'::text, 'restricted'::text, 'embargo'::text]))),
    CONSTRAINT assets_status_check CHECK ((status = ANY (ARRAY['draft'::text, 'active'::text, 'archived'::text])))
);


--
-- Name: COLUMN assets.sensitivity; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.assets.sensitivity IS 'Intrinsic sensitivity tier (public / team / restricted / embargo). Consumed by the federation outbox sender-refusal gate (1.22.I-g) + the inbox receiver-defense gate (1.22.I-h activated at I-i) when activities target this asset. Default ''public'' matches the pre-arc plaintext-everywhere behavior; operator-explicit upgrades are the load-bearing flow.';


--
-- Name: COLUMN assets.page_count; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.assets.page_count IS 'For paginated assets (PDF today; comics + ebooks later), the total page count extracted by the metadata pipeline. NULL = not paginated OR extractor has not run yet; both are read the same way by clients.';


--
-- Name: audit_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.audit_events (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    event_type text NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    subject_user_ref bigint,
    actor_user_ref bigint,
    ip inet,
    user_agent text,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL
);


--
-- Name: brush_pack_stamps; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.brush_pack_stamps (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    pack_id uuid NOT NULL,
    abr_id text,
    label text,
    width integer NOT NULL,
    height integer NOT NULL,
    storage_key text NOT NULL,
    spacing double precision DEFAULT 0.1 NOT NULL,
    align_to_path boolean DEFAULT false NOT NULL,
    size_jitter double precision,
    opacity_jitter double precision,
    angle_jitter double precision,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT brush_pack_stamps_angle_jitter_check CHECK (((angle_jitter IS NULL) OR ((angle_jitter >= (0)::double precision) AND (angle_jitter <= (360)::double precision)))),
    CONSTRAINT brush_pack_stamps_height_check CHECK ((height > 0)),
    CONSTRAINT brush_pack_stamps_opacity_jitter_check CHECK (((opacity_jitter IS NULL) OR ((opacity_jitter >= (0)::double precision) AND (opacity_jitter <= (1)::double precision)))),
    CONSTRAINT brush_pack_stamps_size_jitter_check CHECK (((size_jitter IS NULL) OR ((size_jitter >= (0)::double precision) AND (size_jitter <= (1)::double precision)))),
    CONSTRAINT brush_pack_stamps_spacing_check CHECK (((spacing > (0)::double precision) AND (spacing <= (10)::double precision))),
    CONSTRAINT brush_pack_stamps_width_check CHECK ((width > 0))
);


--
-- Name: brush_packs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.brush_packs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    owner_user_ref bigint NOT NULL,
    name text NOT NULL,
    source_file text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    origin_server_id uuid
);


--
-- Name: capabilities; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.capabilities (
    code text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    required_license_feature text
);


--
-- Name: collection_acls; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.collection_acls (
    collection_id uuid NOT NULL,
    principal_type text NOT NULL,
    principal_id text NOT NULL,
    permission text NOT NULL,
    granted_at timestamp with time zone DEFAULT now() NOT NULL,
    granted_by_user_ref bigint,
    expires_at timestamp with time zone,
    CONSTRAINT collection_acls_permission_check CHECK ((permission = ANY (ARRAY['read'::text, 'write'::text, 'admin'::text]))),
    CONSTRAINT collection_acls_principal_type_check CHECK ((principal_type = ANY (ARRAY['user'::text, 'role'::text, 'team'::text])))
);


--
-- Name: collection_field_value; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.collection_field_value (
    collection_id uuid NOT NULL,
    field_id uuid NOT NULL,
    value_text text,
    value_num double precision,
    value_date timestamp with time zone,
    value_options text[],
    value_ref uuid,
    set_by text DEFAULT 'manual'::text NOT NULL,
    set_at timestamp with time zone DEFAULT now() NOT NULL,
    set_by_user_ref bigint,
    CONSTRAINT collection_field_value_set_by_check CHECK ((set_by = ANY (ARRAY['manual'::text, 'api'::text, 'import'::text, 'computed'::text])))
);


--
-- Name: collection_field_value_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.collection_field_value_history (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    collection_id uuid NOT NULL,
    field_id uuid NOT NULL,
    old_value jsonb,
    new_value jsonb,
    set_by text DEFAULT 'manual'::text NOT NULL,
    changed_by_user_ref bigint,
    changed_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: collection_posts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.collection_posts (
    collection_id uuid NOT NULL,
    post_id uuid NOT NULL,
    sort_order integer DEFAULT 0 NOT NULL,
    pinned boolean DEFAULT true NOT NULL,
    expires_at timestamp with time zone,
    added_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: collection_resources; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.collection_resources (
    collection_id uuid NOT NULL,
    asset_id uuid NOT NULL,
    sort_order integer DEFAULT 0 NOT NULL,
    pinned boolean DEFAULT true NOT NULL,
    expires_at timestamp with time zone,
    added_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: collections; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.collections (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    owner_user_ref bigint NOT NULL,
    name text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    visibility text DEFAULT 'private'::text NOT NULL,
    membership text DEFAULT 'manual'::text NOT NULL,
    expires_at timestamp with time zone,
    featured boolean DEFAULT false NOT NULL,
    purpose text,
    origin_server_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    deleted_reason text,
    search_text tsvector,
    smart_query text,
    CONSTRAINT collections_membership_check CHECK ((membership = ANY (ARRAY['manual'::text, 'query'::text, 'hybrid'::text]))),
    CONSTRAINT collections_visibility_check CHECK ((visibility = ANY (ARRAY['private'::text, 'org-only'::text, 'followers'::text, 'explicit-share'::text])))
);


--
-- Name: COLUMN collections.smart_query; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.collections.smart_query IS 'DSL query string that was executed to populate this collection. Phase 1.16.B-2 writes; Phase 1.16.B-4 re-runs.';


--
-- Name: comments; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.comments (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    target_kind text NOT NULL,
    target_id uuid NOT NULL,
    parent_id uuid,
    root_id uuid NOT NULL,
    depth integer DEFAULT 0 NOT NULL,
    author_user_ref bigint,
    body text NOT NULL,
    body_html text DEFAULT ''::text NOT NULL,
    annotation_type text,
    annotation_data jsonb,
    like_count bigint DEFAULT 0 NOT NULL,
    edited_at timestamp with time zone,
    deleted_at timestamp with time zone,
    origin_server_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    peer_id uuid,
    actor_uri text,
    activity_uri text,
    CONSTRAINT comments_annotation_type_check CHECK ((annotation_type = ANY (ARRAY['point'::text, 'rect'::text, 'timestamp'::text, 'frame'::text, 'whiteboard'::text, 'text-range'::text]))),
    CONSTRAINT comments_origin_check CHECK ((((author_user_ref IS NOT NULL) AND (peer_id IS NULL) AND (actor_uri IS NULL)) OR ((author_user_ref IS NULL) AND (peer_id IS NOT NULL) AND (actor_uri IS NOT NULL)))),
    CONSTRAINT comments_target_kind_check CHECK ((target_kind = ANY (ARRAY['post'::text, 'asset'::text, 'collection'::text])))
);


--
-- Name: creative_lineage; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.creative_lineage (
    derivative_asset_id uuid NOT NULL,
    source_asset_id uuid NOT NULL,
    generation_metadata jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: direct_messages; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.direct_messages (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    sender_user_ref bigint NOT NULL,
    recipient_user_ref bigint NOT NULL,
    body text NOT NULL,
    sent_at timestamp with time zone DEFAULT now() NOT NULL,
    read_at timestamp with time zone,
    origin_server_id uuid,
    CONSTRAINT direct_messages_body_check CHECK ((length(body) > 0)),
    CONSTRAINT direct_messages_check CHECK ((sender_user_ref <> recipient_user_ref))
);


--
-- Name: email_verification_token; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.email_verification_token (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_ref bigint NOT NULL,
    token_hash bytea NOT NULL,
    purpose text DEFAULT 'register'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone,
    CONSTRAINT email_verification_token_purpose_check CHECK ((purpose = ANY (ARRAY['register'::text, 'email_change'::text])))
);


--
-- Name: extraction_failure; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.extraction_failure (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    asset_id uuid NOT NULL,
    format text NOT NULL,
    error_kind text NOT NULL,
    message text NOT NULL,
    field_key text DEFAULT ''::text NOT NULL,
    raw_value jsonb DEFAULT '{}'::jsonb NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    dismissed_at timestamp with time zone,
    CONSTRAINT extraction_failure_error_kind_check CHECK ((error_kind = ANY (ARRAY['unsupported_format'::text, 'malformed_file'::text, 'library_panic'::text, 'validation'::text, 'no_metadata'::text])))
);


--
-- Name: federation_directories; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.federation_directories (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    directory_url text NOT NULL,
    operator_name text DEFAULT ''::text NOT NULL,
    operator_public_key text NOT NULL,
    operator_fingerprint text NOT NULL,
    operator_contact text DEFAULT ''::text NOT NULL,
    subscribed_at timestamp with time zone DEFAULT now() NOT NULL,
    subscribed_by_user_ref bigint NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    last_polled_at timestamp with time zone,
    last_poll_status text DEFAULT 'never_polled'::text NOT NULL,
    last_poll_error text DEFAULT ''::text NOT NULL,
    poll_interval_seconds integer DEFAULT 21600 NOT NULL,
    notes text DEFAULT ''::text NOT NULL,
    publish_status text DEFAULT 'not_published'::text NOT NULL,
    publish_pending_token text DEFAULT ''::text NOT NULL,
    publish_token_expires_at timestamp with time zone,
    publish_record_name text DEFAULT ''::text NOT NULL,
    publish_record_value text DEFAULT ''::text NOT NULL,
    publish_listing_id text DEFAULT ''::text NOT NULL,
    publish_last_attempt_at timestamp with time zone,
    publish_last_error text DEFAULT ''::text NOT NULL,
    publish_display_name text DEFAULT ''::text NOT NULL,
    publish_region text DEFAULT ''::text NOT NULL,
    publish_description text DEFAULT ''::text NOT NULL,
    publish_tags jsonb DEFAULT '[]'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT federation_directories_last_poll_status_check CHECK ((last_poll_status = ANY (ARRAY['never_polled'::text, 'ok'::text, 'unreachable'::text, 'signature_failed'::text, 'malformed'::text, 'spec_version_mismatch'::text]))),
    CONSTRAINT federation_directories_poll_interval_seconds_check CHECK ((poll_interval_seconds >= 300)),
    CONSTRAINT federation_directories_publish_status_check CHECK ((publish_status = ANY (ARRAY['not_published'::text, 'pending_dns'::text, 'pending_register'::text, 'listed'::text, 'failed'::text])))
);


--
-- Name: federation_directory_entries; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.federation_directory_entries (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    directory_id uuid NOT NULL,
    instance_url text NOT NULL,
    display_name text NOT NULL,
    instance_public_key text NOT NULL,
    fingerprint text NOT NULL,
    region text DEFAULT ''::text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    tags jsonb DEFAULT '[]'::jsonb NOT NULL,
    verified_at timestamp with time zone NOT NULL,
    verified_via text NOT NULL,
    listing_id text DEFAULT ''::text NOT NULL,
    cached_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: federation_dispatch_state; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.federation_dispatch_state (
    id integer NOT NULL,
    last_dispatched_activity_id uuid,
    last_dispatched_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT federation_dispatch_state_id_check CHECK ((id = 1))
);


--
-- Name: TABLE federation_dispatch_state; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.federation_dispatch_state IS 'Single-row cursor for the outbox dispatcher. Atomically advanced with the outbox INSERTs in one transaction; restart picks up at the cursor without duplicates.';


--
-- Name: federation_inbox; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.federation_inbox (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    activity_uri text NOT NULL,
    peer_id uuid NOT NULL,
    actor_uri text NOT NULL,
    activity_type text NOT NULL,
    object_kind text,
    object_id uuid,
    envelope_json jsonb NOT NULL,
    http_sig_key text NOT NULL,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    reject_reason text,
    dispatch_attempts integer DEFAULT 0 NOT NULL,
    last_attempt_at timestamp with time zone,
    last_error text DEFAULT ''::text NOT NULL,
    processed_at timestamp with time zone,
    correlation_activity_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    was_encrypted boolean DEFAULT false NOT NULL,
    decrypted_with_key_version integer,
    CONSTRAINT federation_inbox_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'processed'::text, 'rejected'::text, 'failed'::text])))
);


--
-- Name: TABLE federation_inbox; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.federation_inbox IS 'One row per inbound federation activity. Pipeline ingest persists status=pending; background worker transitions to processed/rejected/failed per §2.2 of the 1.22.D design proposal. activity_uri UNIQUE is the load-bearing replay guard.';


--
-- Name: COLUMN federation_inbox.was_encrypted; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.federation_inbox.was_encrypted IS 'Phase 1.22.I-f: TRUE when the dispatcher took the decrypt branch for this row (envelope had a non-empty encryption block). FALSE for the legacy 1.22.D plaintext path. Mirrors federation_outbox.was_encrypted on the sender side.';


--
-- Name: COLUMN federation_inbox.decrypted_with_key_version; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.federation_inbox.decrypted_with_key_version IS 'Phase 1.22.I-f: which version of the receiver''s X25519 key successfully decrypted the envelope. NULL when was_encrypted=false. Surfaces rotation health to operator analytics via the I-h admin federation page.';


--
-- Name: federation_outbox; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.federation_outbox (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    activity_id uuid NOT NULL,
    peer_id uuid NOT NULL,
    target_user_url text,
    status text DEFAULT 'queued'::text NOT NULL,
    attempts smallint DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT now() NOT NULL,
    last_attempt_at timestamp with time zone,
    last_error text DEFAULT ''::text NOT NULL,
    sent_at timestamp with time zone,
    delivered_with_key_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    was_encrypted boolean DEFAULT false NOT NULL,
    sensitivity text,
    refused_reason text,
    CONSTRAINT federation_outbox_status_check CHECK ((status = ANY (ARRAY['queued'::text, 'sent'::text, 'failed'::text, 'cancelled'::text, 'refused'::text])))
);


--
-- Name: TABLE federation_outbox; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.federation_outbox IS 'Per-recipient outbound queue. Derived from activities ledger via the dispatcher (Phase 1.22.D-b). Idempotent on (activity_id, peer_id, target_user_url) so the dispatcher is re-runnable. Sender-side emission refusal for restricted/embargo content (until 1.22.I ships X25519) is enforced at the dispatcher; refused activities never get an outbox row, they emit federation.emission.skipped audit instead.';


--
-- Name: COLUMN federation_outbox.was_encrypted; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.federation_outbox.was_encrypted IS 'Phase 1.22.I-e: TRUE when the dispatcher took the encryption branch for this row (peer.Capabilities.SupportsE2E + recipient key cached). FALSE for the legacy 1.22.D plaintext path. The wire envelope is the source of truth; this column is an observability mirror for scenario 09 + the admin federation surface.';


--
-- Name: federation_peer_suggestions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.federation_peer_suggestions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    source_peer_id uuid NOT NULL,
    suggested_url text NOT NULL,
    suggested_display_name text NOT NULL,
    suggested_public_key text NOT NULL,
    suggested_fingerprint text NOT NULL,
    cached_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: federation_peers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.federation_peers (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    instance_url text NOT NULL,
    display_name text NOT NULL,
    instance_public_key text NOT NULL,
    trust_tier text DEFAULT 'connected'::text NOT NULL,
    encryption_policy text DEFAULT 'plaintext'::text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    status text DEFAULT 'connected'::text NOT NULL,
    handshake_at timestamp with time zone DEFAULT now() NOT NULL,
    handshake_by_user_ref bigint NOT NULL,
    last_seen_at timestamp with time zone,
    notes text DEFAULT ''::text NOT NULL,
    share_in_visible_list boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    capabilities jsonb DEFAULT '[]'::jsonb NOT NULL,
    capabilities_negotiated_at timestamp with time zone,
    CONSTRAINT federation_peers_encryption_policy_check CHECK ((encryption_policy = ANY (ARRAY['plaintext'::text, 'e2e-encrypted'::text]))),
    CONSTRAINT federation_peers_status_check CHECK ((status = ANY (ARRAY['pending_outbound'::text, 'pending_inbound'::text, 'connected'::text]))),
    CONSTRAINT federation_peers_trust_tier_check CHECK ((trust_tier = ANY (ARRAY['connected'::text, 'directory-listed'::text, 'auto-sync'::text])))
);


--
-- Name: COLUMN federation_peers.capabilities; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.federation_peers.capabilities IS 'Bilateral capability intersection (what BOTH peers support). JSONB array of typed strings per federation/peer.Capability. Open vocabulary on the wire, closed dispatch in code. See ADR 0049 §Track B Decision 3.';


--
-- Name: COLUMN federation_peers.capabilities_negotiated_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.federation_peers.capabilities_negotiated_at IS 'When the handshake completed with capability exchange. NULL means "never negotiated" (pre-1.22.I-d peer); peers in that state are surfaced via ListPeersMissingCapabilities for operator re-pairing. Distinct from `capabilities = ''[]''` which means "we negotiated and got an empty intersection" — also legal.';


--
-- Name: federation_remote_actors; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.federation_remote_actors (
    actor_uri text NOT NULL,
    peer_id uuid NOT NULL,
    display_name text DEFAULT ''::text NOT NULL,
    avatar_url text DEFAULT ''::text NOT NULL,
    first_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    encryption_public_key bytea,
    encryption_public_key_version integer,
    encryption_public_key_updated_at timestamp with time zone,
    CONSTRAINT federation_remote_actors_encryption_key_atomic CHECK ((((encryption_public_key IS NULL) AND (encryption_public_key_version IS NULL) AND (encryption_public_key_updated_at IS NULL)) OR ((encryption_public_key IS NOT NULL) AND (octet_length(encryption_public_key) = 32) AND (encryption_public_key_version IS NOT NULL) AND (encryption_public_key_version >= 1) AND (encryption_public_key_updated_at IS NOT NULL))))
);


--
-- Name: TABLE federation_remote_actors; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.federation_remote_actors IS 'Display cache for remote actors surfaced in UI. The inbound dispatch handler upserts on every activity from a remote actor; display fields refresh naturally on each interaction. Keyed on actor_uri (globally unique per spec §8.3).';


--
-- Name: COLUMN federation_remote_actors.encryption_public_key; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.federation_remote_actors.encryption_public_key IS 'X25519 public key advertised by the remote actor in their envelope''s aa:encryptionPublicKey block. NULL when the peer is on a pre-1.22.I-c build. 32 bytes when populated.';


--
-- Name: COLUMN federation_remote_actors.encryption_public_key_version; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.federation_remote_actors.encryption_public_key_version IS 'Per-actor version number, monotonic. Bumped by the remote side on key rotation (1.22.I-h on their end); we just observe the value + persist alongside the key bytes.';


--
-- Name: COLUMN federation_remote_actors.encryption_public_key_updated_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.federation_remote_actors.encryption_public_key_updated_at IS 'When we last observed a key for this actor. Updated on every inbound envelope carrying the block (even when the value did not change) so an operator can see how stale our knowledge is.';


--
-- Name: federation_shares; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.federation_shares (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    grantor_user_ref bigint NOT NULL,
    object_kind text NOT NULL,
    object_id uuid NOT NULL,
    peer_id uuid NOT NULL,
    target_user_url text,
    scope text DEFAULT 'view'::text NOT NULL,
    expires_at timestamp with time zone,
    notes text DEFAULT ''::text NOT NULL,
    granted_activity_id uuid NOT NULL,
    granted_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    revoked_activity_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT federation_shares_object_kind_check CHECK ((object_kind = ANY (ARRAY['asset'::text, 'post'::text, 'collection'::text, 'workspace'::text, 'brand_kit'::text, 'user'::text]))),
    CONSTRAINT federation_shares_scope_check CHECK ((scope = ANY (ARRAY['view'::text, 'comment'::text, 'annotate'::text, 'remix'::text])))
);


--
-- Name: federation_user_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.federation_user_keys (
    user_ref bigint NOT NULL,
    version integer NOT NULL,
    algorithm text DEFAULT 'naclbox-x25519-v1'::text NOT NULL,
    public_key bytea NOT NULL,
    private_key_enc bytea NOT NULL,
    is_current boolean NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    retained_until timestamp with time zone,
    rotated_at timestamp with time zone,
    rotated_by_user_ref bigint,
    CONSTRAINT federation_user_keys_current_xor_retained CHECK ((((is_current = true) AND (retained_until IS NULL)) OR ((is_current = false) AND (retained_until IS NOT NULL)))),
    CONSTRAINT federation_user_keys_private_key_enc_check CHECK ((octet_length(private_key_enc) >= 13)),
    CONSTRAINT federation_user_keys_public_key_check CHECK ((octet_length(public_key) = 32)),
    CONSTRAINT federation_user_keys_version_check CHECK ((version >= 1))
);


--
-- Name: TABLE federation_user_keys; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.federation_user_keys IS 'Per-user X25519 keypairs for NaCl-box encrypted federation. Phase 1.22.I-b. Private key column is atrest-wrapped (AES-GCM, host master key per app/internal/atrest). See ADR 0049 §Track B.';


--
-- Name: COLUMN federation_user_keys.algorithm; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.federation_user_keys.algorithm IS 'Algorithm-version token. Single value today: naclbox-x25519-v1. Future algorithms add new tokens without a schema migration.';


--
-- Name: COLUMN federation_user_keys.is_current; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.federation_user_keys.is_current IS 'Exactly one row per user has is_current=true (enforced by partial unique index federation_user_keys_one_current_idx).';


--
-- Name: COLUMN federation_user_keys.retained_until; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.federation_user_keys.retained_until IS 'Inbound-decrypt window for a rotated-aside key. NULL on the current key; NOW()+grace_period when a rotation flips this row aside.';


--
-- Name: COLUMN federation_user_keys.rotated_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.federation_user_keys.rotated_at IS 'When the rotation that produced (or flipped aside) this row occurred. NULL on pre-I-h rows. Non-NULL on the new current row AND on the previously-current row that was demoted in the same rotation.';


--
-- Name: COLUMN federation_user_keys.rotated_by_user_ref; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.federation_user_keys.rotated_by_user_ref IS 'user.ref of whoever triggered the rotation. Equals user_ref for self-rotation (/account/security); differs for admin-initiated compromised-key recovery (/admin/federation/users/{ref}/rotate-keys). NULL on pre-I-h rows.';


--
-- Name: field_definition; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.field_definition (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    code text NOT NULL,
    label text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    type text NOT NULL,
    options jsonb DEFAULT '{}'::jsonb NOT NULL,
    required boolean DEFAULT false NOT NULL,
    searchable boolean DEFAULT true NOT NULL,
    applies_to bigint[] DEFAULT '{}'::bigint[] NOT NULL,
    field_set_id uuid,
    read_capability text,
    write_capability text,
    display_order integer DEFAULT 100 NOT NULL,
    display_group text DEFAULT 'general'::text NOT NULL,
    source jsonb,
    status text DEFAULT 'active'::text NOT NULL,
    deprecated_replacement_id uuid,
    origin_server_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by_user_ref bigint,
    updated_by_user_ref bigint,
    subject_kind text DEFAULT 'asset'::text NOT NULL,
    extraction_source text DEFAULT ''::text NOT NULL,
    extraction_mode text DEFAULT 'skip_if_set'::text NOT NULL,
    CONSTRAINT field_definition_extraction_mode_check CHECK ((extraction_mode = ANY (ARRAY['skip_if_set'::text, 'replace'::text, 'append'::text, 'prepend'::text]))),
    CONSTRAINT field_definition_status_check CHECK ((status = ANY (ARRAY['active'::text, 'deprecated'::text, 'archived'::text]))),
    CONSTRAINT field_definition_subject_kind_check CHECK ((subject_kind = ANY (ARRAY['asset'::text, 'collection'::text]))),
    CONSTRAINT field_definition_type_check CHECK ((type = ANY (ARRAY['text'::text, 'longtext'::text, 'rich_text'::text, 'number'::text, 'boolean'::text, 'date'::text, 'datetime'::text, 'select'::text, 'multi_select'::text, 'tree'::text, 'reference'::text])))
);


--
-- Name: goose_db_version; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.goose_db_version (
    id integer NOT NULL,
    version_id bigint NOT NULL,
    is_applied boolean NOT NULL,
    tstamp timestamp without time zone DEFAULT now() NOT NULL
);


--
-- Name: goose_db_version_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public.goose_db_version ALTER COLUMN id ADD GENERATED BY DEFAULT AS IDENTITY (
    SEQUENCE NAME public.goose_db_version_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: jobs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.jobs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    type text NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    priority integer DEFAULT 100 NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    max_attempts integer DEFAULT 3 NOT NULL,
    claimed_by text,
    claimed_at timestamp with time zone,
    lease_expires_at timestamp with time zone,
    last_error text,
    result jsonb,
    origin_server_id uuid,
    scheduled_for timestamp with time zone,
    enqueued_at timestamp with time zone DEFAULT now() NOT NULL,
    started_at timestamp with time zone,
    finished_at timestamp with time zone,
    idempotency_key text,
    CONSTRAINT jobs_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'running'::text, 'done'::text, 'failed'::text, 'cancelled'::text])))
);


--
-- Name: likes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.likes (
    target_kind text NOT NULL,
    target_id uuid NOT NULL,
    user_ref bigint,
    liked_at timestamp with time zone DEFAULT now() NOT NULL,
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    peer_id uuid,
    actor_uri text,
    CONSTRAINT likes_origin_check CHECK ((((user_ref IS NOT NULL) AND (peer_id IS NULL) AND (actor_uri IS NULL)) OR ((user_ref IS NULL) AND (peer_id IS NOT NULL) AND (actor_uri IS NOT NULL)))),
    CONSTRAINT likes_target_kind_check CHECK ((target_kind = ANY (ARRAY['post'::text, 'asset'::text, 'comment'::text])))
);


--
-- Name: mcp_server_registration; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.mcp_server_registration (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    name text NOT NULL,
    url text NOT NULL,
    transport text DEFAULT 'http'::text NOT NULL,
    auth_kind text DEFAULT 'none'::text NOT NULL,
    auth_secret_ref text,
    auth_header_name text,
    privacy_class text DEFAULT 'cloud'::text NOT NULL,
    enabled boolean DEFAULT false NOT NULL,
    rate_limit_per_second integer DEFAULT 2 NOT NULL,
    rate_limit_per_minute integer DEFAULT 60 NOT NULL,
    health_check_interval_s integer DEFAULT 60 NOT NULL,
    last_health_check_at timestamp with time zone,
    last_health_status text,
    last_health_error text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    registered_by_user_ref bigint,
    CONSTRAINT mcp_server_registration_auth_kind_check CHECK ((auth_kind = ANY (ARRAY['none'::text, 'bearer'::text, 'header'::text, 'mtls'::text]))),
    CONSTRAINT mcp_server_registration_health_check_interval_s_check CHECK ((health_check_interval_s > 0)),
    CONSTRAINT mcp_server_registration_last_health_status_check CHECK (((last_health_status IS NULL) OR (last_health_status = ANY (ARRAY['healthy'::text, 'degraded'::text, 'unreachable'::text])))),
    CONSTRAINT mcp_server_registration_privacy_class_check CHECK ((privacy_class = ANY (ARRAY['local'::text, 'cloud'::text]))),
    CONSTRAINT mcp_server_registration_rate_limit_per_minute_check CHECK ((rate_limit_per_minute > 0)),
    CONSTRAINT mcp_server_registration_rate_limit_per_second_check CHECK ((rate_limit_per_second > 0)),
    CONSTRAINT mcp_server_registration_transport_check CHECK ((transport = ANY (ARRAY['http'::text, 'stdio'::text])))
);


--
-- Name: mcp_server_tool_grant; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.mcp_server_tool_grant (
    server_id uuid NOT NULL,
    tool_name text NOT NULL,
    additional_capability text,
    cost_estimate_micros bigint DEFAULT 0 NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    CONSTRAINT mcp_server_tool_grant_cost_estimate_micros_check CHECK ((cost_estimate_micros >= 0))
);


--
-- Name: metadata_backfill_run; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.metadata_backfill_run (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    scope jsonb DEFAULT '{}'::jsonb NOT NULL,
    total bigint DEFAULT 0 NOT NULL,
    processed bigint DEFAULT 0 NOT NULL,
    succeeded bigint DEFAULT 0 NOT NULL,
    failed bigint DEFAULT 0 NOT NULL,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    cancelled_at timestamp with time zone,
    started_by_user_ref bigint
);


--
-- Name: notifications; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.notifications (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    recipient_user_ref bigint NOT NULL,
    actor_user_ref bigint,
    verb text NOT NULL,
    target_kind text,
    target_id text,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    read_at timestamp with time zone,
    delivered_at timestamp with time zone DEFAULT now() NOT NULL,
    email_sent_at timestamp with time zone,
    origin_server_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: post_acls; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.post_acls (
    post_id uuid NOT NULL,
    principal_type text NOT NULL,
    principal_id text NOT NULL,
    permission text NOT NULL,
    granted_at timestamp with time zone DEFAULT now() NOT NULL,
    granted_by_user_ref bigint,
    expires_at timestamp with time zone,
    CONSTRAINT post_acls_permission_check CHECK ((permission = ANY (ARRAY['read'::text, 'write'::text, 'admin'::text]))),
    CONSTRAINT post_acls_principal_type_check CHECK ((principal_type = ANY (ARRAY['user'::text, 'role'::text, 'team'::text])))
);


--
-- Name: post_assets; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.post_assets (
    post_id uuid NOT NULL,
    asset_id uuid NOT NULL,
    sort_order integer DEFAULT 0 NOT NULL,
    added_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: post_tags; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.post_tags (
    post_id uuid NOT NULL,
    tag text NOT NULL
);


--
-- Name: posts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.posts (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    author_user_ref bigint NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    visibility text DEFAULT 'org-only'::text NOT NULL,
    cover_asset_id uuid,
    posted_at timestamp with time zone DEFAULT now() NOT NULL,
    like_count bigint DEFAULT 0 NOT NULL,
    comment_count bigint DEFAULT 0 NOT NULL,
    search_text tsvector,
    origin_server_id uuid,
    deleted_at timestamp with time zone,
    deleted_reason text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    state_id uuid,
    team_id uuid,
    cover_thumbnail_asset_id uuid,
    subtitle_track_override jsonb,
    CONSTRAINT posts_visibility_check CHECK ((visibility = ANY (ARRAY['private'::text, 'org-only'::text, 'followers'::text, 'explicit-share'::text])))
);


--
-- Name: COLUMN posts.subtitle_track_override; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.posts.subtitle_track_override IS 'Per-post override for the parent asset''s subtitle tracks. NULL means use the asset''s intrinsic tracks (99% case). Non-NULL JSONB carries director-cut overrides — see the subtitles package for the consumed shape. Phase 1.18.B-3.';


--
-- Name: featured_items; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.featured_items (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    subject_kind text NOT NULL,
    subject_id uuid NOT NULL,
    position integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by_user_ref bigint,
    CONSTRAINT featured_items_subject_kind_check CHECK ((subject_kind = ANY (ARRAY['asset'::text, 'collection'::text])))
);


--
-- Name: resource_request; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.resource_request (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    requester_user_ref bigint NOT NULL,
    target_asset_id uuid NOT NULL,
    requested_capability text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    decided_at timestamp with time zone,
    decided_by_user_ref bigint,
    decision_reason text DEFAULT ''::text NOT NULL,
    expires_at timestamp with time zone,
    requested_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT resource_request_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'granted'::text, 'denied'::text, 'expired'::text])))
);


--
-- Name: resource_type_ref_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public.asset_types ALTER COLUMN ref ADD GENERATED BY DEFAULT AS IDENTITY (
    SEQUENCE NAME public.resource_type_ref_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: role_capabilities; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.role_capabilities (
    role_id uuid NOT NULL,
    capability_code text NOT NULL
);


--
-- Name: roles; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.roles (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    parent_id uuid,
    name text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    origin_server_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: saved_search; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.saved_search (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    owner_user_ref bigint NOT NULL,
    name text NOT NULL,
    dsl text NOT NULL,
    notify_channel text DEFAULT 'email'::text NOT NULL,
    notify_interval_minutes integer DEFAULT 60 NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    last_result_hash text,
    last_result_ids uuid[],
    last_run_at timestamp with time zone,
    last_notified_at timestamp with time zone,
    origin_server_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT saved_search_notify_channel_check CHECK ((notify_channel = ANY (ARRAY['email'::text, 'none'::text]))),
    CONSTRAINT saved_search_notify_interval_minutes_check CHECK ((notify_interval_minutes >= 1))
);


--
-- Name: search_reindex_run; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.search_reindex_run (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    cancelled_at timestamp with time zone,
    scope jsonb DEFAULT '{}'::jsonb NOT NULL,
    target text NOT NULL,
    total_estimated bigint,
    processed bigint DEFAULT 0 NOT NULL,
    succeeded bigint DEFAULT 0 NOT NULL,
    failed bigint DEFAULT 0 NOT NULL,
    started_by_user_ref bigint,
    last_error text,
    CONSTRAINT search_reindex_run_target_check CHECK ((target = ANY (ARRAY['tsvector'::text, 'embedding'::text, 'both'::text])))
);


--
-- Name: search_visual_backfill_run; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.search_visual_backfill_run (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    cancelled_at timestamp with time zone,
    scope jsonb DEFAULT '{}'::jsonb NOT NULL,
    total_estimated bigint,
    processed bigint DEFAULT 0 NOT NULL,
    succeeded bigint DEFAULT 0 NOT NULL,
    failed bigint DEFAULT 0 NOT NULL,
    started_by_user_ref bigint,
    last_error text
);


--
-- Name: search_feedback; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.search_feedback (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    query_hash text NOT NULL,
    dsl_query text NOT NULL,
    hit_asset_id uuid NOT NULL,
    hit_position integer NOT NULL,
    direction text NOT NULL,
    user_ref bigint NOT NULL,
    ip_hash text,
    feedback_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT search_feedback_direction_check CHECK ((direction = ANY (ARRAY['up'::text, 'down'::text]))),
    CONSTRAINT search_feedback_hit_position_check CHECK ((hit_position >= 1))
);


--
-- Name: sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.sessions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_ref bigint NOT NULL,
    token_hash bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_used_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone,
    revoked_at timestamp with time zone,
    ip inet,
    user_agent text,
    origin_server_id uuid,
    impersonated_by_user_ref bigint
);


--
-- Name: storage_objects; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.storage_objects (
    hash text NOT NULL,
    size_bytes bigint NOT NULL,
    content_type text DEFAULT 'application/octet-stream'::text NOT NULL,
    backend text NOT NULL,
    backend_bucket text,
    origin_server_id uuid,
    gc_eligible_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT storage_objects_hash_check CHECK ((hash ~ '^[0-9a-f]{64}$'::text)),
    CONSTRAINT storage_objects_size_bytes_check CHECK ((size_bytes >= 0))
);


--
-- Name: storage_pins; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.storage_pins (
    object_hash text NOT NULL,
    pin_subject_type text NOT NULL,
    pin_subject_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: storage_variants; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.storage_variants (
    object_hash text NOT NULL,
    variant_key text NOT NULL,
    size_bytes bigint NOT NULL,
    content_type text DEFAULT 'application/octet-stream'::text NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT storage_variants_size_bytes_check CHECK ((size_bytes >= 0))
);


--
-- Name: system_config; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.system_config (
    key text NOT NULL,
    value jsonb DEFAULT '{}'::jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: team_closure; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.team_closure (
    ancestor_id uuid NOT NULL,
    descendant_id uuid NOT NULL,
    depth integer NOT NULL
);


--
-- Name: team_memberships; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.team_memberships (
    team_id uuid NOT NULL,
    user_ref bigint NOT NULL,
    added_at timestamp with time zone DEFAULT now() NOT NULL,
    added_by_user_ref bigint
);


--
-- Name: team_parents; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.team_parents (
    child_id uuid NOT NULL,
    parent_id uuid NOT NULL,
    CONSTRAINT team_parents_check CHECK ((child_id <> parent_id))
);


--
-- Name: teams; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.teams (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    slug text NOT NULL,
    name text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    origin_server_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone
);


--
-- Name: user; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."user" (
    ref bigint NOT NULL,
    username character varying(50),
    password character varying(255),
    fullname character varying(100),
    email character varying(100),
    usergroup bigint,
    last_active timestamp with time zone,
    account_expires timestamp with time zone,
    comments text,
    password_last_change timestamp with time zone,
    approved bigint DEFAULT 1 NOT NULL,
    lang character varying(11),
    created timestamp with time zone DEFAULT now(),
    password_reset_hash character varying(100),
    origin character varying(50),
    actor_uri text,
    signing_public_key_pem text,
    signing_private_key_enc bytea,
    encryption_public_key bytea,
    encryption_private_key_enc bytea,
    email_verified_at timestamp with time zone,
    failed_login_count integer DEFAULT 0 NOT NULL,
    lockout_until timestamp with time zone,
    CONSTRAINT user_approved_check CHECK ((approved = ANY (ARRAY[(0)::bigint, (1)::bigint, (2)::bigint, (3)::bigint])))
);


--
-- Name: user_blocks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_blocks (
    blocker_user_ref bigint NOT NULL,
    blocked_user_ref bigint NOT NULL,
    reason text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    origin_server_id uuid,
    CONSTRAINT user_blocks_check CHECK ((blocker_user_ref <> blocked_user_ref))
);


--
-- Name: user_capability_grants; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_capability_grants (
    user_ref bigint NOT NULL,
    capability_code text NOT NULL,
    granted_at timestamp with time zone DEFAULT now() NOT NULL,
    granted_by_user_ref bigint,
    note text DEFAULT ''::text NOT NULL,
    team_id uuid,
    expires_at timestamp with time zone,
    request_ref uuid
);


--
-- Name: user_capability_revokes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_capability_revokes (
    user_ref bigint NOT NULL,
    capability_code text NOT NULL,
    revoked_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_by_user_ref bigint,
    note text DEFAULT ''::text NOT NULL,
    team_id uuid,
    expires_at timestamp with time zone
);


--
-- Name: user_follows; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_follows (
    follower_user_ref bigint NOT NULL,
    followee_user_ref bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    origin_server_id uuid,
    CONSTRAINT user_follows_check CHECK ((follower_user_ref <> followee_user_ref))
);


--
-- Name: user_password_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_password_history (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_ref bigint NOT NULL,
    password_hash character varying(255) NOT NULL,
    changed_at timestamp with time zone DEFAULT now() NOT NULL,
    origin_server_id uuid
);


--
-- Name: user_preferences; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_preferences (
    user_ref bigint NOT NULL,
    notification_channels jsonb DEFAULT '{}'::jsonb NOT NULL,
    default_views jsonb DEFAULT '{}'::jsonb NOT NULL,
    origin_server_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    email_cadence jsonb DEFAULT '{}'::jsonb NOT NULL
);


--
-- Name: digest_queue; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.digest_queue (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_ref bigint NOT NULL,
    topic text NOT NULL,
    cadence text NOT NULL,
    notification_id uuid NOT NULL,
    queued_at timestamp with time zone DEFAULT now() NOT NULL,
    sent_at timestamp with time zone,
    CONSTRAINT digest_queue_cadence_check CHECK ((cadence = ANY (ARRAY['hourly'::text, 'daily'::text, 'weekly'::text])))
);


--
-- Name: user_profiles; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_profiles (
    user_ref bigint NOT NULL,
    display_name text,
    bio text DEFAULT ''::text NOT NULL,
    avatar_url text,
    location text DEFAULT ''::text NOT NULL,
    website_url text,
    social_links jsonb DEFAULT '{}'::jsonb NOT NULL,
    origin_server_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    language text DEFAULT ''::text NOT NULL,
    theme text DEFAULT ''::text NOT NULL,
    CONSTRAINT user_profiles_theme_check CHECK ((theme = ANY (ARRAY[''::text, 'light'::text, 'dark'::text])))
);


--
-- Name: user_ref_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public."user" ALTER COLUMN ref ADD GENERATED BY DEFAULT AS IDENTITY (
    SEQUENCE NAME public.user_ref_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: user_roles; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_roles (
    user_ref bigint NOT NULL,
    role_id uuid NOT NULL,
    assigned_at timestamp with time zone DEFAULT now() NOT NULL,
    assigned_by_user_ref bigint,
    team_id uuid
);


--
-- Name: user_totp; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_totp (
    user_ref bigint NOT NULL,
    secret_enc bytea NOT NULL,
    confirmed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_used_at timestamp with time zone
);


--
-- Name: user_totp_recovery_code; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_totp_recovery_code (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_ref bigint NOT NULL,
    code_hash bytea NOT NULL,
    used_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: workflow_audit; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workflow_audit (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    resource_kind text NOT NULL,
    resource_id uuid NOT NULL,
    from_state_id uuid,
    to_state_id uuid NOT NULL,
    actor_user_ref bigint,
    note text DEFAULT ''::text NOT NULL,
    transitioned_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: workflow_states; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workflow_states (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    domain text NOT NULL,
    code text NOT NULL,
    label text NOT NULL,
    sort_order integer DEFAULT 0 NOT NULL,
    is_initial boolean DEFAULT false NOT NULL,
    is_terminal boolean DEFAULT false NOT NULL,
    visible_by_default boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    icon text DEFAULT ''::text NOT NULL,
    color text DEFAULT ''::text NOT NULL,
    requires_note boolean DEFAULT false NOT NULL
);


--
-- Name: workflow_transitions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workflow_transitions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    from_state_id uuid,
    to_state_id uuid NOT NULL,
    required_capability text,
    requires_team_scope boolean DEFAULT false NOT NULL
);


--
-- Name: activities activities_activity_uri_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.activities
    ADD CONSTRAINT activities_activity_uri_key UNIQUE (activity_uri);


--
-- Name: activities activities_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.activities
    ADD CONSTRAINT activities_pkey PRIMARY KEY (id);


--
-- Name: ai_provider_call ai_provider_call_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ai_provider_call
    ADD CONSTRAINT ai_provider_call_pkey PRIMARY KEY (id);


--
-- Name: api_tokens api_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_tokens
    ADD CONSTRAINT api_tokens_pkey PRIMARY KEY (id);


--
-- Name: api_tokens api_tokens_token_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_tokens
    ADD CONSTRAINT api_tokens_token_hash_key UNIQUE (token_hash);


--
-- Name: asset_alternates asset_alternates_asset_id_label_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_alternates
    ADD CONSTRAINT asset_alternates_asset_id_label_key UNIQUE (asset_id, label);


--
-- Name: asset_alternates asset_alternates_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_alternates
    ADD CONSTRAINT asset_alternates_pkey PRIMARY KEY (id);


--
-- Name: asset_companions asset_companions_asset_id_companion_path_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_companions
    ADD CONSTRAINT asset_companions_asset_id_companion_path_key UNIQUE (asset_id, companion_path);


--
-- Name: asset_companions asset_companions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_companions
    ADD CONSTRAINT asset_companions_pkey PRIMARY KEY (id);


--
-- Name: asset_embedding_d768 asset_embedding_d768_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_embedding_d768
    ADD CONSTRAINT asset_embedding_d768_pkey PRIMARY KEY (asset_id, provider, model, modality);


--
-- Name: asset_field_value_history asset_field_value_history_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_field_value_history
    ADD CONSTRAINT asset_field_value_history_pkey PRIMARY KEY (id);


--
-- Name: asset_field_value asset_field_value_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_field_value
    ADD CONSTRAINT asset_field_value_pkey PRIMARY KEY (asset_id, field_id);


--
-- Name: asset_subtitle_tracks asset_subtitle_tracks_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_subtitle_tracks
    ADD CONSTRAINT asset_subtitle_tracks_pkey PRIMARY KEY (asset_id, lang);


--
-- Name: asset_tag asset_tag_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_tag
    ADD CONSTRAINT asset_tag_pkey PRIMARY KEY (asset_id, tag);


--
-- Name: asset_type_acls asset_type_acls_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_type_acls
    ADD CONSTRAINT asset_type_acls_pkey PRIMARY KEY (asset_type_ref, principal_type, principal_id, permission);


--
-- Name: assets assets_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.assets
    ADD CONSTRAINT assets_pkey PRIMARY KEY (id);


--
-- Name: audit_events audit_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audit_events
    ADD CONSTRAINT audit_events_pkey PRIMARY KEY (id);


--
-- Name: brush_pack_stamps brush_pack_stamps_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.brush_pack_stamps
    ADD CONSTRAINT brush_pack_stamps_pkey PRIMARY KEY (id);


--
-- Name: brush_packs brush_packs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.brush_packs
    ADD CONSTRAINT brush_packs_pkey PRIMARY KEY (id);


--
-- Name: capabilities capabilities_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.capabilities
    ADD CONSTRAINT capabilities_pkey PRIMARY KEY (code);


--
-- Name: collection_acls collection_acls_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.collection_acls
    ADD CONSTRAINT collection_acls_pkey PRIMARY KEY (collection_id, principal_type, principal_id, permission);


--
-- Name: collection_field_value_history collection_field_value_history_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.collection_field_value_history
    ADD CONSTRAINT collection_field_value_history_pkey PRIMARY KEY (id);


--
-- Name: collection_field_value collection_field_value_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.collection_field_value
    ADD CONSTRAINT collection_field_value_pkey PRIMARY KEY (collection_id, field_id);


--
-- Name: collection_posts collection_posts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.collection_posts
    ADD CONSTRAINT collection_posts_pkey PRIMARY KEY (collection_id, post_id);


--
-- Name: collection_resources collection_resources_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.collection_resources
    ADD CONSTRAINT collection_resources_pkey PRIMARY KEY (collection_id, asset_id);


--
-- Name: collections collections_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.collections
    ADD CONSTRAINT collections_pkey PRIMARY KEY (id);


--
-- Name: comments comments_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.comments
    ADD CONSTRAINT comments_pkey PRIMARY KEY (id);


--
-- Name: creative_lineage creative_lineage_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.creative_lineage
    ADD CONSTRAINT creative_lineage_pkey PRIMARY KEY (derivative_asset_id);


--
-- Name: direct_messages direct_messages_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.direct_messages
    ADD CONSTRAINT direct_messages_pkey PRIMARY KEY (id);


--
-- Name: email_verification_token email_verification_token_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.email_verification_token
    ADD CONSTRAINT email_verification_token_pkey PRIMARY KEY (id);


--
-- Name: email_verification_token email_verification_token_token_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.email_verification_token
    ADD CONSTRAINT email_verification_token_token_hash_key UNIQUE (token_hash);


--
-- Name: extraction_failure extraction_failure_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.extraction_failure
    ADD CONSTRAINT extraction_failure_pkey PRIMARY KEY (id);


--
-- Name: federation_directories federation_directories_directory_url_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_directories
    ADD CONSTRAINT federation_directories_directory_url_key UNIQUE (directory_url);


--
-- Name: federation_directories federation_directories_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_directories
    ADD CONSTRAINT federation_directories_pkey PRIMARY KEY (id);


--
-- Name: federation_directory_entries federation_directory_entries_directory_id_instance_url_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_directory_entries
    ADD CONSTRAINT federation_directory_entries_directory_id_instance_url_key UNIQUE (directory_id, instance_url);


--
-- Name: federation_directory_entries federation_directory_entries_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_directory_entries
    ADD CONSTRAINT federation_directory_entries_pkey PRIMARY KEY (id);


--
-- Name: federation_dispatch_state federation_dispatch_state_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_dispatch_state
    ADD CONSTRAINT federation_dispatch_state_pkey PRIMARY KEY (id);


--
-- Name: federation_inbox federation_inbox_activity_uri_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_inbox
    ADD CONSTRAINT federation_inbox_activity_uri_key UNIQUE (activity_uri);


--
-- Name: federation_inbox federation_inbox_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_inbox
    ADD CONSTRAINT federation_inbox_pkey PRIMARY KEY (id);


--
-- Name: federation_outbox federation_outbox_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_outbox
    ADD CONSTRAINT federation_outbox_pkey PRIMARY KEY (id);


--
-- Name: federation_peer_suggestions federation_peer_suggestions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_peer_suggestions
    ADD CONSTRAINT federation_peer_suggestions_pkey PRIMARY KEY (id);


--
-- Name: federation_peer_suggestions federation_peer_suggestions_source_peer_id_suggested_url_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_peer_suggestions
    ADD CONSTRAINT federation_peer_suggestions_source_peer_id_suggested_url_key UNIQUE (source_peer_id, suggested_url);


--
-- Name: federation_peers federation_peers_instance_url_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_peers
    ADD CONSTRAINT federation_peers_instance_url_key UNIQUE (instance_url);


--
-- Name: federation_peers federation_peers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_peers
    ADD CONSTRAINT federation_peers_pkey PRIMARY KEY (id);


--
-- Name: federation_remote_actors federation_remote_actors_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_remote_actors
    ADD CONSTRAINT federation_remote_actors_pkey PRIMARY KEY (actor_uri);


--
-- Name: federation_shares federation_shares_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_shares
    ADD CONSTRAINT federation_shares_pkey PRIMARY KEY (id);


--
-- Name: federation_user_keys federation_user_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_user_keys
    ADD CONSTRAINT federation_user_keys_pkey PRIMARY KEY (user_ref, version);


--
-- Name: field_definition field_definition_code_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.field_definition
    ADD CONSTRAINT field_definition_code_key UNIQUE (code);


--
-- Name: field_definition field_definition_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.field_definition
    ADD CONSTRAINT field_definition_pkey PRIMARY KEY (id);


--
-- Name: goose_db_version goose_db_version_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.goose_db_version
    ADD CONSTRAINT goose_db_version_pkey PRIMARY KEY (id);


--
-- Name: jobs jobs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_pkey PRIMARY KEY (id);


--
-- Name: likes likes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.likes
    ADD CONSTRAINT likes_pkey PRIMARY KEY (id);


--
-- Name: mcp_server_registration mcp_server_registration_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mcp_server_registration
    ADD CONSTRAINT mcp_server_registration_name_key UNIQUE (name);


--
-- Name: mcp_server_registration mcp_server_registration_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mcp_server_registration
    ADD CONSTRAINT mcp_server_registration_pkey PRIMARY KEY (id);


--
-- Name: mcp_server_tool_grant mcp_server_tool_grant_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mcp_server_tool_grant
    ADD CONSTRAINT mcp_server_tool_grant_pkey PRIMARY KEY (server_id, tool_name);


--
-- Name: metadata_backfill_run metadata_backfill_run_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.metadata_backfill_run
    ADD CONSTRAINT metadata_backfill_run_pkey PRIMARY KEY (id);


--
-- Name: notifications notifications_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.notifications
    ADD CONSTRAINT notifications_pkey PRIMARY KEY (id);


--
-- Name: post_acls post_acls_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.post_acls
    ADD CONSTRAINT post_acls_pkey PRIMARY KEY (post_id, principal_type, principal_id, permission);


--
-- Name: post_assets post_assets_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.post_assets
    ADD CONSTRAINT post_assets_pkey PRIMARY KEY (post_id, asset_id);


--
-- Name: post_tags post_tags_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.post_tags
    ADD CONSTRAINT post_tags_pkey PRIMARY KEY (post_id, tag);


--
-- Name: posts posts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.posts
    ADD CONSTRAINT posts_pkey PRIMARY KEY (id);


--
-- Name: featured_items featured_items_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.featured_items
    ADD CONSTRAINT featured_items_pkey PRIMARY KEY (id);


--
-- Name: featured_items featured_items_subject_unique; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.featured_items
    ADD CONSTRAINT featured_items_subject_unique UNIQUE (subject_kind, subject_id);


--
-- Name: resource_request resource_request_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.resource_request
    ADD CONSTRAINT resource_request_pkey PRIMARY KEY (id);


--
-- Name: asset_types resource_type_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_types
    ADD CONSTRAINT resource_type_pkey PRIMARY KEY (ref);


--
-- Name: role_capabilities role_capabilities_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.role_capabilities
    ADD CONSTRAINT role_capabilities_pkey PRIMARY KEY (role_id, capability_code);


--
-- Name: roles roles_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.roles
    ADD CONSTRAINT roles_name_key UNIQUE (name);


--
-- Name: roles roles_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.roles
    ADD CONSTRAINT roles_pkey PRIMARY KEY (id);


--
-- Name: saved_search saved_search_owner_user_ref_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.saved_search
    ADD CONSTRAINT saved_search_owner_user_ref_name_key UNIQUE (owner_user_ref, name);


--
-- Name: saved_search saved_search_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.saved_search
    ADD CONSTRAINT saved_search_pkey PRIMARY KEY (id);


--
-- Name: search_feedback search_feedback_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.search_feedback
    ADD CONSTRAINT search_feedback_pkey PRIMARY KEY (id);


--
-- Name: search_feedback search_feedback_user_ref_hit_asset_query_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.search_feedback
    ADD CONSTRAINT search_feedback_user_ref_hit_asset_query_hash_key UNIQUE (user_ref, hit_asset_id, query_hash);


--
-- Name: search_reindex_run search_reindex_run_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.search_reindex_run
    ADD CONSTRAINT search_reindex_run_pkey PRIMARY KEY (id);


--
-- Name: search_visual_backfill_run search_visual_backfill_run_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.search_visual_backfill_run
    ADD CONSTRAINT search_visual_backfill_run_pkey PRIMARY KEY (id);


--
-- Name: sessions sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (id);


--
-- Name: sessions sessions_token_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_token_hash_key UNIQUE (token_hash);


--
-- Name: storage_objects storage_objects_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_objects
    ADD CONSTRAINT storage_objects_pkey PRIMARY KEY (hash);


--
-- Name: storage_pins storage_pins_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_pins
    ADD CONSTRAINT storage_pins_pkey PRIMARY KEY (object_hash, pin_subject_type, pin_subject_id);


--
-- Name: storage_variants storage_variants_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_variants
    ADD CONSTRAINT storage_variants_pkey PRIMARY KEY (object_hash, variant_key);


--
-- Name: system_config system_config_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.system_config
    ADD CONSTRAINT system_config_pkey PRIMARY KEY (key);


--
-- Name: team_closure team_closure_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_closure
    ADD CONSTRAINT team_closure_pkey PRIMARY KEY (ancestor_id, descendant_id);


--
-- Name: team_memberships team_memberships_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_memberships
    ADD CONSTRAINT team_memberships_pkey PRIMARY KEY (team_id, user_ref);


--
-- Name: team_parents team_parents_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_parents
    ADD CONSTRAINT team_parents_pkey PRIMARY KEY (child_id, parent_id);


--
-- Name: teams teams_origin_server_id_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.teams
    ADD CONSTRAINT teams_origin_server_id_slug_key UNIQUE NULLS NOT DISTINCT (origin_server_id, slug);


--
-- Name: teams teams_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.teams
    ADD CONSTRAINT teams_pkey PRIMARY KEY (id);


--
-- Name: user_blocks user_blocks_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_blocks
    ADD CONSTRAINT user_blocks_pkey PRIMARY KEY (blocker_user_ref, blocked_user_ref);


--
-- Name: user_capability_grants user_capability_grants_unique; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_capability_grants
    ADD CONSTRAINT user_capability_grants_unique UNIQUE NULLS NOT DISTINCT (user_ref, capability_code, team_id);


--
-- Name: user_capability_revokes user_capability_revokes_unique; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_capability_revokes
    ADD CONSTRAINT user_capability_revokes_unique UNIQUE NULLS NOT DISTINCT (user_ref, capability_code, team_id);


--
-- Name: user_follows user_follows_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_follows
    ADD CONSTRAINT user_follows_pkey PRIMARY KEY (follower_user_ref, followee_user_ref);


--
-- Name: user_password_history user_password_history_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_password_history
    ADD CONSTRAINT user_password_history_pkey PRIMARY KEY (id);


--
-- Name: user user_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."user"
    ADD CONSTRAINT user_pkey PRIMARY KEY (ref);


--
-- Name: user_preferences user_preferences_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_preferences
    ADD CONSTRAINT user_preferences_pkey PRIMARY KEY (user_ref);


--
-- Name: digest_queue digest_queue_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.digest_queue
    ADD CONSTRAINT digest_queue_pkey PRIMARY KEY (id);


--
-- Name: digest_queue digest_queue_user_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.digest_queue
    ADD CONSTRAINT digest_queue_user_ref_fkey FOREIGN KEY (user_ref) REFERENCES public."user"(ref) ON DELETE CASCADE;


--
-- Name: digest_queue digest_queue_notification_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.digest_queue
    ADD CONSTRAINT digest_queue_notification_id_fkey FOREIGN KEY (notification_id) REFERENCES public.notifications(id) ON DELETE CASCADE;


--
-- Name: digest_queue_pending_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX digest_queue_pending_idx ON public.digest_queue USING btree (cadence, user_ref) WHERE (sent_at IS NULL);


--
-- Name: user_profiles user_profiles_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_profiles
    ADD CONSTRAINT user_profiles_pkey PRIMARY KEY (user_ref);


--
-- Name: user_roles user_roles_unique; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_roles
    ADD CONSTRAINT user_roles_unique UNIQUE NULLS NOT DISTINCT (user_ref, role_id, team_id);


--
-- Name: user_totp user_totp_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_totp
    ADD CONSTRAINT user_totp_pkey PRIMARY KEY (user_ref);


--
-- Name: user_totp_recovery_code user_totp_recovery_code_code_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_totp_recovery_code
    ADD CONSTRAINT user_totp_recovery_code_code_hash_key UNIQUE (code_hash);


--
-- Name: user_totp_recovery_code user_totp_recovery_code_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_totp_recovery_code
    ADD CONSTRAINT user_totp_recovery_code_pkey PRIMARY KEY (id);


--
-- Name: workflow_audit workflow_audit_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workflow_audit
    ADD CONSTRAINT workflow_audit_pkey PRIMARY KEY (id);


--
-- Name: workflow_states workflow_states_domain_code_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workflow_states
    ADD CONSTRAINT workflow_states_domain_code_key UNIQUE (domain, code);


--
-- Name: workflow_states workflow_states_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workflow_states
    ADD CONSTRAINT workflow_states_pkey PRIMARY KEY (id);


--
-- Name: workflow_transitions workflow_transitions_from_state_id_to_state_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workflow_transitions
    ADD CONSTRAINT workflow_transitions_from_state_id_to_state_id_key UNIQUE NULLS NOT DISTINCT (from_state_id, to_state_id);


--
-- Name: workflow_transitions workflow_transitions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workflow_transitions
    ADD CONSTRAINT workflow_transitions_pkey PRIMARY KEY (id);


--
-- Name: activities_actor_outbox_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX activities_actor_outbox_idx ON public.activities USING btree (actor_user_ref, published_at DESC, id DESC) WHERE ((source = 'local'::text) AND (actor_user_ref IS NOT NULL));


--
-- Name: activities_object_recent_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX activities_object_recent_idx ON public.activities USING btree (object_kind, object_local_id, published_at DESC) WHERE ((object_kind IS NOT NULL) AND (object_local_id IS NOT NULL));


--
-- Name: activities_source_recent_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX activities_source_recent_idx ON public.activities USING btree (source, published_at DESC) WHERE (source <> 'local'::text);


--
-- Name: activities_type_recent_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX activities_type_recent_idx ON public.activities USING btree (activity_type, published_at DESC);


--
-- Name: afvh_asset_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX afvh_asset_idx ON public.asset_field_value_history USING btree (asset_id, changed_at DESC);


--
-- Name: afvh_field_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX afvh_field_idx ON public.asset_field_value_history USING btree (field_id, changed_at DESC);


--
-- Name: api_tokens__active_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX api_tokens__active_idx ON public.api_tokens USING btree (token_hash) WHERE (revoked_at IS NULL);


--
-- Name: api_tokens__user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX api_tokens__user_idx ON public.api_tokens USING btree (user_ref);


--
-- Name: asset_alternates__asset_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX asset_alternates__asset_idx ON public.asset_alternates USING btree (asset_id);


--
-- Name: asset_alternates__hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX asset_alternates__hash_idx ON public.asset_alternates USING btree (object_hash);


--
-- Name: asset_alternates__kind_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX asset_alternates__kind_idx ON public.asset_alternates USING btree (asset_id, kind);


--
-- Name: asset_companions__asset_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX asset_companions__asset_idx ON public.asset_companions USING btree (asset_id);


--
-- Name: asset_companions__hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX asset_companions__hash_idx ON public.asset_companions USING btree (object_hash);


--
-- Name: asset_field_value_asset_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX asset_field_value_asset_idx ON public.asset_field_value USING btree (asset_id);


--
-- Name: asset_field_value_date_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX asset_field_value_date_idx ON public.asset_field_value USING btree (field_id, value_date) WHERE (value_date IS NOT NULL);


--
-- Name: asset_field_value_field_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX asset_field_value_field_idx ON public.asset_field_value USING btree (field_id);


--
-- Name: asset_field_value_num_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX asset_field_value_num_idx ON public.asset_field_value USING btree (field_id, value_num) WHERE (value_num IS NOT NULL);


--
-- Name: asset_field_value_options_gin; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX asset_field_value_options_gin ON public.asset_field_value USING gin (value_options) WHERE (value_options IS NOT NULL);


--
-- Name: asset_field_value_ref_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX asset_field_value_ref_idx ON public.asset_field_value USING btree (value_ref) WHERE (value_ref IS NOT NULL);


--
-- Name: asset_field_value_text_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX asset_field_value_text_idx ON public.asset_field_value USING btree (field_id, value_text) WHERE (value_text IS NOT NULL);


--
-- Name: asset_tag_tag_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX asset_tag_tag_idx ON public.asset_tag USING btree (tag);


--
-- Name: asset_type_acls_expires_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX asset_type_acls_expires_idx ON public.asset_type_acls USING btree (expires_at) WHERE (expires_at IS NOT NULL);


--
-- Name: asset_type_acls_principal_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX asset_type_acls_principal_idx ON public.asset_type_acls USING btree (principal_type, principal_id);


--
-- Name: assets_created_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX assets_created_at_idx ON public.assets USING btree (created_at DESC) WHERE (deleted_at IS NULL);


--
-- Name: assets_file_hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX assets_file_hash_idx ON public.assets USING btree (file_hash) WHERE (file_hash IS NOT NULL);


--
-- Name: assets_metadata_gin; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX assets_metadata_gin ON public.assets USING gin (metadata);


--
-- Name: assets_owner_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX assets_owner_idx ON public.assets USING btree (owner_user_ref) WHERE ((deleted_at IS NULL) AND (owner_user_ref IS NOT NULL));


--
-- Name: assets_processing_status_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX assets_processing_status_idx ON public.assets USING btree (processing_status) WHERE (processing_status <> 'ready'::text);


--
-- Name: assets_search_text_gin; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX assets_search_text_gin ON public.assets USING gin (search_text) WHERE (deleted_at IS NULL);


--
-- Name: assets_state_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX assets_state_idx ON public.assets USING btree (state_id) WHERE (state_id IS NOT NULL);


--
-- Name: assets_status_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX assets_status_idx ON public.assets USING btree (status) WHERE (deleted_at IS NULL);


--
-- Name: assets_team_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX assets_team_idx ON public.assets USING btree (team_id) WHERE (team_id IS NOT NULL);


--
-- Name: assets_title_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX assets_title_trgm ON public.assets USING gin (title public.gin_trgm_ops) WHERE ((deleted_at IS NULL) AND (title <> ''::text));


--
-- Name: assets_type_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX assets_type_idx ON public.assets USING btree (asset_type) WHERE (deleted_at IS NULL);


--
-- Name: audit_events__subject_time_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX audit_events__subject_time_idx ON public.audit_events USING btree (subject_user_ref, occurred_at DESC) WHERE (subject_user_ref IS NOT NULL);


--
-- Name: audit_events__type_time_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX audit_events__type_time_idx ON public.audit_events USING btree (event_type, occurred_at DESC);


--
-- Name: brush_pack_stamps_pack_abr_uniq; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX brush_pack_stamps_pack_abr_uniq ON public.brush_pack_stamps USING btree (pack_id, abr_id) WHERE (abr_id IS NOT NULL);


--
-- Name: brush_pack_stamps_pack_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX brush_pack_stamps_pack_idx ON public.brush_pack_stamps USING btree (pack_id);


--
-- Name: brush_packs_owner_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX brush_packs_owner_idx ON public.brush_packs USING btree (owner_user_ref);


--
-- Name: cfvh_collection_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cfvh_collection_idx ON public.collection_field_value_history USING btree (collection_id, changed_at DESC);


--
-- Name: cfvh_field_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cfvh_field_idx ON public.collection_field_value_history USING btree (field_id, changed_at DESC);


--
-- Name: collection_acls_expires_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX collection_acls_expires_idx ON public.collection_acls USING btree (expires_at) WHERE (expires_at IS NOT NULL);


--
-- Name: collection_acls_principal_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX collection_acls_principal_idx ON public.collection_acls USING btree (principal_type, principal_id);


--
-- Name: collection_posts_expires_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX collection_posts_expires_idx ON public.collection_posts USING btree (expires_at) WHERE (expires_at IS NOT NULL);


--
-- Name: collection_posts_post_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX collection_posts_post_idx ON public.collection_posts USING btree (post_id);


--
-- Name: collection_posts_sort_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX collection_posts_sort_idx ON public.collection_posts USING btree (collection_id, sort_order, added_at);


--
-- Name: collection_resources_asset_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX collection_resources_asset_idx ON public.collection_resources USING btree (asset_id);


--
-- Name: collection_resources_expires_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX collection_resources_expires_idx ON public.collection_resources USING btree (expires_at) WHERE (expires_at IS NOT NULL);


--
-- Name: collection_resources_sort_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX collection_resources_sort_idx ON public.collection_resources USING btree (collection_id, sort_order);


--
-- Name: collections_created_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX collections_created_at_idx ON public.collections USING btree (created_at DESC);


--
-- Name: collections_expires_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX collections_expires_idx ON public.collections USING btree (expires_at) WHERE (expires_at IS NOT NULL);


--
-- Name: collections_featured_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX collections_featured_idx ON public.collections USING btree (featured) WHERE featured;


--
-- Name: collections_name_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX collections_name_trgm ON public.collections USING gin (name public.gin_trgm_ops) WHERE (name <> ''::text);


--
-- Name: collections_owner_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX collections_owner_idx ON public.collections USING btree (owner_user_ref);


--
-- Name: collections_search_text_gin; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX collections_search_text_gin ON public.collections USING gin (search_text);


--
-- Name: collections_visibility_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX collections_visibility_idx ON public.collections USING btree (visibility);


--
-- Name: comments_activity_uri_uniq_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX comments_activity_uri_uniq_idx ON public.comments USING btree (activity_uri) WHERE (activity_uri IS NOT NULL);


--
-- Name: comments_annotation_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX comments_annotation_idx ON public.comments USING btree (target_kind, target_id, annotation_type) WHERE ((annotation_type IS NOT NULL) AND (deleted_at IS NULL));


--
-- Name: comments_author_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX comments_author_idx ON public.comments USING btree (author_user_ref, created_at DESC) WHERE (deleted_at IS NULL);


--
-- Name: comments_target_active_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX comments_target_active_idx ON public.comments USING btree (target_kind, target_id, created_at DESC) WHERE (deleted_at IS NULL);


--
-- Name: comments_text_annotations_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX comments_text_annotations_idx ON public.comments USING btree (target_kind, target_id, created_at DESC) WHERE ((annotation_type = 'text-range'::text) AND (deleted_at IS NULL));


--
-- Name: comments_thread_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX comments_thread_idx ON public.comments USING btree (root_id, depth, created_at) WHERE (deleted_at IS NULL);


--
-- Name: comments_whiteboards_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX comments_whiteboards_idx ON public.comments USING btree (target_kind, target_id, created_at DESC) WHERE ((annotation_type = 'whiteboard'::text) AND (deleted_at IS NULL));


--
-- Name: federation_directories_due_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_directories_due_idx ON public.federation_directories USING btree (last_polled_at NULLS FIRST) WHERE (enabled = true);


--
-- Name: federation_directory_entries_by_dir_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_directory_entries_by_dir_idx ON public.federation_directory_entries USING btree (directory_id, verified_at DESC);


--
-- Name: federation_directory_entries_by_url_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_directory_entries_by_url_idx ON public.federation_directory_entries USING btree (instance_url);


--
-- Name: federation_inbox_by_peer_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_inbox_by_peer_idx ON public.federation_inbox USING btree (peer_id, received_at DESC);


--
-- Name: federation_inbox_by_status_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_inbox_by_status_idx ON public.federation_inbox USING btree (status, received_at DESC);


--
-- Name: federation_inbox_pending_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_inbox_pending_idx ON public.federation_inbox USING btree (received_at) WHERE (status = 'pending'::text);


--
-- Name: federation_outbox_by_peer_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_outbox_by_peer_idx ON public.federation_outbox USING btree (peer_id, created_at DESC);


--
-- Name: federation_outbox_dedup_broadcast_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX federation_outbox_dedup_broadcast_idx ON public.federation_outbox USING btree (activity_id, peer_id) WHERE (target_user_url IS NULL);


--
-- Name: federation_outbox_dedup_targeted_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX federation_outbox_dedup_targeted_idx ON public.federation_outbox USING btree (activity_id, peer_id, target_user_url) WHERE (target_user_url IS NOT NULL);


--
-- Name: federation_outbox_due_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_outbox_due_idx ON public.federation_outbox USING btree (next_attempt_at) WHERE (status = 'queued'::text);


--
-- Name: federation_peer_suggestions_by_source_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_peer_suggestions_by_source_idx ON public.federation_peer_suggestions USING btree (source_peer_id, cached_at DESC);


--
-- Name: federation_peer_suggestions_by_url_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_peer_suggestions_by_url_idx ON public.federation_peer_suggestions USING btree (suggested_url);


--
-- Name: federation_peers_enabled_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_peers_enabled_idx ON public.federation_peers USING btree (instance_url) WHERE ((enabled = true) AND (status = 'connected'::text));


--
-- Name: federation_peers_handshake_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_peers_handshake_at_idx ON public.federation_peers USING btree (handshake_at DESC);


--
-- Name: federation_peers_pending_inbound_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_peers_pending_inbound_idx ON public.federation_peers USING btree (handshake_at DESC) WHERE (status = 'pending_inbound'::text);


--
-- Name: federation_peers_unnegotiated_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_peers_unnegotiated_idx ON public.federation_peers USING btree (id) WHERE (capabilities_negotiated_at IS NULL);


--
-- Name: federation_peers_visible_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_peers_visible_idx ON public.federation_peers USING btree (instance_url) WHERE ((enabled = true) AND (status = 'connected'::text) AND (share_in_visible_list = true));


--
-- Name: federation_remote_actors_by_peer_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_remote_actors_by_peer_idx ON public.federation_remote_actors USING btree (peer_id, last_seen_at DESC);


--
-- Name: federation_remote_actors_missing_encryption_key_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_remote_actors_missing_encryption_key_idx ON public.federation_remote_actors USING btree (peer_id) WHERE (encryption_public_key IS NULL);


--
-- Name: federation_shares_active_uniq_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX federation_shares_active_uniq_idx ON public.federation_shares USING btree (grantor_user_ref, object_kind, object_id, peer_id, COALESCE(target_user_url, ''::text)) WHERE (revoked_at IS NULL);


--
-- Name: federation_shares_by_grantor_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_shares_by_grantor_idx ON public.federation_shares USING btree (grantor_user_ref, granted_at DESC) WHERE (revoked_at IS NULL);


--
-- Name: federation_shares_by_peer_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_shares_by_peer_idx ON public.federation_shares USING btree (peer_id) WHERE (revoked_at IS NULL);


--
-- Name: federation_shares_delivery_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_shares_delivery_idx ON public.federation_shares USING btree (object_kind, object_id) WHERE (revoked_at IS NULL);


--
-- Name: federation_shares_expiring_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_shares_expiring_idx ON public.federation_shares USING btree (expires_at) WHERE ((revoked_at IS NULL) AND (expires_at IS NOT NULL));


--
-- Name: federation_shares_lookup_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_shares_lookup_idx ON public.federation_shares USING btree (object_kind, object_id, peer_id) WHERE (revoked_at IS NULL);


--
-- Name: federation_user_keys_one_current_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX federation_user_keys_one_current_idx ON public.federation_user_keys USING btree (user_ref) WHERE (is_current = true);


--
-- Name: federation_user_keys_retained_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX federation_user_keys_retained_idx ON public.federation_user_keys USING btree (retained_until) WHERE (retained_until IS NOT NULL);


--
-- Name: field_definition_applies_to_gin; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX field_definition_applies_to_gin ON public.field_definition USING gin (applies_to);


--
-- Name: field_definition_group_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX field_definition_group_idx ON public.field_definition USING btree (display_group, display_order);


--
-- Name: field_definition_options_gin; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX field_definition_options_gin ON public.field_definition USING gin (options);


--
-- Name: field_definition_status_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX field_definition_status_idx ON public.field_definition USING btree (status) WHERE (status = 'active'::text);


--
-- Name: idx_ai_provider_call_asset; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ai_provider_call_asset ON public.ai_provider_call USING btree (asset_id, concern, triggered_at DESC) WHERE (asset_id IS NOT NULL);


--
-- Name: idx_ai_provider_call_billing; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ai_provider_call_billing ON public.ai_provider_call USING btree (provider, triggered_at DESC);


--
-- Name: idx_ai_provider_call_job; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ai_provider_call_job ON public.ai_provider_call USING btree (job_id) WHERE (job_id IS NOT NULL);


--
-- Name: idx_asset_embedding_d768_asset; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_asset_embedding_d768_asset ON public.asset_embedding_d768 USING btree (asset_id);


--
-- Name: idx_asset_embedding_d768_hnsw_cosine; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_asset_embedding_d768_hnsw_cosine ON public.asset_embedding_d768 USING hnsw (embedding public.vector_cosine_ops) WITH (m='16', ef_construction='64');


--
-- Name: idx_asset_tag_ai_provenance; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_asset_tag_ai_provenance ON public.asset_tag USING btree (created_by_model, added_at DESC) WHERE (source = 'ai'::text);


--
-- Name: idx_asset_tag_asset_source; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_asset_tag_asset_source ON public.asset_tag USING btree (asset_id, source);


--
-- Name: idx_assets_owner_hash_unique; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_assets_owner_hash_unique ON public.assets USING btree (owner_user_ref, file_hash) WHERE ((file_hash IS NOT NULL) AND (deleted_at IS NULL));


--
-- Name: idx_assets_sensitivity_restricted; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_assets_sensitivity_restricted ON public.assets USING btree (sensitivity) WHERE (sensitivity = ANY (ARRAY['restricted'::text, 'embargo'::text]));


--
-- Name: idx_collection_field_value_collection_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_collection_field_value_collection_id ON public.collection_field_value USING btree (collection_id);


--
-- Name: idx_collection_field_value_field_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_collection_field_value_field_id ON public.collection_field_value USING btree (field_id);


--
-- Name: idx_creative_lineage_source; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_creative_lineage_source ON public.creative_lineage USING btree (source_asset_id);


--
-- Name: idx_dm_recipient_recent; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_dm_recipient_recent ON public.direct_messages USING btree (recipient_user_ref, sent_at DESC);


--
-- Name: idx_dm_sender_recent; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_dm_sender_recent ON public.direct_messages USING btree (sender_user_ref, sent_at DESC);


--
-- Name: idx_dm_unread; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_dm_unread ON public.direct_messages USING btree (recipient_user_ref) WHERE (read_at IS NULL);


--
-- Name: idx_email_verification_token_active; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_email_verification_token_active ON public.email_verification_token USING btree (user_ref, expires_at) WHERE (consumed_at IS NULL);


--
-- Name: idx_extraction_failure_asset; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_extraction_failure_asset ON public.extraction_failure USING btree (asset_id);


--
-- Name: idx_extraction_failure_pending; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_extraction_failure_pending ON public.extraction_failure USING btree (occurred_at DESC) WHERE (dismissed_at IS NULL);


--
-- Name: idx_field_definition_extraction_source; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_field_definition_extraction_source ON public.field_definition USING btree (extraction_source) WHERE (extraction_source <> ''::text);


--
-- Name: idx_field_definition_subject_kind; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_field_definition_subject_kind ON public.field_definition USING btree (subject_kind, status, display_order);


--
-- Name: idx_mcp_server_enabled; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_mcp_server_enabled ON public.mcp_server_registration USING btree (enabled) WHERE (enabled = true);


--
-- Name: idx_metadata_backfill_run_active; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_metadata_backfill_run_active ON public.metadata_backfill_run USING btree (started_at DESC) WHERE ((completed_at IS NULL) AND (cancelled_at IS NULL));


--
-- Name: idx_notifications_actor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_notifications_actor ON public.notifications USING btree (actor_user_ref, created_at DESC) WHERE (actor_user_ref IS NOT NULL);


--
-- Name: idx_notifications_recipient_recent; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_notifications_recipient_recent ON public.notifications USING btree (recipient_user_ref, created_at DESC, id DESC);


--
-- Name: idx_notifications_unread; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_notifications_unread ON public.notifications USING btree (recipient_user_ref) WHERE (read_at IS NULL);


--
-- Name: idx_resource_request_by_asset; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_resource_request_by_asset ON public.resource_request USING btree (target_asset_id);


--
-- Name: idx_resource_request_by_requester; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_resource_request_by_requester ON public.resource_request USING btree (requester_user_ref, requested_at DESC);


--
-- Name: idx_resource_request_pending_oldest_first; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_resource_request_pending_oldest_first ON public.resource_request USING btree (requested_at) WHERE (state = 'pending'::text);


--
-- Name: idx_sessions_impersonated_by_active; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_sessions_impersonated_by_active ON public.sessions USING btree (impersonated_by_user_ref, created_at DESC) WHERE ((impersonated_by_user_ref IS NOT NULL) AND (revoked_at IS NULL));


--
-- Name: idx_user_blocks_blocked; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_blocks_blocked ON public.user_blocks USING btree (blocked_user_ref);


--
-- Name: idx_user_capability_grants_expires_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_capability_grants_expires_at ON public.user_capability_grants USING btree (expires_at) WHERE (expires_at IS NOT NULL);


--
-- Name: idx_user_capability_grants_request_ref; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_capability_grants_request_ref ON public.user_capability_grants USING btree (request_ref) WHERE (request_ref IS NOT NULL);


--
-- Name: idx_user_capability_revokes_expires_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_capability_revokes_expires_at ON public.user_capability_revokes USING btree (expires_at) WHERE (expires_at IS NOT NULL);


--
-- Name: idx_user_follows_followee; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_follows_followee ON public.user_follows USING btree (followee_user_ref, created_at DESC);


--
-- Name: idx_user_totp_confirmed; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_totp_confirmed ON public.user_totp USING btree (user_ref) WHERE (confirmed_at IS NOT NULL);


--
-- Name: idx_user_totp_recovery_active; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_totp_recovery_active ON public.user_totp_recovery_code USING btree (user_ref) WHERE (used_at IS NULL);


--
-- Name: jobs_lease_expiry_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX jobs_lease_expiry_idx ON public.jobs USING btree (lease_expires_at) WHERE (status = 'running'::text);


--
-- Name: jobs_pending_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX jobs_pending_idx ON public.jobs USING btree (priority, enqueued_at) WHERE (status = 'pending'::text);


--
-- Name: jobs_type_status_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX jobs_type_status_idx ON public.jobs USING btree (type, status);


--
-- Name: likes_local_uniq_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX likes_local_uniq_idx ON public.likes USING btree (target_kind, target_id, user_ref) WHERE (user_ref IS NOT NULL);


--
-- Name: likes_remote_uniq_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX likes_remote_uniq_idx ON public.likes USING btree (target_kind, target_id, peer_id, actor_uri) WHERE (peer_id IS NOT NULL);


--
-- Name: likes_target_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX likes_target_idx ON public.likes USING btree (target_kind, target_id);


--
-- Name: likes_user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX likes_user_idx ON public.likes USING btree (user_ref, liked_at DESC);


--
-- Name: post_acls_expires_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX post_acls_expires_idx ON public.post_acls USING btree (expires_at) WHERE (expires_at IS NOT NULL);


--
-- Name: post_acls_principal_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX post_acls_principal_idx ON public.post_acls USING btree (principal_type, principal_id);


--
-- Name: post_assets_asset_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX post_assets_asset_idx ON public.post_assets USING btree (asset_id);


--
-- Name: post_assets_sort_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX post_assets_sort_idx ON public.post_assets USING btree (post_id, sort_order, added_at);


--
-- Name: post_tags_tag_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX post_tags_tag_idx ON public.post_tags USING btree (tag);


--
-- Name: post_tags_tag_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX post_tags_tag_trgm ON public.post_tags USING gin (tag public.gin_trgm_ops);


--
-- Name: posts_author_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX posts_author_idx ON public.posts USING btree (author_user_ref, posted_at DESC) WHERE (deleted_at IS NULL);


--
-- Name: posts_cover_thumbnail_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX posts_cover_thumbnail_idx ON public.posts USING btree (cover_thumbnail_asset_id) WHERE (cover_thumbnail_asset_id IS NOT NULL);


--
-- Name: posts_public_feed_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX posts_public_feed_idx ON public.posts USING btree (posted_at DESC) WHERE ((deleted_at IS NULL) AND (visibility = 'public'::text));


--
-- Name: posts_search_text_gin; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX posts_search_text_gin ON public.posts USING gin (search_text) WHERE (deleted_at IS NULL);


--
-- Name: posts_state_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX posts_state_idx ON public.posts USING btree (state_id) WHERE (state_id IS NOT NULL);


--
-- Name: posts_team_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX posts_team_idx ON public.posts USING btree (team_id) WHERE (team_id IS NOT NULL);


--
-- Name: posts_title_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX posts_title_trgm ON public.posts USING gin (title public.gin_trgm_ops) WHERE ((deleted_at IS NULL) AND (title <> ''::text));


--
-- Name: posts_visibility_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX posts_visibility_idx ON public.posts USING btree (visibility) WHERE (deleted_at IS NULL);


--
-- Name: roles__parent_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX roles__parent_idx ON public.roles USING btree (parent_id);


--
-- Name: saved_search_due_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX saved_search_due_idx ON public.saved_search USING btree (last_run_at NULLS FIRST) WHERE (enabled = true);


--
-- Name: saved_search_owner_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX saved_search_owner_idx ON public.saved_search USING btree (owner_user_ref, id);


--
-- Name: search_feedback_feedback_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX search_feedback_feedback_at_idx ON public.search_feedback USING btree (feedback_at DESC);


--
-- Name: search_feedback_hit_asset_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX search_feedback_hit_asset_idx ON public.search_feedback USING btree (hit_asset_id);


--
-- Name: search_feedback_query_hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX search_feedback_query_hash_idx ON public.search_feedback USING btree (query_hash);


--
-- Name: search_feedback_user_ref_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX search_feedback_user_ref_at_idx ON public.search_feedback USING btree (user_ref, feedback_at DESC);


--
-- Name: search_reindex_run_active_uniq; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX search_reindex_run_active_uniq ON public.search_reindex_run USING btree ((true)) WHERE ((completed_at IS NULL) AND (cancelled_at IS NULL));


--
-- Name: search_visual_backfill_run_active_uniq; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX search_visual_backfill_run_active_uniq ON public.search_visual_backfill_run USING btree ((true)) WHERE ((completed_at IS NULL) AND (cancelled_at IS NULL));


--
-- Name: search_visual_backfill_run_started_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX search_visual_backfill_run_started_idx ON public.search_visual_backfill_run USING btree (started_at DESC);


--
-- Name: search_reindex_run_started_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX search_reindex_run_started_idx ON public.search_reindex_run USING btree (started_at DESC);


--
-- Name: sessions__last_used_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX sessions__last_used_idx ON public.sessions USING btree (last_used_at) WHERE (revoked_at IS NULL);


--
-- Name: sessions__user_ref_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX sessions__user_ref_idx ON public.sessions USING btree (user_ref) WHERE (revoked_at IS NULL);


--
-- Name: storage_objects__gc_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX storage_objects__gc_idx ON public.storage_objects USING btree (gc_eligible_at) WHERE (gc_eligible_at IS NOT NULL);


--
-- Name: storage_pins__subject_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX storage_pins__subject_idx ON public.storage_pins USING btree (pin_subject_type, pin_subject_id);


--
-- Name: team_closure_descendant_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX team_closure_descendant_idx ON public.team_closure USING btree (descendant_id);


--
-- Name: team_memberships_user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX team_memberships_user_idx ON public.team_memberships USING btree (user_ref);


--
-- Name: team_parents_parent_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX team_parents_parent_idx ON public.team_parents USING btree (parent_id);


--
-- Name: teams_active_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX teams_active_idx ON public.teams USING btree (id) WHERE (deleted_at IS NULL);


--
-- Name: teams_origin_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX teams_origin_idx ON public.teams USING btree (origin_server_id) WHERE (origin_server_id IS NOT NULL);


--
-- Name: uq_jobs_idempotency_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX uq_jobs_idempotency_key ON public.jobs USING btree (type, idempotency_key) WHERE ((idempotency_key IS NOT NULL) AND (status = ANY (ARRAY['pending'::text, 'running'::text])));


--
-- Name: user_actor_uri_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX user_actor_uri_idx ON public."user" USING btree (actor_uri) WHERE (actor_uri IS NOT NULL);


--
-- Name: user_approved_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX user_approved_idx ON public."user" USING btree (approved);


--
-- Name: user_capability_grants_team_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX user_capability_grants_team_idx ON public.user_capability_grants USING btree (team_id) WHERE (team_id IS NOT NULL);


--
-- Name: user_capability_grants_user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX user_capability_grants_user_idx ON public.user_capability_grants USING btree (user_ref);


--
-- Name: user_capability_revokes_team_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX user_capability_revokes_team_idx ON public.user_capability_revokes USING btree (team_id) WHERE (team_id IS NOT NULL);


--
-- Name: user_capability_revokes_user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX user_capability_revokes_user_idx ON public.user_capability_revokes USING btree (user_ref);


--
-- Name: user_created_ref_desc_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX user_created_ref_desc_idx ON public."user" USING btree (created DESC NULLS LAST, ref DESC);


--
-- Name: user_email_lower_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX user_email_lower_idx ON public."user" USING btree (lower((email)::text));


--
-- Name: user_fullname_lower_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX user_fullname_lower_idx ON public."user" USING btree (lower((fullname)::text));


--
-- Name: user_password_history_user_changed_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX user_password_history_user_changed_idx ON public.user_password_history USING btree (user_ref, changed_at DESC);


--
-- Name: user_profiles_origin_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX user_profiles_origin_idx ON public.user_profiles USING btree (origin_server_id) WHERE (origin_server_id IS NOT NULL);


--
-- Name: user_roles_role_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX user_roles_role_idx ON public.user_roles USING btree (role_id);


--
-- Name: user_roles_team_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX user_roles_team_idx ON public.user_roles USING btree (team_id) WHERE (team_id IS NOT NULL);


--
-- Name: user_roles_user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX user_roles_user_idx ON public.user_roles USING btree (user_ref);


--
-- Name: user_username_lower_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX user_username_lower_idx ON public."user" USING btree (lower((username)::text));


--
-- Name: user_username_uniq_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX user_username_uniq_idx ON public."user" USING btree (username);


--
-- Name: workflow_audit_resource_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX workflow_audit_resource_idx ON public.workflow_audit USING btree (resource_kind, resource_id, transitioned_at DESC);


--
-- Name: workflow_states_domain_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX workflow_states_domain_idx ON public.workflow_states USING btree (domain, sort_order);


--
-- Name: workflow_states_one_initial_per_domain; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX workflow_states_one_initial_per_domain ON public.workflow_states USING btree (domain) WHERE is_initial;


--
-- Name: workflow_transitions_from_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX workflow_transitions_from_idx ON public.workflow_transitions USING btree (from_state_id);


--
-- Name: workflow_transitions_to_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX workflow_transitions_to_idx ON public.workflow_transitions USING btree (to_state_id);


--
-- Name: roles acl_sweep_after_role_delete; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER acl_sweep_after_role_delete AFTER DELETE ON public.roles FOR EACH ROW EXECUTE FUNCTION public.acl_sweep_on_role_delete();


--
-- Name: teams acl_sweep_after_team_delete; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER acl_sweep_after_team_delete AFTER DELETE ON public.teams FOR EACH ROW EXECUTE FUNCTION public.acl_sweep_on_team_delete();


--
-- Name: asset_field_value asset_field_value_search_text; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER asset_field_value_search_text AFTER INSERT OR DELETE OR UPDATE ON public.asset_field_value FOR EACH ROW EXECUTE FUNCTION public.asset_field_value_trigger();


--
-- Name: roles asset_type_acl_sweep_after_role_delete; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER asset_type_acl_sweep_after_role_delete AFTER DELETE ON public.roles FOR EACH ROW EXECUTE FUNCTION public.asset_type_acl_sweep_on_role_delete();


--
-- Name: teams asset_type_acl_sweep_after_team_delete; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER asset_type_acl_sweep_after_team_delete AFTER DELETE ON public.teams FOR EACH ROW EXECUTE FUNCTION public.asset_type_acl_sweep_on_team_delete();


--
-- Name: assets assets_search_text_trigger; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER assets_search_text_trigger AFTER INSERT OR UPDATE ON public.assets FOR EACH ROW EXECUTE FUNCTION public.asset_changed_trigger();


--
-- Name: collections collections_search_text; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER collections_search_text AFTER INSERT OR UPDATE OF name, description ON public.collections FOR EACH ROW EXECUTE FUNCTION public.collections_search_text_trigger();


--
-- Name: comments comments_maintain_counter_delete; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER comments_maintain_counter_delete AFTER DELETE ON public.comments FOR EACH ROW EXECUTE FUNCTION public.comments_after_delete();


--
-- Name: comments comments_maintain_counter_insert; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER comments_maintain_counter_insert AFTER INSERT ON public.comments FOR EACH ROW EXECUTE FUNCTION public.comments_after_insert();


--
-- Name: comments comments_maintain_counter_update; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER comments_maintain_counter_update AFTER UPDATE OF deleted_at ON public.comments FOR EACH ROW EXECUTE FUNCTION public.comments_after_update();


--
-- Name: activities federation_dispatch_notify_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER federation_dispatch_notify_trg AFTER INSERT ON public.activities FOR EACH ROW EXECUTE FUNCTION public.federation_dispatch_notify();


--
-- Name: federation_inbox federation_inbox_dispatch_notify_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER federation_inbox_dispatch_notify_trg AFTER INSERT ON public.federation_inbox FOR EACH ROW EXECUTE FUNCTION public.federation_inbox_dispatch_notify();


--
-- Name: federation_outbox federation_outbox_dispatch_notify_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER federation_outbox_dispatch_notify_trg AFTER INSERT ON public.federation_outbox FOR EACH ROW EXECUTE FUNCTION public.federation_outbox_dispatch_notify();


--
-- Name: likes likes_maintain_counter_delete; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER likes_maintain_counter_delete AFTER DELETE ON public.likes FOR EACH ROW EXECUTE FUNCTION public.likes_after_delete();


--
-- Name: likes likes_maintain_counter_insert; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER likes_maintain_counter_insert AFTER INSERT ON public.likes FOR EACH ROW EXECUTE FUNCTION public.likes_after_insert();


--
-- Name: post_assets post_assets_search_text; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER post_assets_search_text AFTER INSERT OR DELETE OR UPDATE ON public.post_assets FOR EACH ROW EXECUTE FUNCTION public.post_assets_search_text_trigger();


--
-- Name: post_tags post_tags_search_text; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER post_tags_search_text AFTER INSERT OR DELETE OR UPDATE ON public.post_tags FOR EACH ROW EXECUTE FUNCTION public.post_tags_search_text_trigger();


--
-- Name: posts posts_search_text; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER posts_search_text AFTER INSERT OR UPDATE OF title, description ON public.posts FOR EACH ROW EXECUTE FUNCTION public.posts_search_text_trigger();


--
-- Name: posts social_sweep_after_post_delete; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER social_sweep_after_post_delete AFTER DELETE ON public.posts FOR EACH ROW EXECUTE FUNCTION public.social_sweep_on_post_delete();


--
-- Name: team_parents team_parents_propagate_closure; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER team_parents_propagate_closure AFTER INSERT ON public.team_parents FOR EACH ROW EXECUTE FUNCTION public.team_parents_after_insert();


--
-- Name: team_parents team_parents_rebuild_on_delete; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER team_parents_rebuild_on_delete AFTER DELETE ON public.team_parents FOR EACH ROW EXECUTE FUNCTION public.team_parents_after_delete();


--
-- Name: team_parents team_parents_reject_cycle; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER team_parents_reject_cycle BEFORE INSERT ON public.team_parents FOR EACH ROW EXECUTE FUNCTION public.team_parents_before_insert();


--
-- Name: teams teams_self_closure; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER teams_self_closure AFTER INSERT ON public.teams FOR EACH ROW EXECUTE FUNCTION public.teams_insert_self_closure();


--
-- Name: ai_provider_call ai_provider_call_job_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ai_provider_call
    ADD CONSTRAINT ai_provider_call_job_id_fkey FOREIGN KEY (job_id) REFERENCES public.jobs(id) ON DELETE SET NULL;


--
-- Name: asset_alternates asset_alternates_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_alternates
    ADD CONSTRAINT asset_alternates_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id) ON DELETE CASCADE;


--
-- Name: asset_alternates asset_alternates_object_hash_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_alternates
    ADD CONSTRAINT asset_alternates_object_hash_fkey FOREIGN KEY (object_hash) REFERENCES public.storage_objects(hash) ON DELETE RESTRICT;


--
-- Name: asset_companions asset_companions_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_companions
    ADD CONSTRAINT asset_companions_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id) ON DELETE CASCADE;


--
-- Name: asset_companions asset_companions_object_hash_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_companions
    ADD CONSTRAINT asset_companions_object_hash_fkey FOREIGN KEY (object_hash) REFERENCES public.storage_objects(hash) ON DELETE RESTRICT;


--
-- Name: asset_embedding_d768 asset_embedding_d768_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_embedding_d768
    ADD CONSTRAINT asset_embedding_d768_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id) ON DELETE CASCADE;


--
-- Name: asset_field_value asset_field_value_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_field_value
    ADD CONSTRAINT asset_field_value_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id) ON DELETE CASCADE;


--
-- Name: asset_field_value asset_field_value_field_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_field_value
    ADD CONSTRAINT asset_field_value_field_id_fkey FOREIGN KEY (field_id) REFERENCES public.field_definition(id) ON DELETE CASCADE;


--
-- Name: asset_subtitle_tracks asset_subtitle_tracks_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_subtitle_tracks
    ADD CONSTRAINT asset_subtitle_tracks_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id) ON DELETE CASCADE;


--
-- Name: asset_tag asset_tag_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_tag
    ADD CONSTRAINT asset_tag_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id) ON DELETE CASCADE;


--
-- Name: asset_type_acls asset_type_acls_asset_type_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.asset_type_acls
    ADD CONSTRAINT asset_type_acls_asset_type_ref_fkey FOREIGN KEY (asset_type_ref) REFERENCES public.asset_types(ref) ON DELETE CASCADE;


--
-- Name: assets assets_asset_type_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.assets
    ADD CONSTRAINT assets_asset_type_fkey FOREIGN KEY (asset_type) REFERENCES public.asset_types(ref) ON DELETE RESTRICT;


--
-- Name: assets assets_file_hash_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.assets
    ADD CONSTRAINT assets_file_hash_fkey FOREIGN KEY (file_hash) REFERENCES public.storage_objects(hash) ON DELETE SET NULL;


--
-- Name: assets assets_state_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.assets
    ADD CONSTRAINT assets_state_id_fkey FOREIGN KEY (state_id) REFERENCES public.workflow_states(id) ON DELETE SET NULL;


--
-- Name: assets assets_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.assets
    ADD CONSTRAINT assets_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE SET NULL;


--
-- Name: brush_pack_stamps brush_pack_stamps_pack_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.brush_pack_stamps
    ADD CONSTRAINT brush_pack_stamps_pack_id_fkey FOREIGN KEY (pack_id) REFERENCES public.brush_packs(id) ON DELETE CASCADE;


--
-- Name: brush_packs brush_packs_owner_user_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.brush_packs
    ADD CONSTRAINT brush_packs_owner_user_ref_fkey FOREIGN KEY (owner_user_ref) REFERENCES public."user"(ref) ON DELETE CASCADE;


--
-- Name: collection_acls collection_acls_collection_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.collection_acls
    ADD CONSTRAINT collection_acls_collection_id_fkey FOREIGN KEY (collection_id) REFERENCES public.collections(id) ON DELETE CASCADE;


--
-- Name: collection_field_value collection_field_value_collection_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.collection_field_value
    ADD CONSTRAINT collection_field_value_collection_id_fkey FOREIGN KEY (collection_id) REFERENCES public.collections(id) ON DELETE CASCADE;


--
-- Name: collection_field_value collection_field_value_field_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.collection_field_value
    ADD CONSTRAINT collection_field_value_field_id_fkey FOREIGN KEY (field_id) REFERENCES public.field_definition(id) ON DELETE CASCADE;


--
-- Name: collection_field_value_history collection_field_value_history_collection_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.collection_field_value_history
    ADD CONSTRAINT collection_field_value_history_collection_id_fkey FOREIGN KEY (collection_id) REFERENCES public.collections(id) ON DELETE CASCADE;


--
-- Name: collection_field_value_history collection_field_value_history_field_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.collection_field_value_history
    ADD CONSTRAINT collection_field_value_history_field_id_fkey FOREIGN KEY (field_id) REFERENCES public.field_definition(id) ON DELETE CASCADE;


--
-- Name: collection_posts collection_posts_collection_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.collection_posts
    ADD CONSTRAINT collection_posts_collection_id_fkey FOREIGN KEY (collection_id) REFERENCES public.collections(id) ON DELETE CASCADE;


--
-- Name: collection_posts collection_posts_post_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.collection_posts
    ADD CONSTRAINT collection_posts_post_id_fkey FOREIGN KEY (post_id) REFERENCES public.posts(id) ON DELETE CASCADE;


--
-- Name: collection_resources collection_resources_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.collection_resources
    ADD CONSTRAINT collection_resources_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id) ON DELETE CASCADE;


--
-- Name: collection_resources collection_resources_collection_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.collection_resources
    ADD CONSTRAINT collection_resources_collection_id_fkey FOREIGN KEY (collection_id) REFERENCES public.collections(id) ON DELETE CASCADE;


--
-- Name: comments comments_parent_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.comments
    ADD CONSTRAINT comments_parent_id_fkey FOREIGN KEY (parent_id) REFERENCES public.comments(id) ON DELETE CASCADE;


--
-- Name: comments comments_peer_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.comments
    ADD CONSTRAINT comments_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE;


--
-- Name: creative_lineage creative_lineage_derivative_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.creative_lineage
    ADD CONSTRAINT creative_lineage_derivative_asset_id_fkey FOREIGN KEY (derivative_asset_id) REFERENCES public.assets(id) ON DELETE CASCADE;


--
-- Name: creative_lineage creative_lineage_source_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.creative_lineage
    ADD CONSTRAINT creative_lineage_source_asset_id_fkey FOREIGN KEY (source_asset_id) REFERENCES public.assets(id) ON DELETE CASCADE;


--
-- Name: email_verification_token email_verification_token_user_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.email_verification_token
    ADD CONSTRAINT email_verification_token_user_ref_fkey FOREIGN KEY (user_ref) REFERENCES public."user"(ref) ON DELETE CASCADE;


--
-- Name: extraction_failure extraction_failure_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.extraction_failure
    ADD CONSTRAINT extraction_failure_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id) ON DELETE CASCADE;


--
-- Name: federation_directory_entries federation_directory_entries_directory_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_directory_entries
    ADD CONSTRAINT federation_directory_entries_directory_id_fkey FOREIGN KEY (directory_id) REFERENCES public.federation_directories(id) ON DELETE CASCADE;


--
-- Name: federation_inbox federation_inbox_correlation_activity_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_inbox
    ADD CONSTRAINT federation_inbox_correlation_activity_id_fkey FOREIGN KEY (correlation_activity_id) REFERENCES public.activities(id) ON DELETE SET NULL;


--
-- Name: federation_inbox federation_inbox_peer_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_inbox
    ADD CONSTRAINT federation_inbox_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE;


--
-- Name: federation_outbox federation_outbox_activity_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_outbox
    ADD CONSTRAINT federation_outbox_activity_id_fkey FOREIGN KEY (activity_id) REFERENCES public.activities(id) ON DELETE CASCADE;


--
-- Name: federation_outbox federation_outbox_peer_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_outbox
    ADD CONSTRAINT federation_outbox_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE;


--
-- Name: federation_peer_suggestions federation_peer_suggestions_source_peer_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_peer_suggestions
    ADD CONSTRAINT federation_peer_suggestions_source_peer_id_fkey FOREIGN KEY (source_peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE;


--
-- Name: federation_remote_actors federation_remote_actors_peer_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_remote_actors
    ADD CONSTRAINT federation_remote_actors_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE;


--
-- Name: federation_shares federation_shares_granted_activity_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_shares
    ADD CONSTRAINT federation_shares_granted_activity_id_fkey FOREIGN KEY (granted_activity_id) REFERENCES public.activities(id) ON DELETE RESTRICT;


--
-- Name: federation_shares federation_shares_peer_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_shares
    ADD CONSTRAINT federation_shares_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE;


--
-- Name: federation_shares federation_shares_revoked_activity_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_shares
    ADD CONSTRAINT federation_shares_revoked_activity_id_fkey FOREIGN KEY (revoked_activity_id) REFERENCES public.activities(id) ON DELETE RESTRICT;


--
-- Name: federation_user_keys federation_user_keys_rotated_by_user_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_user_keys
    ADD CONSTRAINT federation_user_keys_rotated_by_user_ref_fkey FOREIGN KEY (rotated_by_user_ref) REFERENCES public."user"(ref) ON DELETE SET NULL;


--
-- Name: federation_user_keys federation_user_keys_user_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.federation_user_keys
    ADD CONSTRAINT federation_user_keys_user_ref_fkey FOREIGN KEY (user_ref) REFERENCES public."user"(ref) ON DELETE CASCADE;


--
-- Name: field_definition field_definition_deprecated_replacement_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.field_definition
    ADD CONSTRAINT field_definition_deprecated_replacement_id_fkey FOREIGN KEY (deprecated_replacement_id) REFERENCES public.field_definition(id) ON DELETE RESTRICT;


--
-- Name: likes likes_peer_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.likes
    ADD CONSTRAINT likes_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE;


--
-- Name: mcp_server_tool_grant mcp_server_tool_grant_server_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mcp_server_tool_grant
    ADD CONSTRAINT mcp_server_tool_grant_server_id_fkey FOREIGN KEY (server_id) REFERENCES public.mcp_server_registration(id) ON DELETE CASCADE;


--
-- Name: metadata_backfill_run metadata_backfill_run_started_by_user_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.metadata_backfill_run
    ADD CONSTRAINT metadata_backfill_run_started_by_user_ref_fkey FOREIGN KEY (started_by_user_ref) REFERENCES public."user"(ref) ON DELETE SET NULL;


--
-- Name: post_acls post_acls_post_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.post_acls
    ADD CONSTRAINT post_acls_post_id_fkey FOREIGN KEY (post_id) REFERENCES public.posts(id) ON DELETE CASCADE;


--
-- Name: post_assets post_assets_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.post_assets
    ADD CONSTRAINT post_assets_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id) ON DELETE CASCADE;


--
-- Name: post_assets post_assets_post_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.post_assets
    ADD CONSTRAINT post_assets_post_id_fkey FOREIGN KEY (post_id) REFERENCES public.posts(id) ON DELETE CASCADE;


--
-- Name: post_tags post_tags_post_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.post_tags
    ADD CONSTRAINT post_tags_post_id_fkey FOREIGN KEY (post_id) REFERENCES public.posts(id) ON DELETE CASCADE;


--
-- Name: posts posts_cover_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.posts
    ADD CONSTRAINT posts_cover_asset_id_fkey FOREIGN KEY (cover_asset_id) REFERENCES public.assets(id) ON DELETE SET NULL;


--
-- Name: posts posts_cover_thumbnail_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.posts
    ADD CONSTRAINT posts_cover_thumbnail_asset_id_fkey FOREIGN KEY (cover_thumbnail_asset_id) REFERENCES public.assets(id) ON DELETE SET NULL;


--
-- Name: posts posts_state_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.posts
    ADD CONSTRAINT posts_state_id_fkey FOREIGN KEY (state_id) REFERENCES public.workflow_states(id) ON DELETE SET NULL;


--
-- Name: posts posts_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.posts
    ADD CONSTRAINT posts_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE SET NULL;


--
-- Name: resource_request resource_request_decided_by_user_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.resource_request
    ADD CONSTRAINT resource_request_decided_by_user_ref_fkey FOREIGN KEY (decided_by_user_ref) REFERENCES public."user"(ref);


--
-- Name: resource_request resource_request_requester_user_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.resource_request
    ADD CONSTRAINT resource_request_requester_user_ref_fkey FOREIGN KEY (requester_user_ref) REFERENCES public."user"(ref) ON DELETE CASCADE;


--
-- Name: role_capabilities role_capabilities_capability_code_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.role_capabilities
    ADD CONSTRAINT role_capabilities_capability_code_fkey FOREIGN KEY (capability_code) REFERENCES public.capabilities(code) ON DELETE CASCADE;


--
-- Name: role_capabilities role_capabilities_role_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.role_capabilities
    ADD CONSTRAINT role_capabilities_role_id_fkey FOREIGN KEY (role_id) REFERENCES public.roles(id) ON DELETE CASCADE;


--
-- Name: roles roles_parent_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.roles
    ADD CONSTRAINT roles_parent_id_fkey FOREIGN KEY (parent_id) REFERENCES public.roles(id) ON DELETE SET NULL;


--
-- Name: saved_search saved_search_owner_user_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.saved_search
    ADD CONSTRAINT saved_search_owner_user_ref_fkey FOREIGN KEY (owner_user_ref) REFERENCES public."user"(ref) ON DELETE CASCADE;


--
-- Name: search_feedback search_feedback_hit_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.search_feedback
    ADD CONSTRAINT search_feedback_hit_asset_id_fkey FOREIGN KEY (hit_asset_id) REFERENCES public.assets(id) ON DELETE CASCADE;


--
-- Name: search_feedback search_feedback_user_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.search_feedback
    ADD CONSTRAINT search_feedback_user_ref_fkey FOREIGN KEY (user_ref) REFERENCES public."user"(ref) ON DELETE CASCADE;


--
-- Name: search_reindex_run search_reindex_run_started_by_user_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.search_reindex_run
    ADD CONSTRAINT search_reindex_run_started_by_user_ref_fkey FOREIGN KEY (started_by_user_ref) REFERENCES public."user"(ref);


--
-- Name: search_visual_backfill_run search_visual_backfill_run_started_by_user_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.search_visual_backfill_run
    ADD CONSTRAINT search_visual_backfill_run_started_by_user_ref_fkey FOREIGN KEY (started_by_user_ref) REFERENCES public."user"(ref);


--
-- Name: sessions sessions_impersonated_by_user_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_impersonated_by_user_ref_fkey FOREIGN KEY (impersonated_by_user_ref) REFERENCES public."user"(ref) ON DELETE SET NULL;


--
-- Name: storage_pins storage_pins_object_hash_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_pins
    ADD CONSTRAINT storage_pins_object_hash_fkey FOREIGN KEY (object_hash) REFERENCES public.storage_objects(hash) ON DELETE RESTRICT;


--
-- Name: storage_variants storage_variants_object_hash_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_variants
    ADD CONSTRAINT storage_variants_object_hash_fkey FOREIGN KEY (object_hash) REFERENCES public.storage_objects(hash) ON DELETE CASCADE;


--
-- Name: team_closure team_closure_ancestor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_closure
    ADD CONSTRAINT team_closure_ancestor_id_fkey FOREIGN KEY (ancestor_id) REFERENCES public.teams(id) ON DELETE CASCADE;


--
-- Name: team_closure team_closure_descendant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_closure
    ADD CONSTRAINT team_closure_descendant_id_fkey FOREIGN KEY (descendant_id) REFERENCES public.teams(id) ON DELETE CASCADE;


--
-- Name: team_memberships team_memberships_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_memberships
    ADD CONSTRAINT team_memberships_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;


--
-- Name: team_parents team_parents_child_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_parents
    ADD CONSTRAINT team_parents_child_id_fkey FOREIGN KEY (child_id) REFERENCES public.teams(id) ON DELETE CASCADE;


--
-- Name: team_parents team_parents_parent_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_parents
    ADD CONSTRAINT team_parents_parent_id_fkey FOREIGN KEY (parent_id) REFERENCES public.teams(id) ON DELETE CASCADE;


--
-- Name: user_capability_grants user_capability_grants_capability_code_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_capability_grants
    ADD CONSTRAINT user_capability_grants_capability_code_fkey FOREIGN KEY (capability_code) REFERENCES public.capabilities(code) ON DELETE CASCADE;


--
-- Name: user_capability_grants user_capability_grants_request_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_capability_grants
    ADD CONSTRAINT user_capability_grants_request_ref_fkey FOREIGN KEY (request_ref) REFERENCES public.resource_request(id) ON DELETE SET NULL;


--
-- Name: user_capability_grants user_capability_grants_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_capability_grants
    ADD CONSTRAINT user_capability_grants_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;


--
-- Name: user_capability_revokes user_capability_revokes_capability_code_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_capability_revokes
    ADD CONSTRAINT user_capability_revokes_capability_code_fkey FOREIGN KEY (capability_code) REFERENCES public.capabilities(code) ON DELETE CASCADE;


--
-- Name: user_capability_revokes user_capability_revokes_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_capability_revokes
    ADD CONSTRAINT user_capability_revokes_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;


--
-- Name: user_roles user_role_role_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_roles
    ADD CONSTRAINT user_role_role_id_fkey FOREIGN KEY (role_id) REFERENCES public.roles(id) ON DELETE CASCADE;


--
-- Name: user_roles user_roles_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_roles
    ADD CONSTRAINT user_roles_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;


--
-- Name: user_totp_recovery_code user_totp_recovery_code_user_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_totp_recovery_code
    ADD CONSTRAINT user_totp_recovery_code_user_ref_fkey FOREIGN KEY (user_ref) REFERENCES public."user"(ref) ON DELETE CASCADE;


--
-- Name: user_totp user_totp_user_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_totp
    ADD CONSTRAINT user_totp_user_ref_fkey FOREIGN KEY (user_ref) REFERENCES public."user"(ref) ON DELETE CASCADE;


--
-- Name: workflow_audit workflow_audit_from_state_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workflow_audit
    ADD CONSTRAINT workflow_audit_from_state_id_fkey FOREIGN KEY (from_state_id) REFERENCES public.workflow_states(id) ON DELETE SET NULL;


--
-- Name: workflow_audit workflow_audit_to_state_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workflow_audit
    ADD CONSTRAINT workflow_audit_to_state_id_fkey FOREIGN KEY (to_state_id) REFERENCES public.workflow_states(id) ON DELETE SET NULL;


--
-- Name: workflow_transitions workflow_transitions_from_state_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workflow_transitions
    ADD CONSTRAINT workflow_transitions_from_state_id_fkey FOREIGN KEY (from_state_id) REFERENCES public.workflow_states(id) ON DELETE CASCADE;


--
-- Name: workflow_transitions workflow_transitions_required_capability_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workflow_transitions
    ADD CONSTRAINT workflow_transitions_required_capability_fkey FOREIGN KEY (required_capability) REFERENCES public.capabilities(code) ON DELETE SET NULL;


--
-- Name: workflow_transitions workflow_transitions_to_state_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workflow_transitions
    ADD CONSTRAINT workflow_transitions_to_state_id_fkey FOREIGN KEY (to_state_id) REFERENCES public.workflow_states(id) ON DELETE CASCADE;


--
-- PostgreSQL database dump complete
--

\unrestrict BIawPaLNrAa1UYpG7hem3AQptIFpU8M7DvjZ4he0fDcFV12iKTSuRDviek2Uif9


--
-- Storage integrity sweeps (#403, migration 00007). Appended rather
-- than re-dumping the whole schema: a full pg_dump refresh also pulls
-- in unrelated drift that has accumulated since this file was last
-- regenerated, which changes sqlc output for other packages.
--

CREATE TABLE public.storage_sweep_runs (
    id uuid NOT NULL,
    kind text NOT NULL,
    status text DEFAULT 'running'::text NOT NULL,
    cursor text,
    objects_scanned bigint DEFAULT 0 NOT NULL,
    findings_count bigint DEFAULT 0 NOT NULL,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    finished_at timestamp with time zone,
    error text,
    triggered_by_user_ref bigint,
    CONSTRAINT storage_sweep_runs_counts_check CHECK (((objects_scanned >= 0) AND (findings_count >= 0))),
    CONSTRAINT storage_sweep_runs_kind_check CHECK ((kind = ANY (ARRAY['orphan_scan'::text, 'checksum_verify'::text]))),
    CONSTRAINT storage_sweep_runs_status_check CHECK ((status = ANY (ARRAY['running'::text, 'completed'::text, 'failed'::text])))
);

CREATE TABLE public.storage_sweep_findings (
    id uuid NOT NULL,
    run_id uuid NOT NULL,
    finding text NOT NULL,
    object_hash text NOT NULL,
    variant_key text NOT NULL,
    detail text DEFAULT ''::text NOT NULL,
    detected_at timestamp with time zone DEFAULT now() NOT NULL,
    resolved_at timestamp with time zone,
    CONSTRAINT storage_sweep_findings_finding_check CHECK ((finding = ANY (ARRAY['missing_object'::text, 'orphan_object'::text, 'checksum_mismatch'::text, 'size_mismatch'::text])))
);

CREATE INDEX storage_sweep_findings_run_idx ON public.storage_sweep_findings USING btree (run_id, detected_at DESC);

CREATE INDEX storage_sweep_findings_subject_idx ON public.storage_sweep_findings USING btree (object_hash, variant_key) WHERE (resolved_at IS NULL);

CREATE INDEX storage_sweep_runs_kind_started_idx ON public.storage_sweep_runs USING btree (kind, started_at DESC);
