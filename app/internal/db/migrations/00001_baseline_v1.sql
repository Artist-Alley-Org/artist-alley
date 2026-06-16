-- 00001_baseline_v1.sql
--
-- Pre-MVP migration baseline per ADR 0046 — the SQUASH commit.
-- Replaces migrations 00001 through 00014 (the prior pre-MVP
-- sequence shipped through Phase 1.22.I-i) with a single
-- canonical end-state.
--
-- DESTRUCTIVE: no upgrade path from the prior sequence is
-- supported. Developers updating from a pre-squash commit MUST
-- run `docker compose down -v` before pulling this revision;
-- their existing DB will not be migration-compatible.
--
-- Source: pg_dump --schema-only on a freshly-migrated DB at
--         the post-1.22.I-i + post-1.49.C-1 state, then
--         hand-edited per the 24 audit-derived edits from
--         docs/cleanup-audit-2026-06.md:
--           - F-001..F-017: 17 RS-heritage user columns dropped
--           - F-018: 4 implicit-NO-ACTION FKs annotated explicit
--                    ON DELETE RESTRICT
--           - F-021: federation_user_keys.user_id → user_ref
--           - F-022: COMMENT ON COLUMN asset_field_value.value_ref
--           - F-023: brush_packs.owner_ref → owner_user_ref
--
-- ADR:    docs/adr/0046-migration-baseline-and-squash-policy.md
-- Audit:  docs/cleanup-audit-2026-06.md (incl. C-2 action log)
--
-- ADR 0046 §"When a squash is allowed" — this is the LAST
-- allowed squash. After the v1.0.0 tag ships, all subsequent
-- migrations are append-only forever. The Down section is
-- intentionally empty: rollback from a baseline means restoring
-- from backup, not running migrations.

-- +goose Up
-- +goose StatementBegin

--
--





CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;



COMMENT ON EXTENSION pg_trgm IS 'text similarity measurement and index searching based on trigrams';



CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;



COMMENT ON EXTENSION pgcrypto IS 'cryptographic functions';



CREATE EXTENSION IF NOT EXISTS "uuid-ossp" WITH SCHEMA public;



COMMENT ON EXTENSION "uuid-ossp" IS 'generate universally unique identifiers (UUIDs)';



CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public;



COMMENT ON EXTENSION vector IS 'vector data type and ivfflat and hnsw access methods';



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



CREATE FUNCTION public.asset_type_acl_sweep_on_role_delete() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    DELETE FROM asset_type_acls
     WHERE principal_type = 'role' AND principal_id = OLD.id::text;
    RETURN OLD;
END;
$$;



CREATE FUNCTION public.asset_type_acl_sweep_on_team_delete() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    DELETE FROM asset_type_acls
     WHERE principal_type = 'team' AND principal_id = OLD.id::text;
    RETURN OLD;
END;
$$;



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



CREATE FUNCTION public.federation_dispatch_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('federation_dispatch_pending', NEW.id::text);
    RETURN NEW;
END;
$$;



CREATE FUNCTION public.federation_inbox_dispatch_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('federation_inbox_pending', NEW.id::text);
    RETURN NEW;
END;
$$;



CREATE FUNCTION public.federation_outbox_dispatch_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('federation_outbox_pending', NEW.id::text);
    RETURN NEW;
END;
$$;



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



CREATE FUNCTION public.post_assets_search_text_trigger() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM rebuild_post_search_text(COALESCE(NEW.post_id, OLD.post_id));
    RETURN NULL;
END;
$$;



CREATE FUNCTION public.post_tags_search_text_trigger() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM rebuild_post_search_text(COALESCE(NEW.post_id, OLD.post_id));
    RETURN NULL;
END;
$$;



CREATE FUNCTION public.posts_search_text_trigger() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM rebuild_post_search_text(NEW.id);
    RETURN NEW;
END;
$$;



CREATE FUNCTION public.rebuild_asset_search_text(p_asset_id uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    new_text TEXT;
BEGIN
    SELECT COALESCE(
        STRING_AGG(
            CASE
                WHEN v.value_text     IS NOT NULL THEN v.value_text
                WHEN v.value_options  IS NOT NULL THEN array_to_string(v.value_options, ' ')
                ELSE NULL
            END,
            ' '
        ),
        ''
    )
    INTO new_text
    FROM asset_field_value v
    JOIN field_definition f ON f.id = v.field_id
    WHERE v.asset_id = p_asset_id
      AND f.searchable = TRUE
      AND f.status = 'active';

    -- Include the asset's own title + description so they're searchable
    -- even before any field values land.
    UPDATE assets
       SET search_text = to_tsvector('english',
                            COALESCE(title, '') || ' ' ||
                            COALESCE(description, '') || ' ' ||
                            COALESCE(new_text, ''))
     WHERE id = p_asset_id;
END;
$$;



CREATE FUNCTION public.rebuild_post_search_text(p_post_id uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    asset_search TEXT;
    post_tag_text TEXT;
BEGIN
    SELECT COALESCE(string_agg(COALESCE(a.search_text::text, ''), ' '), '')
      INTO asset_search
      FROM post_assets pa
      JOIN assets a ON a.id = pa.asset_id
     WHERE pa.post_id = p_post_id AND a.deleted_at IS NULL;

    SELECT COALESCE(string_agg(tag, ' '), '')
      INTO post_tag_text
      FROM post_tags
     WHERE post_id = p_post_id;

    UPDATE posts
       SET search_text = to_tsvector('english',
                coalesce(title, '')       || ' ' ||
                coalesce(description, '') || ' ' ||
                coalesce(post_tag_text, '') || ' ' ||
                coalesce(asset_search, ''))
     WHERE id = p_post_id;
END;
$$;



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



CREATE FUNCTION public.team_parents_after_delete() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM team_closure_rebuild();
    RETURN NULL;
END;
$$;



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



CREATE TABLE public.asset_tag (
    asset_id uuid NOT NULL,
    tag text NOT NULL,
    added_at timestamp with time zone DEFAULT now() NOT NULL
);



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
    CONSTRAINT assets_processing_status_check CHECK ((processing_status = ANY (ARRAY['pending'::text, 'processing'::text, 'ready'::text, 'failed'::text]))),
    CONSTRAINT assets_sensitivity_check CHECK ((sensitivity = ANY (ARRAY['public'::text, 'team'::text, 'restricted'::text, 'embargo'::text]))),
    CONSTRAINT assets_status_check CHECK ((status = ANY (ARRAY['draft'::text, 'active'::text, 'archived'::text])))
);



COMMENT ON COLUMN public.assets.sensitivity IS 'Intrinsic sensitivity tier (public / team / restricted / embargo). Consumed by the federation outbox sender-refusal gate (1.22.I-g) + the inbox receiver-defense gate (1.22.I-h activated at I-i) when activities target this asset. Default ''public'' matches the pre-arc plaintext-everywhere behavior; operator-explicit upgrades are the load-bearing flow.';



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



CREATE TABLE public.brush_packs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    owner_user_ref bigint NOT NULL,
    name text NOT NULL,
    source_file text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    origin_server_id uuid
);



CREATE TABLE public.capabilities (
    code text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    required_license_feature text
);



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



CREATE TABLE public.collection_posts (
    collection_id uuid NOT NULL,
    post_id uuid NOT NULL,
    sort_order integer DEFAULT 0 NOT NULL,
    pinned boolean DEFAULT true NOT NULL,
    expires_at timestamp with time zone,
    added_at timestamp with time zone DEFAULT now() NOT NULL
);



CREATE TABLE public.collection_resources (
    collection_id uuid NOT NULL,
    asset_id uuid NOT NULL,
    sort_order integer DEFAULT 0 NOT NULL,
    pinned boolean DEFAULT true NOT NULL,
    expires_at timestamp with time zone,
    added_at timestamp with time zone DEFAULT now() NOT NULL
);



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
    CONSTRAINT collections_membership_check CHECK ((membership = ANY (ARRAY['manual'::text, 'query'::text, 'hybrid'::text]))),
    CONSTRAINT collections_visibility_check CHECK ((visibility = ANY (ARRAY['private'::text, 'org-only'::text, 'followers'::text, 'explicit-share'::text])))
);



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



CREATE TABLE public.federation_dispatch_state (
    id integer NOT NULL,
    last_dispatched_activity_id uuid,
    last_dispatched_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT federation_dispatch_state_id_check CHECK ((id = 1))
);



COMMENT ON TABLE public.federation_dispatch_state IS 'Single-row cursor for the outbox dispatcher. Atomically advanced with the outbox INSERTs in one transaction; restart picks up at the cursor without duplicates.';



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



COMMENT ON TABLE public.federation_inbox IS 'One row per inbound federation activity. Pipeline ingest persists status=pending; background worker transitions to processed/rejected/failed per §2.2 of the 1.22.D design proposal. activity_uri UNIQUE is the load-bearing replay guard.';



COMMENT ON COLUMN public.federation_inbox.was_encrypted IS 'Phase 1.22.I-f: TRUE when the dispatcher took the decrypt branch for this row (envelope had a non-empty encryption block). FALSE for the legacy 1.22.D plaintext path. Mirrors federation_outbox.was_encrypted on the sender side.';



COMMENT ON COLUMN public.federation_inbox.decrypted_with_key_version IS 'Phase 1.22.I-f: which version of the receiver''s X25519 key successfully decrypted the envelope. NULL when was_encrypted=false. Surfaces rotation health to operator analytics via the I-h admin federation page.';



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



COMMENT ON TABLE public.federation_outbox IS 'Per-recipient outbound queue. Derived from activities ledger via the dispatcher (Phase 1.22.D-b). Idempotent on (activity_id, peer_id, target_user_url) so the dispatcher is re-runnable. Sender-side emission refusal for restricted/embargo content (until 1.22.I ships X25519) is enforced at the dispatcher; refused activities never get an outbox row, they emit federation.emission.skipped audit instead.';



COMMENT ON COLUMN public.federation_outbox.was_encrypted IS 'Phase 1.22.I-e: TRUE when the dispatcher took the encryption branch for this row (peer.Capabilities.SupportsE2E + recipient key cached). FALSE for the legacy 1.22.D plaintext path. The wire envelope is the source of truth; this column is an observability mirror for scenario 09 + the admin federation surface.';



CREATE TABLE public.federation_peer_suggestions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    source_peer_id uuid NOT NULL,
    suggested_url text NOT NULL,
    suggested_display_name text NOT NULL,
    suggested_public_key text NOT NULL,
    suggested_fingerprint text NOT NULL,
    cached_at timestamp with time zone DEFAULT now() NOT NULL
);



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



COMMENT ON COLUMN public.federation_peers.capabilities IS 'Bilateral capability intersection (what BOTH peers support). JSONB array of typed strings per federation/peer.Capability. Open vocabulary on the wire, closed dispatch in code. See ADR 0049 §Track B Decision 3.';



COMMENT ON COLUMN public.federation_peers.capabilities_negotiated_at IS 'When the handshake completed with capability exchange. NULL means "never negotiated" (pre-1.22.I-d peer); peers in that state are surfaced via ListPeersMissingCapabilities for operator re-pairing. Distinct from `capabilities = ''[]''` which means "we negotiated and got an empty intersection" — also legal.';



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



COMMENT ON TABLE public.federation_remote_actors IS 'Display cache for remote actors surfaced in UI. The inbound dispatch handler upserts on every activity from a remote actor; display fields refresh naturally on each interaction. Keyed on actor_uri (globally unique per spec §8.3).';



COMMENT ON COLUMN public.federation_remote_actors.encryption_public_key IS 'X25519 public key advertised by the remote actor in their envelope''s aa:encryptionPublicKey block. NULL when the peer is on a pre-1.22.I-c build. 32 bytes when populated.';



COMMENT ON COLUMN public.federation_remote_actors.encryption_public_key_version IS 'Per-actor version number, monotonic. Bumped by the remote side on key rotation (1.22.I-h on their end); we just observe the value + persist alongside the key bytes.';



COMMENT ON COLUMN public.federation_remote_actors.encryption_public_key_updated_at IS 'When we last observed a key for this actor. Updated on every inbound envelope carrying the block (even when the value did not change) so an operator can see how stale our knowledge is.';



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



COMMENT ON TABLE public.federation_user_keys IS 'Per-user X25519 keypairs for NaCl-box encrypted federation. Phase 1.22.I-b. Private key column is atrest-wrapped (AES-GCM, host master key per app/internal/atrest). See ADR 0049 §Track B.';



COMMENT ON COLUMN public.federation_user_keys.algorithm IS 'Algorithm-version token. Single value today: naclbox-x25519-v1. Future algorithms add new tokens without a schema migration.';



COMMENT ON COLUMN public.federation_user_keys.is_current IS 'Exactly one row per user has is_current=true (enforced by partial unique index federation_user_keys_one_current_idx).';



COMMENT ON COLUMN public.federation_user_keys.retained_until IS 'Inbound-decrypt window for a rotated-aside key. NULL on the current key; NOW()+grace_period when a rotation flips this row aside.';



COMMENT ON COLUMN public.federation_user_keys.rotated_at IS 'When the rotation that produced (or flipped aside) this row occurred. NULL on pre-I-h rows. Non-NULL on the new current row AND on the previously-current row that was demoted in the same rotation.';



COMMENT ON COLUMN public.federation_user_keys.rotated_by_user_ref IS 'user.ref of whoever triggered the rotation. Equals user_ref for self-rotation (/account/security); differs for admin-initiated compromised-key recovery (/admin/federation/users/{ref}/rotate-keys). NULL on pre-I-h rows.';



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
    CONSTRAINT field_definition_status_check CHECK ((status = ANY (ARRAY['active'::text, 'deprecated'::text, 'archived'::text]))),
    CONSTRAINT field_definition_type_check CHECK ((type = ANY (ARRAY['text'::text, 'longtext'::text, 'rich_text'::text, 'number'::text, 'boolean'::text, 'date'::text, 'datetime'::text, 'select'::text, 'multi_select'::text, 'tree'::text, 'reference'::text])))
);



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
    CONSTRAINT jobs_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'running'::text, 'done'::text, 'failed'::text, 'cancelled'::text])))
);



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



CREATE TABLE public.post_assets (
    post_id uuid NOT NULL,
    asset_id uuid NOT NULL,
    sort_order integer DEFAULT 0 NOT NULL,
    added_at timestamp with time zone DEFAULT now() NOT NULL
);



CREATE TABLE public.post_tags (
    post_id uuid NOT NULL,
    tag text NOT NULL
);



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
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    state_id uuid,
    team_id uuid,
    cover_thumbnail_asset_id uuid,
    CONSTRAINT posts_visibility_check CHECK ((visibility = ANY (ARRAY['private'::text, 'org-only'::text, 'followers'::text, 'explicit-share'::text])))
);



ALTER TABLE public.asset_types ALTER COLUMN ref ADD GENERATED BY DEFAULT AS IDENTITY (
    SEQUENCE NAME public.resource_type_ref_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);



CREATE TABLE public.role_capabilities (
    role_id uuid NOT NULL,
    capability_code text NOT NULL
);



CREATE TABLE public.roles (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    parent_id uuid,
    name text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    origin_server_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);



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
    origin_server_id uuid
);



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



CREATE TABLE public.storage_pins (
    object_hash text NOT NULL,
    pin_subject_type text NOT NULL,
    pin_subject_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);



CREATE TABLE public.storage_variants (
    object_hash text NOT NULL,
    variant_key text NOT NULL,
    size_bytes bigint NOT NULL,
    content_type text DEFAULT 'application/octet-stream'::text NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT storage_variants_size_bytes_check CHECK ((size_bytes >= 0))
);



CREATE TABLE public.system_config (
    key text NOT NULL,
    value jsonb DEFAULT '{}'::jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);



CREATE TABLE public.team_closure (
    ancestor_id uuid NOT NULL,
    descendant_id uuid NOT NULL,
    depth integer NOT NULL
);



CREATE TABLE public.team_memberships (
    team_id uuid NOT NULL,
    user_ref bigint NOT NULL,
    added_at timestamp with time zone DEFAULT now() NOT NULL,
    added_by_user_ref bigint
);



CREATE TABLE public.team_parents (
    child_id uuid NOT NULL,
    parent_id uuid NOT NULL,
    CONSTRAINT team_parents_check CHECK ((child_id <> parent_id))
);



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



CREATE TABLE public."user" (
    ref bigint NOT NULL,
    username character varying(50),
    password character varying(255),
    fullname character varying(100),
    email character varying(100),
    usergroup bigint,
    last_active timestamp with time zone,
    logged_in integer,
    account_expires timestamp with time zone,
    comments text,
    session character varying(50),
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
    encryption_private_key_enc bytea
);



CREATE TABLE public.user_blocks (
    blocker_user_ref bigint NOT NULL,
    blocked_user_ref bigint NOT NULL,
    reason text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    origin_server_id uuid,
    CONSTRAINT user_blocks_check CHECK ((blocker_user_ref <> blocked_user_ref))
);



CREATE TABLE public.user_capability_grants (
    user_ref bigint NOT NULL,
    capability_code text NOT NULL,
    granted_at timestamp with time zone DEFAULT now() NOT NULL,
    granted_by_user_ref bigint,
    note text DEFAULT ''::text NOT NULL,
    team_id uuid
);



CREATE TABLE public.user_capability_revokes (
    user_ref bigint NOT NULL,
    capability_code text NOT NULL,
    revoked_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_by_user_ref bigint,
    note text DEFAULT ''::text NOT NULL,
    team_id uuid
);



CREATE TABLE public.user_follows (
    follower_user_ref bigint NOT NULL,
    followee_user_ref bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    origin_server_id uuid,
    CONSTRAINT user_follows_check CHECK ((follower_user_ref <> followee_user_ref))
);



CREATE TABLE public.user_password_history (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_ref bigint NOT NULL,
    password_hash character varying(255) NOT NULL,
    changed_at timestamp with time zone DEFAULT now() NOT NULL,
    origin_server_id uuid
);



CREATE TABLE public.user_preferences (
    user_ref bigint NOT NULL,
    notification_channels jsonb DEFAULT '{}'::jsonb NOT NULL,
    default_views jsonb DEFAULT '{}'::jsonb NOT NULL,
    origin_server_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);



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



ALTER TABLE public."user" ALTER COLUMN ref ADD GENERATED BY DEFAULT AS IDENTITY (
    SEQUENCE NAME public.user_ref_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);



CREATE TABLE public.user_roles (
    user_ref bigint NOT NULL,
    role_id uuid NOT NULL,
    assigned_at timestamp with time zone DEFAULT now() NOT NULL,
    assigned_by_user_ref bigint,
    team_id uuid
);



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



CREATE TABLE public.workflow_transitions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    from_state_id uuid,
    to_state_id uuid NOT NULL,
    required_capability text,
    requires_team_scope boolean DEFAULT false NOT NULL
);



ALTER TABLE ONLY public.activities
    ADD CONSTRAINT activities_activity_uri_key UNIQUE (activity_uri);



ALTER TABLE ONLY public.activities
    ADD CONSTRAINT activities_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.api_tokens
    ADD CONSTRAINT api_tokens_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.api_tokens
    ADD CONSTRAINT api_tokens_token_hash_key UNIQUE (token_hash);



ALTER TABLE ONLY public.asset_alternates
    ADD CONSTRAINT asset_alternates_asset_id_label_key UNIQUE (asset_id, label);



ALTER TABLE ONLY public.asset_alternates
    ADD CONSTRAINT asset_alternates_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.asset_companions
    ADD CONSTRAINT asset_companions_asset_id_companion_path_key UNIQUE (asset_id, companion_path);



ALTER TABLE ONLY public.asset_companions
    ADD CONSTRAINT asset_companions_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.asset_field_value_history
    ADD CONSTRAINT asset_field_value_history_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.asset_field_value
    ADD CONSTRAINT asset_field_value_pkey PRIMARY KEY (asset_id, field_id);



ALTER TABLE ONLY public.asset_tag
    ADD CONSTRAINT asset_tag_pkey PRIMARY KEY (asset_id, tag);



ALTER TABLE ONLY public.asset_type_acls
    ADD CONSTRAINT asset_type_acls_pkey PRIMARY KEY (asset_type_ref, principal_type, principal_id, permission);



ALTER TABLE ONLY public.assets
    ADD CONSTRAINT assets_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.audit_events
    ADD CONSTRAINT audit_events_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.brush_pack_stamps
    ADD CONSTRAINT brush_pack_stamps_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.brush_packs
    ADD CONSTRAINT brush_packs_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.capabilities
    ADD CONSTRAINT capabilities_pkey PRIMARY KEY (code);



ALTER TABLE ONLY public.collection_acls
    ADD CONSTRAINT collection_acls_pkey PRIMARY KEY (collection_id, principal_type, principal_id, permission);



ALTER TABLE ONLY public.collection_posts
    ADD CONSTRAINT collection_posts_pkey PRIMARY KEY (collection_id, post_id);



ALTER TABLE ONLY public.collection_resources
    ADD CONSTRAINT collection_resources_pkey PRIMARY KEY (collection_id, asset_id);



ALTER TABLE ONLY public.collections
    ADD CONSTRAINT collections_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.comments
    ADD CONSTRAINT comments_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.direct_messages
    ADD CONSTRAINT direct_messages_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.federation_directories
    ADD CONSTRAINT federation_directories_directory_url_key UNIQUE (directory_url);



ALTER TABLE ONLY public.federation_directories
    ADD CONSTRAINT federation_directories_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.federation_directory_entries
    ADD CONSTRAINT federation_directory_entries_directory_id_instance_url_key UNIQUE (directory_id, instance_url);



ALTER TABLE ONLY public.federation_directory_entries
    ADD CONSTRAINT federation_directory_entries_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.federation_dispatch_state
    ADD CONSTRAINT federation_dispatch_state_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.federation_inbox
    ADD CONSTRAINT federation_inbox_activity_uri_key UNIQUE (activity_uri);



ALTER TABLE ONLY public.federation_inbox
    ADD CONSTRAINT federation_inbox_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.federation_outbox
    ADD CONSTRAINT federation_outbox_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.federation_peer_suggestions
    ADD CONSTRAINT federation_peer_suggestions_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.federation_peer_suggestions
    ADD CONSTRAINT federation_peer_suggestions_source_peer_id_suggested_url_key UNIQUE (source_peer_id, suggested_url);



ALTER TABLE ONLY public.federation_peers
    ADD CONSTRAINT federation_peers_instance_url_key UNIQUE (instance_url);



ALTER TABLE ONLY public.federation_peers
    ADD CONSTRAINT federation_peers_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.federation_remote_actors
    ADD CONSTRAINT federation_remote_actors_pkey PRIMARY KEY (actor_uri);



ALTER TABLE ONLY public.federation_shares
    ADD CONSTRAINT federation_shares_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.federation_user_keys
    ADD CONSTRAINT federation_user_keys_pkey PRIMARY KEY (user_ref, version);



ALTER TABLE ONLY public.field_definition
    ADD CONSTRAINT field_definition_code_key UNIQUE (code);



ALTER TABLE ONLY public.field_definition
    ADD CONSTRAINT field_definition_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.likes
    ADD CONSTRAINT likes_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.notifications
    ADD CONSTRAINT notifications_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.post_acls
    ADD CONSTRAINT post_acls_pkey PRIMARY KEY (post_id, principal_type, principal_id, permission);



ALTER TABLE ONLY public.post_assets
    ADD CONSTRAINT post_assets_pkey PRIMARY KEY (post_id, asset_id);



ALTER TABLE ONLY public.post_tags
    ADD CONSTRAINT post_tags_pkey PRIMARY KEY (post_id, tag);



ALTER TABLE ONLY public.posts
    ADD CONSTRAINT posts_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.asset_types
    ADD CONSTRAINT resource_type_pkey PRIMARY KEY (ref);



ALTER TABLE ONLY public.role_capabilities
    ADD CONSTRAINT role_capabilities_pkey PRIMARY KEY (role_id, capability_code);



ALTER TABLE ONLY public.roles
    ADD CONSTRAINT roles_name_key UNIQUE (name);



ALTER TABLE ONLY public.roles
    ADD CONSTRAINT roles_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_token_hash_key UNIQUE (token_hash);



ALTER TABLE ONLY public.storage_objects
    ADD CONSTRAINT storage_objects_pkey PRIMARY KEY (hash);



ALTER TABLE ONLY public.storage_pins
    ADD CONSTRAINT storage_pins_pkey PRIMARY KEY (object_hash, pin_subject_type, pin_subject_id);



ALTER TABLE ONLY public.storage_variants
    ADD CONSTRAINT storage_variants_pkey PRIMARY KEY (object_hash, variant_key);



ALTER TABLE ONLY public.system_config
    ADD CONSTRAINT system_config_pkey PRIMARY KEY (key);



ALTER TABLE ONLY public.team_closure
    ADD CONSTRAINT team_closure_pkey PRIMARY KEY (ancestor_id, descendant_id);



ALTER TABLE ONLY public.team_memberships
    ADD CONSTRAINT team_memberships_pkey PRIMARY KEY (team_id, user_ref);



ALTER TABLE ONLY public.team_parents
    ADD CONSTRAINT team_parents_pkey PRIMARY KEY (child_id, parent_id);



ALTER TABLE ONLY public.teams
    ADD CONSTRAINT teams_origin_server_id_slug_key UNIQUE NULLS NOT DISTINCT (origin_server_id, slug);



ALTER TABLE ONLY public.teams
    ADD CONSTRAINT teams_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.user_blocks
    ADD CONSTRAINT user_blocks_pkey PRIMARY KEY (blocker_user_ref, blocked_user_ref);



ALTER TABLE ONLY public.user_capability_grants
    ADD CONSTRAINT user_capability_grants_unique UNIQUE NULLS NOT DISTINCT (user_ref, capability_code, team_id);



ALTER TABLE ONLY public.user_capability_revokes
    ADD CONSTRAINT user_capability_revokes_unique UNIQUE NULLS NOT DISTINCT (user_ref, capability_code, team_id);



ALTER TABLE ONLY public.user_follows
    ADD CONSTRAINT user_follows_pkey PRIMARY KEY (follower_user_ref, followee_user_ref);



ALTER TABLE ONLY public.user_password_history
    ADD CONSTRAINT user_password_history_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public."user"
    ADD CONSTRAINT user_pkey PRIMARY KEY (ref);



ALTER TABLE ONLY public.user_preferences
    ADD CONSTRAINT user_preferences_pkey PRIMARY KEY (user_ref);



ALTER TABLE ONLY public.user_profiles
    ADD CONSTRAINT user_profiles_pkey PRIMARY KEY (user_ref);



ALTER TABLE ONLY public.user_roles
    ADD CONSTRAINT user_roles_unique UNIQUE NULLS NOT DISTINCT (user_ref, role_id, team_id);



ALTER TABLE ONLY public.workflow_audit
    ADD CONSTRAINT workflow_audit_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.workflow_states
    ADD CONSTRAINT workflow_states_domain_code_key UNIQUE (domain, code);



ALTER TABLE ONLY public.workflow_states
    ADD CONSTRAINT workflow_states_pkey PRIMARY KEY (id);



ALTER TABLE ONLY public.workflow_transitions
    ADD CONSTRAINT workflow_transitions_from_state_id_to_state_id_key UNIQUE NULLS NOT DISTINCT (from_state_id, to_state_id);



ALTER TABLE ONLY public.workflow_transitions
    ADD CONSTRAINT workflow_transitions_pkey PRIMARY KEY (id);



CREATE INDEX activities_actor_outbox_idx ON public.activities USING btree (actor_user_ref, published_at DESC, id DESC) WHERE ((source = 'local'::text) AND (actor_user_ref IS NOT NULL));



CREATE INDEX activities_object_recent_idx ON public.activities USING btree (object_kind, object_local_id, published_at DESC) WHERE ((object_kind IS NOT NULL) AND (object_local_id IS NOT NULL));



CREATE INDEX activities_source_recent_idx ON public.activities USING btree (source, published_at DESC) WHERE (source <> 'local'::text);



CREATE INDEX activities_type_recent_idx ON public.activities USING btree (activity_type, published_at DESC);



CREATE INDEX afvh_asset_idx ON public.asset_field_value_history USING btree (asset_id, changed_at DESC);



CREATE INDEX afvh_field_idx ON public.asset_field_value_history USING btree (field_id, changed_at DESC);



CREATE INDEX api_tokens__active_idx ON public.api_tokens USING btree (token_hash) WHERE (revoked_at IS NULL);



CREATE INDEX api_tokens__user_idx ON public.api_tokens USING btree (user_ref);



CREATE INDEX asset_alternates__asset_idx ON public.asset_alternates USING btree (asset_id);



CREATE INDEX asset_alternates__hash_idx ON public.asset_alternates USING btree (object_hash);



CREATE INDEX asset_alternates__kind_idx ON public.asset_alternates USING btree (asset_id, kind);



CREATE INDEX asset_companions__asset_idx ON public.asset_companions USING btree (asset_id);



CREATE INDEX asset_companions__hash_idx ON public.asset_companions USING btree (object_hash);



CREATE INDEX asset_field_value_asset_idx ON public.asset_field_value USING btree (asset_id);



CREATE INDEX asset_field_value_date_idx ON public.asset_field_value USING btree (field_id, value_date) WHERE (value_date IS NOT NULL);



CREATE INDEX asset_field_value_field_idx ON public.asset_field_value USING btree (field_id);



CREATE INDEX asset_field_value_num_idx ON public.asset_field_value USING btree (field_id, value_num) WHERE (value_num IS NOT NULL);



CREATE INDEX asset_field_value_options_gin ON public.asset_field_value USING gin (value_options) WHERE (value_options IS NOT NULL);



CREATE INDEX asset_field_value_ref_idx ON public.asset_field_value USING btree (value_ref) WHERE (value_ref IS NOT NULL);



CREATE INDEX asset_field_value_text_idx ON public.asset_field_value USING btree (field_id, value_text) WHERE (value_text IS NOT NULL);



CREATE INDEX asset_tag_tag_idx ON public.asset_tag USING btree (tag);



CREATE INDEX asset_type_acls_expires_idx ON public.asset_type_acls USING btree (expires_at) WHERE (expires_at IS NOT NULL);



CREATE INDEX asset_type_acls_principal_idx ON public.asset_type_acls USING btree (principal_type, principal_id);



CREATE INDEX assets_created_at_idx ON public.assets USING btree (created_at DESC) WHERE (deleted_at IS NULL);



CREATE INDEX assets_file_hash_idx ON public.assets USING btree (file_hash) WHERE (file_hash IS NOT NULL);



CREATE INDEX assets_metadata_gin ON public.assets USING gin (metadata);



CREATE INDEX assets_owner_idx ON public.assets USING btree (owner_user_ref) WHERE ((deleted_at IS NULL) AND (owner_user_ref IS NOT NULL));



CREATE INDEX assets_processing_status_idx ON public.assets USING btree (processing_status) WHERE (processing_status <> 'ready'::text);



CREATE INDEX assets_search_text_gin ON public.assets USING gin (search_text) WHERE (deleted_at IS NULL);



CREATE INDEX assets_state_idx ON public.assets USING btree (state_id) WHERE (state_id IS NOT NULL);



CREATE INDEX assets_status_idx ON public.assets USING btree (status) WHERE (deleted_at IS NULL);



CREATE INDEX assets_team_idx ON public.assets USING btree (team_id) WHERE (team_id IS NOT NULL);



CREATE INDEX assets_type_idx ON public.assets USING btree (asset_type) WHERE (deleted_at IS NULL);



CREATE INDEX audit_events__subject_time_idx ON public.audit_events USING btree (subject_user_ref, occurred_at DESC) WHERE (subject_user_ref IS NOT NULL);



CREATE INDEX audit_events__type_time_idx ON public.audit_events USING btree (event_type, occurred_at DESC);



CREATE UNIQUE INDEX brush_pack_stamps_pack_abr_uniq ON public.brush_pack_stamps USING btree (pack_id, abr_id) WHERE (abr_id IS NOT NULL);



CREATE INDEX brush_pack_stamps_pack_idx ON public.brush_pack_stamps USING btree (pack_id);



CREATE INDEX brush_packs_owner_idx ON public.brush_packs USING btree (owner_user_ref);



CREATE INDEX collection_acls_expires_idx ON public.collection_acls USING btree (expires_at) WHERE (expires_at IS NOT NULL);



CREATE INDEX collection_acls_principal_idx ON public.collection_acls USING btree (principal_type, principal_id);



CREATE INDEX collection_posts_expires_idx ON public.collection_posts USING btree (expires_at) WHERE (expires_at IS NOT NULL);



CREATE INDEX collection_posts_post_idx ON public.collection_posts USING btree (post_id);



CREATE INDEX collection_posts_sort_idx ON public.collection_posts USING btree (collection_id, sort_order, added_at);



CREATE INDEX collection_resources_asset_idx ON public.collection_resources USING btree (asset_id);



CREATE INDEX collection_resources_expires_idx ON public.collection_resources USING btree (expires_at) WHERE (expires_at IS NOT NULL);



CREATE INDEX collection_resources_sort_idx ON public.collection_resources USING btree (collection_id, sort_order);



CREATE INDEX collections_created_at_idx ON public.collections USING btree (created_at DESC);



CREATE INDEX collections_expires_idx ON public.collections USING btree (expires_at) WHERE (expires_at IS NOT NULL);



CREATE INDEX collections_featured_idx ON public.collections USING btree (featured) WHERE featured;



CREATE INDEX collections_owner_idx ON public.collections USING btree (owner_user_ref);



CREATE INDEX collections_visibility_idx ON public.collections USING btree (visibility);



CREATE UNIQUE INDEX comments_activity_uri_uniq_idx ON public.comments USING btree (activity_uri) WHERE (activity_uri IS NOT NULL);



CREATE INDEX comments_annotation_idx ON public.comments USING btree (target_kind, target_id, annotation_type) WHERE ((annotation_type IS NOT NULL) AND (deleted_at IS NULL));



CREATE INDEX comments_author_idx ON public.comments USING btree (author_user_ref, created_at DESC) WHERE (deleted_at IS NULL);



CREATE INDEX comments_target_active_idx ON public.comments USING btree (target_kind, target_id, created_at DESC) WHERE (deleted_at IS NULL);



CREATE INDEX comments_text_annotations_idx ON public.comments USING btree (target_kind, target_id, created_at DESC) WHERE ((annotation_type = 'text-range'::text) AND (deleted_at IS NULL));



CREATE INDEX comments_thread_idx ON public.comments USING btree (root_id, depth, created_at) WHERE (deleted_at IS NULL);



CREATE INDEX comments_whiteboards_idx ON public.comments USING btree (target_kind, target_id, created_at DESC) WHERE ((annotation_type = 'whiteboard'::text) AND (deleted_at IS NULL));



CREATE INDEX federation_directories_due_idx ON public.federation_directories USING btree (last_polled_at NULLS FIRST) WHERE (enabled = true);



CREATE INDEX federation_directory_entries_by_dir_idx ON public.federation_directory_entries USING btree (directory_id, verified_at DESC);



CREATE INDEX federation_directory_entries_by_url_idx ON public.federation_directory_entries USING btree (instance_url);



CREATE INDEX federation_inbox_by_peer_idx ON public.federation_inbox USING btree (peer_id, received_at DESC);



CREATE INDEX federation_inbox_by_status_idx ON public.federation_inbox USING btree (status, received_at DESC);



CREATE INDEX federation_inbox_pending_idx ON public.federation_inbox USING btree (received_at) WHERE (status = 'pending'::text);



CREATE INDEX federation_outbox_by_peer_idx ON public.federation_outbox USING btree (peer_id, created_at DESC);



CREATE UNIQUE INDEX federation_outbox_dedup_broadcast_idx ON public.federation_outbox USING btree (activity_id, peer_id) WHERE (target_user_url IS NULL);



CREATE UNIQUE INDEX federation_outbox_dedup_targeted_idx ON public.federation_outbox USING btree (activity_id, peer_id, target_user_url) WHERE (target_user_url IS NOT NULL);



CREATE INDEX federation_outbox_due_idx ON public.federation_outbox USING btree (next_attempt_at) WHERE (status = 'queued'::text);



CREATE INDEX federation_peer_suggestions_by_source_idx ON public.federation_peer_suggestions USING btree (source_peer_id, cached_at DESC);



CREATE INDEX federation_peer_suggestions_by_url_idx ON public.federation_peer_suggestions USING btree (suggested_url);



CREATE INDEX federation_peers_enabled_idx ON public.federation_peers USING btree (instance_url) WHERE ((enabled = true) AND (status = 'connected'::text));



CREATE INDEX federation_peers_handshake_at_idx ON public.federation_peers USING btree (handshake_at DESC);



CREATE INDEX federation_peers_pending_inbound_idx ON public.federation_peers USING btree (handshake_at DESC) WHERE (status = 'pending_inbound'::text);



CREATE INDEX federation_peers_unnegotiated_idx ON public.federation_peers USING btree (id) WHERE (capabilities_negotiated_at IS NULL);



CREATE INDEX federation_peers_visible_idx ON public.federation_peers USING btree (instance_url) WHERE ((enabled = true) AND (status = 'connected'::text) AND (share_in_visible_list = true));



CREATE INDEX federation_remote_actors_by_peer_idx ON public.federation_remote_actors USING btree (peer_id, last_seen_at DESC);



CREATE INDEX federation_remote_actors_missing_encryption_key_idx ON public.federation_remote_actors USING btree (peer_id) WHERE (encryption_public_key IS NULL);



CREATE UNIQUE INDEX federation_shares_active_uniq_idx ON public.federation_shares USING btree (grantor_user_ref, object_kind, object_id, peer_id, COALESCE(target_user_url, ''::text)) WHERE (revoked_at IS NULL);



CREATE INDEX federation_shares_by_grantor_idx ON public.federation_shares USING btree (grantor_user_ref, granted_at DESC) WHERE (revoked_at IS NULL);



CREATE INDEX federation_shares_by_peer_idx ON public.federation_shares USING btree (peer_id) WHERE (revoked_at IS NULL);



CREATE INDEX federation_shares_delivery_idx ON public.federation_shares USING btree (object_kind, object_id) WHERE (revoked_at IS NULL);



CREATE INDEX federation_shares_expiring_idx ON public.federation_shares USING btree (expires_at) WHERE ((revoked_at IS NULL) AND (expires_at IS NOT NULL));



CREATE INDEX federation_shares_lookup_idx ON public.federation_shares USING btree (object_kind, object_id, peer_id) WHERE (revoked_at IS NULL);



CREATE UNIQUE INDEX federation_user_keys_one_current_idx ON public.federation_user_keys USING btree (user_ref) WHERE (is_current = true);



CREATE INDEX federation_user_keys_retained_idx ON public.federation_user_keys USING btree (retained_until) WHERE (retained_until IS NOT NULL);



CREATE INDEX field_definition_applies_to_gin ON public.field_definition USING gin (applies_to);



CREATE INDEX field_definition_group_idx ON public.field_definition USING btree (display_group, display_order);



CREATE INDEX field_definition_options_gin ON public.field_definition USING gin (options);



CREATE INDEX field_definition_status_idx ON public.field_definition USING btree (status) WHERE (status = 'active'::text);



CREATE INDEX idx_assets_sensitivity_restricted ON public.assets USING btree (sensitivity) WHERE (sensitivity = ANY (ARRAY['restricted'::text, 'embargo'::text]));



CREATE INDEX idx_dm_recipient_recent ON public.direct_messages USING btree (recipient_user_ref, sent_at DESC);



CREATE INDEX idx_dm_sender_recent ON public.direct_messages USING btree (sender_user_ref, sent_at DESC);



CREATE INDEX idx_dm_unread ON public.direct_messages USING btree (recipient_user_ref) WHERE (read_at IS NULL);



CREATE INDEX idx_notifications_actor ON public.notifications USING btree (actor_user_ref, created_at DESC) WHERE (actor_user_ref IS NOT NULL);



CREATE INDEX idx_notifications_recipient_recent ON public.notifications USING btree (recipient_user_ref, created_at DESC, id DESC);



CREATE INDEX idx_notifications_unread ON public.notifications USING btree (recipient_user_ref) WHERE (read_at IS NULL);



CREATE INDEX idx_user_blocks_blocked ON public.user_blocks USING btree (blocked_user_ref);



CREATE INDEX idx_user_follows_followee ON public.user_follows USING btree (followee_user_ref, created_at DESC);



CREATE INDEX jobs_lease_expiry_idx ON public.jobs USING btree (lease_expires_at) WHERE (status = 'running'::text);



CREATE INDEX jobs_pending_idx ON public.jobs USING btree (priority, enqueued_at) WHERE (status = 'pending'::text);



CREATE INDEX jobs_type_status_idx ON public.jobs USING btree (type, status);



CREATE UNIQUE INDEX likes_local_uniq_idx ON public.likes USING btree (target_kind, target_id, user_ref) WHERE (user_ref IS NOT NULL);



CREATE UNIQUE INDEX likes_remote_uniq_idx ON public.likes USING btree (target_kind, target_id, peer_id, actor_uri) WHERE (peer_id IS NOT NULL);



CREATE INDEX likes_target_idx ON public.likes USING btree (target_kind, target_id);



CREATE INDEX likes_user_idx ON public.likes USING btree (user_ref, liked_at DESC);



CREATE INDEX post_acls_expires_idx ON public.post_acls USING btree (expires_at) WHERE (expires_at IS NOT NULL);



CREATE INDEX post_acls_principal_idx ON public.post_acls USING btree (principal_type, principal_id);



CREATE INDEX post_assets_asset_idx ON public.post_assets USING btree (asset_id);



CREATE INDEX post_assets_sort_idx ON public.post_assets USING btree (post_id, sort_order, added_at);



CREATE INDEX post_tags_tag_idx ON public.post_tags USING btree (tag);



CREATE INDEX posts_author_idx ON public.posts USING btree (author_user_ref, posted_at DESC) WHERE (deleted_at IS NULL);



CREATE INDEX posts_cover_thumbnail_idx ON public.posts USING btree (cover_thumbnail_asset_id) WHERE (cover_thumbnail_asset_id IS NOT NULL);



CREATE INDEX posts_public_feed_idx ON public.posts USING btree (posted_at DESC) WHERE ((deleted_at IS NULL) AND (visibility = 'public'::text));



CREATE INDEX posts_search_text_gin ON public.posts USING gin (search_text) WHERE (deleted_at IS NULL);



CREATE INDEX posts_state_idx ON public.posts USING btree (state_id) WHERE (state_id IS NOT NULL);



CREATE INDEX posts_team_idx ON public.posts USING btree (team_id) WHERE (team_id IS NOT NULL);



CREATE INDEX posts_visibility_idx ON public.posts USING btree (visibility) WHERE (deleted_at IS NULL);



CREATE INDEX roles__parent_idx ON public.roles USING btree (parent_id);



CREATE INDEX sessions__last_used_idx ON public.sessions USING btree (last_used_at) WHERE (revoked_at IS NULL);



CREATE INDEX sessions__user_ref_idx ON public.sessions USING btree (user_ref) WHERE (revoked_at IS NULL);



CREATE INDEX storage_objects__gc_idx ON public.storage_objects USING btree (gc_eligible_at) WHERE (gc_eligible_at IS NOT NULL);



CREATE INDEX storage_pins__subject_idx ON public.storage_pins USING btree (pin_subject_type, pin_subject_id);



CREATE INDEX team_closure_descendant_idx ON public.team_closure USING btree (descendant_id);



CREATE INDEX team_memberships_user_idx ON public.team_memberships USING btree (user_ref);



CREATE INDEX team_parents_parent_idx ON public.team_parents USING btree (parent_id);



CREATE INDEX teams_active_idx ON public.teams USING btree (id) WHERE (deleted_at IS NULL);



CREATE INDEX teams_origin_idx ON public.teams USING btree (origin_server_id) WHERE (origin_server_id IS NOT NULL);



CREATE UNIQUE INDEX user_actor_uri_idx ON public."user" USING btree (actor_uri) WHERE (actor_uri IS NOT NULL);



CREATE INDEX user_approved_idx ON public."user" USING btree (approved);



CREATE INDEX user_capability_grants_team_idx ON public.user_capability_grants USING btree (team_id) WHERE (team_id IS NOT NULL);



CREATE INDEX user_capability_grants_user_idx ON public.user_capability_grants USING btree (user_ref);



CREATE INDEX user_capability_revokes_team_idx ON public.user_capability_revokes USING btree (team_id) WHERE (team_id IS NOT NULL);



CREATE INDEX user_capability_revokes_user_idx ON public.user_capability_revokes USING btree (user_ref);



CREATE INDEX user_created_ref_desc_idx ON public."user" USING btree (created DESC NULLS LAST, ref DESC);



CREATE INDEX user_email_lower_idx ON public."user" USING btree (lower((email)::text));



CREATE INDEX user_fullname_lower_idx ON public."user" USING btree (lower((fullname)::text));



CREATE INDEX user_password_history_user_changed_idx ON public.user_password_history USING btree (user_ref, changed_at DESC);



CREATE INDEX user_profiles_origin_idx ON public.user_profiles USING btree (origin_server_id) WHERE (origin_server_id IS NOT NULL);



CREATE INDEX user_roles_role_idx ON public.user_roles USING btree (role_id);



CREATE INDEX user_roles_team_idx ON public.user_roles USING btree (team_id) WHERE (team_id IS NOT NULL);



CREATE INDEX user_roles_user_idx ON public.user_roles USING btree (user_ref);



CREATE INDEX user_session_idx ON public."user" USING btree (session);



CREATE INDEX user_username_lower_idx ON public."user" USING btree (lower((username)::text));



CREATE UNIQUE INDEX user_username_uniq_idx ON public."user" USING btree (username);



CREATE INDEX workflow_audit_resource_idx ON public.workflow_audit USING btree (resource_kind, resource_id, transitioned_at DESC);



CREATE INDEX workflow_states_domain_idx ON public.workflow_states USING btree (domain, sort_order);



CREATE UNIQUE INDEX workflow_states_one_initial_per_domain ON public.workflow_states USING btree (domain) WHERE is_initial;



CREATE INDEX workflow_transitions_from_idx ON public.workflow_transitions USING btree (from_state_id);



CREATE INDEX workflow_transitions_to_idx ON public.workflow_transitions USING btree (to_state_id);



CREATE TRIGGER acl_sweep_after_role_delete AFTER DELETE ON public.roles FOR EACH ROW EXECUTE FUNCTION public.acl_sweep_on_role_delete();



CREATE TRIGGER acl_sweep_after_team_delete AFTER DELETE ON public.teams FOR EACH ROW EXECUTE FUNCTION public.acl_sweep_on_team_delete();



CREATE TRIGGER asset_field_value_search_text AFTER INSERT OR DELETE OR UPDATE ON public.asset_field_value FOR EACH ROW EXECUTE FUNCTION public.asset_field_value_trigger();



CREATE TRIGGER asset_type_acl_sweep_after_role_delete AFTER DELETE ON public.roles FOR EACH ROW EXECUTE FUNCTION public.asset_type_acl_sweep_on_role_delete();



CREATE TRIGGER asset_type_acl_sweep_after_team_delete AFTER DELETE ON public.teams FOR EACH ROW EXECUTE FUNCTION public.asset_type_acl_sweep_on_team_delete();



CREATE TRIGGER assets_search_text_trigger AFTER INSERT OR UPDATE ON public.assets FOR EACH ROW EXECUTE FUNCTION public.asset_changed_trigger();



CREATE TRIGGER comments_maintain_counter_delete AFTER DELETE ON public.comments FOR EACH ROW EXECUTE FUNCTION public.comments_after_delete();



CREATE TRIGGER comments_maintain_counter_insert AFTER INSERT ON public.comments FOR EACH ROW EXECUTE FUNCTION public.comments_after_insert();



CREATE TRIGGER comments_maintain_counter_update AFTER UPDATE OF deleted_at ON public.comments FOR EACH ROW EXECUTE FUNCTION public.comments_after_update();



CREATE TRIGGER federation_dispatch_notify_trg AFTER INSERT ON public.activities FOR EACH ROW EXECUTE FUNCTION public.federation_dispatch_notify();



CREATE TRIGGER federation_inbox_dispatch_notify_trg AFTER INSERT ON public.federation_inbox FOR EACH ROW EXECUTE FUNCTION public.federation_inbox_dispatch_notify();



CREATE TRIGGER federation_outbox_dispatch_notify_trg AFTER INSERT ON public.federation_outbox FOR EACH ROW EXECUTE FUNCTION public.federation_outbox_dispatch_notify();



CREATE TRIGGER likes_maintain_counter_delete AFTER DELETE ON public.likes FOR EACH ROW EXECUTE FUNCTION public.likes_after_delete();



CREATE TRIGGER likes_maintain_counter_insert AFTER INSERT ON public.likes FOR EACH ROW EXECUTE FUNCTION public.likes_after_insert();



CREATE TRIGGER post_assets_search_text AFTER INSERT OR DELETE OR UPDATE ON public.post_assets FOR EACH ROW EXECUTE FUNCTION public.post_assets_search_text_trigger();



CREATE TRIGGER post_tags_search_text AFTER INSERT OR DELETE OR UPDATE ON public.post_tags FOR EACH ROW EXECUTE FUNCTION public.post_tags_search_text_trigger();



CREATE TRIGGER posts_search_text AFTER INSERT OR UPDATE OF title, description ON public.posts FOR EACH ROW EXECUTE FUNCTION public.posts_search_text_trigger();



CREATE TRIGGER social_sweep_after_post_delete AFTER DELETE ON public.posts FOR EACH ROW EXECUTE FUNCTION public.social_sweep_on_post_delete();



CREATE TRIGGER team_parents_propagate_closure AFTER INSERT ON public.team_parents FOR EACH ROW EXECUTE FUNCTION public.team_parents_after_insert();



CREATE TRIGGER team_parents_rebuild_on_delete AFTER DELETE ON public.team_parents FOR EACH ROW EXECUTE FUNCTION public.team_parents_after_delete();



CREATE TRIGGER team_parents_reject_cycle BEFORE INSERT ON public.team_parents FOR EACH ROW EXECUTE FUNCTION public.team_parents_before_insert();



CREATE TRIGGER teams_self_closure AFTER INSERT ON public.teams FOR EACH ROW EXECUTE FUNCTION public.teams_insert_self_closure();



ALTER TABLE ONLY public.asset_alternates
    ADD CONSTRAINT asset_alternates_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.asset_alternates
    ADD CONSTRAINT asset_alternates_object_hash_fkey FOREIGN KEY (object_hash) REFERENCES public.storage_objects(hash) ON DELETE RESTRICT;



ALTER TABLE ONLY public.asset_companions
    ADD CONSTRAINT asset_companions_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.asset_companions
    ADD CONSTRAINT asset_companions_object_hash_fkey FOREIGN KEY (object_hash) REFERENCES public.storage_objects(hash) ON DELETE RESTRICT;



ALTER TABLE ONLY public.asset_field_value
    ADD CONSTRAINT asset_field_value_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.asset_field_value
    ADD CONSTRAINT asset_field_value_field_id_fkey FOREIGN KEY (field_id) REFERENCES public.field_definition(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.asset_tag
    ADD CONSTRAINT asset_tag_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.asset_type_acls
    ADD CONSTRAINT asset_type_acls_asset_type_ref_fkey FOREIGN KEY (asset_type_ref) REFERENCES public.asset_types(ref) ON DELETE CASCADE;



ALTER TABLE ONLY public.assets
    ADD CONSTRAINT assets_asset_type_fkey FOREIGN KEY (asset_type) REFERENCES public.asset_types(ref) ON DELETE RESTRICT;



ALTER TABLE ONLY public.assets
    ADD CONSTRAINT assets_file_hash_fkey FOREIGN KEY (file_hash) REFERENCES public.storage_objects(hash) ON DELETE SET NULL;



ALTER TABLE ONLY public.assets
    ADD CONSTRAINT assets_state_id_fkey FOREIGN KEY (state_id) REFERENCES public.workflow_states(id) ON DELETE SET NULL;



ALTER TABLE ONLY public.assets
    ADD CONSTRAINT assets_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE SET NULL;



ALTER TABLE ONLY public.brush_pack_stamps
    ADD CONSTRAINT brush_pack_stamps_pack_id_fkey FOREIGN KEY (pack_id) REFERENCES public.brush_packs(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.brush_packs
    ADD CONSTRAINT brush_packs_owner_user_ref_fkey FOREIGN KEY (owner_user_ref) REFERENCES public."user"(ref) ON DELETE CASCADE;



ALTER TABLE ONLY public.collection_acls
    ADD CONSTRAINT collection_acls_collection_id_fkey FOREIGN KEY (collection_id) REFERENCES public.collections(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.collection_posts
    ADD CONSTRAINT collection_posts_collection_id_fkey FOREIGN KEY (collection_id) REFERENCES public.collections(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.collection_posts
    ADD CONSTRAINT collection_posts_post_id_fkey FOREIGN KEY (post_id) REFERENCES public.posts(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.collection_resources
    ADD CONSTRAINT collection_resources_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.collection_resources
    ADD CONSTRAINT collection_resources_collection_id_fkey FOREIGN KEY (collection_id) REFERENCES public.collections(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.comments
    ADD CONSTRAINT comments_parent_id_fkey FOREIGN KEY (parent_id) REFERENCES public.comments(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.comments
    ADD CONSTRAINT comments_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.federation_directory_entries
    ADD CONSTRAINT federation_directory_entries_directory_id_fkey FOREIGN KEY (directory_id) REFERENCES public.federation_directories(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.federation_inbox
    ADD CONSTRAINT federation_inbox_correlation_activity_id_fkey FOREIGN KEY (correlation_activity_id) REFERENCES public.activities(id) ON DELETE SET NULL;



ALTER TABLE ONLY public.federation_inbox
    ADD CONSTRAINT federation_inbox_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.federation_outbox
    ADD CONSTRAINT federation_outbox_activity_id_fkey FOREIGN KEY (activity_id) REFERENCES public.activities(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.federation_outbox
    ADD CONSTRAINT federation_outbox_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.federation_peer_suggestions
    ADD CONSTRAINT federation_peer_suggestions_source_peer_id_fkey FOREIGN KEY (source_peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.federation_remote_actors
    ADD CONSTRAINT federation_remote_actors_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.federation_shares
    ADD CONSTRAINT federation_shares_granted_activity_id_fkey FOREIGN KEY (granted_activity_id) REFERENCES public.activities(id) ON DELETE RESTRICT;



ALTER TABLE ONLY public.federation_shares
    ADD CONSTRAINT federation_shares_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.federation_shares
    ADD CONSTRAINT federation_shares_revoked_activity_id_fkey FOREIGN KEY (revoked_activity_id) REFERENCES public.activities(id) ON DELETE RESTRICT;



ALTER TABLE ONLY public.federation_user_keys
    ADD CONSTRAINT federation_user_keys_rotated_by_user_ref_fkey FOREIGN KEY (rotated_by_user_ref) REFERENCES public."user"(ref) ON DELETE SET NULL;



ALTER TABLE ONLY public.federation_user_keys
    ADD CONSTRAINT federation_user_keys_user_ref_fkey FOREIGN KEY (user_ref) REFERENCES public."user"(ref) ON DELETE CASCADE;



ALTER TABLE ONLY public.field_definition
    ADD CONSTRAINT field_definition_deprecated_replacement_id_fkey FOREIGN KEY (deprecated_replacement_id) REFERENCES public.field_definition(id) ON DELETE RESTRICT;



ALTER TABLE ONLY public.likes
    ADD CONSTRAINT likes_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.post_acls
    ADD CONSTRAINT post_acls_post_id_fkey FOREIGN KEY (post_id) REFERENCES public.posts(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.post_assets
    ADD CONSTRAINT post_assets_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.post_assets
    ADD CONSTRAINT post_assets_post_id_fkey FOREIGN KEY (post_id) REFERENCES public.posts(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.post_tags
    ADD CONSTRAINT post_tags_post_id_fkey FOREIGN KEY (post_id) REFERENCES public.posts(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.posts
    ADD CONSTRAINT posts_cover_asset_id_fkey FOREIGN KEY (cover_asset_id) REFERENCES public.assets(id) ON DELETE SET NULL;



ALTER TABLE ONLY public.posts
    ADD CONSTRAINT posts_cover_thumbnail_asset_id_fkey FOREIGN KEY (cover_thumbnail_asset_id) REFERENCES public.assets(id) ON DELETE SET NULL;



ALTER TABLE ONLY public.posts
    ADD CONSTRAINT posts_state_id_fkey FOREIGN KEY (state_id) REFERENCES public.workflow_states(id) ON DELETE SET NULL;



ALTER TABLE ONLY public.posts
    ADD CONSTRAINT posts_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE SET NULL;



ALTER TABLE ONLY public.role_capabilities
    ADD CONSTRAINT role_capabilities_capability_code_fkey FOREIGN KEY (capability_code) REFERENCES public.capabilities(code) ON DELETE CASCADE;



ALTER TABLE ONLY public.role_capabilities
    ADD CONSTRAINT role_capabilities_role_id_fkey FOREIGN KEY (role_id) REFERENCES public.roles(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.roles
    ADD CONSTRAINT roles_parent_id_fkey FOREIGN KEY (parent_id) REFERENCES public.roles(id) ON DELETE SET NULL;



ALTER TABLE ONLY public.storage_pins
    ADD CONSTRAINT storage_pins_object_hash_fkey FOREIGN KEY (object_hash) REFERENCES public.storage_objects(hash) ON DELETE RESTRICT;



ALTER TABLE ONLY public.storage_variants
    ADD CONSTRAINT storage_variants_object_hash_fkey FOREIGN KEY (object_hash) REFERENCES public.storage_objects(hash) ON DELETE CASCADE;



ALTER TABLE ONLY public.team_closure
    ADD CONSTRAINT team_closure_ancestor_id_fkey FOREIGN KEY (ancestor_id) REFERENCES public.teams(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.team_closure
    ADD CONSTRAINT team_closure_descendant_id_fkey FOREIGN KEY (descendant_id) REFERENCES public.teams(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.team_memberships
    ADD CONSTRAINT team_memberships_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.team_parents
    ADD CONSTRAINT team_parents_child_id_fkey FOREIGN KEY (child_id) REFERENCES public.teams(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.team_parents
    ADD CONSTRAINT team_parents_parent_id_fkey FOREIGN KEY (parent_id) REFERENCES public.teams(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.user_capability_grants
    ADD CONSTRAINT user_capability_grants_capability_code_fkey FOREIGN KEY (capability_code) REFERENCES public.capabilities(code) ON DELETE CASCADE;



ALTER TABLE ONLY public.user_capability_grants
    ADD CONSTRAINT user_capability_grants_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.user_capability_revokes
    ADD CONSTRAINT user_capability_revokes_capability_code_fkey FOREIGN KEY (capability_code) REFERENCES public.capabilities(code) ON DELETE CASCADE;



ALTER TABLE ONLY public.user_capability_revokes
    ADD CONSTRAINT user_capability_revokes_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.user_roles
    ADD CONSTRAINT user_role_role_id_fkey FOREIGN KEY (role_id) REFERENCES public.roles(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.user_roles
    ADD CONSTRAINT user_roles_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.workflow_audit
    ADD CONSTRAINT workflow_audit_from_state_id_fkey FOREIGN KEY (from_state_id) REFERENCES public.workflow_states(id) ON DELETE SET NULL;



ALTER TABLE ONLY public.workflow_audit
    ADD CONSTRAINT workflow_audit_to_state_id_fkey FOREIGN KEY (to_state_id) REFERENCES public.workflow_states(id) ON DELETE SET NULL;



ALTER TABLE ONLY public.workflow_transitions
    ADD CONSTRAINT workflow_transitions_from_state_id_fkey FOREIGN KEY (from_state_id) REFERENCES public.workflow_states(id) ON DELETE CASCADE;



ALTER TABLE ONLY public.workflow_transitions
    ADD CONSTRAINT workflow_transitions_required_capability_fkey FOREIGN KEY (required_capability) REFERENCES public.capabilities(code) ON DELETE SET NULL;



ALTER TABLE ONLY public.workflow_transitions
    ADD CONSTRAINT workflow_transitions_to_state_id_fkey FOREIGN KEY (to_state_id) REFERENCES public.workflow_states(id) ON DELETE CASCADE;


--
--



-- F-022 (cleanup-audit-2026-06.md): document that the _ref
-- suffix on asset_field_value.value_ref follows the table's
-- local multi-type value-column convention (sibling to
-- value_text / value_num / value_date / value_options), NOT
-- the schema-wide BIGINT-FK _ref rule. The column is a UUID
-- storing a domain-object reference for ref-typed field values.
COMMENT ON COLUMN public.asset_field_value.value_ref IS
    'UUID reference value for ref-typed fields. The _ref suffix follows the table''s local multi-type value-column convention (sibling to value_text / value_num / value_date / value_options), distinct from the schema-wide BIGINT-FK _ref rule.';

-- Catalog seed data (from the pre-squash 00002_seeds.sql).
-- Order respects FK dependencies; Postgres defers FK checks
-- within a transaction by default for these tables.

INSERT INTO public.asset_types VALUES (2, 'Document', NULL, 20, NULL, NULL, NULL, 'file-text', NULL, NULL);
INSERT INTO public.asset_types VALUES (3, 'Video', NULL, 30, NULL, NULL, NULL, 'video', NULL, NULL);
INSERT INTO public.asset_types VALUES (4, 'Audio', NULL, 40, NULL, NULL, NULL, 'music', NULL, NULL);
INSERT INTO public.asset_types VALUES (5, '3D Object', NULL, 50, NULL, NULL, NULL, 'box', NULL, NULL);
INSERT INTO public.asset_types VALUES (6, 'Archive', NULL, 60, NULL, NULL, NULL, 'archive', NULL, NULL);
INSERT INTO public.asset_types VALUES (7, 'Font', NULL, 70, NULL, NULL, NULL, 'type', NULL, NULL);
INSERT INTO public.asset_types VALUES (1, 'Image', NULL, 10, NULL, NULL, NULL, 'image', NULL, NULL);
INSERT INTO public.asset_types VALUES (8, 'Comic', NULL, 80, NULL, NULL, NULL, 'book-open', NULL, NULL);
INSERT INTO public.asset_types VALUES (10, 'Ebook', NULL, 100, NULL, NULL, NULL, 'book', NULL, NULL);
INSERT INTO public.asset_types VALUES (11, 'Audiobook', NULL, 110, NULL, NULL, NULL, 'headphones', NULL, NULL);
INSERT INTO public.asset_types VALUES (12, 'Texture', NULL, 120, NULL, NULL, NULL, 'grid-3x3', NULL, NULL);
INSERT INTO public.asset_types VALUES (13, 'Sprite', NULL, 130, NULL, NULL, NULL, 'grid-2x2', NULL, NULL);
INSERT INTO public.asset_types VALUES (14, 'Code', NULL, 140, NULL, NULL, NULL, 'file-code-2', NULL, NULL);

INSERT INTO public.capabilities VALUES ('system.admin', 'Superpower — bypasses every capability check', '2026-06-06 18:04:06.825478+00', NULL);
INSERT INTO public.capabilities VALUES ('users.read', 'Read other users'' profiles and metadata', '2026-06-06 18:04:06.825478+00', NULL);
INSERT INTO public.capabilities VALUES ('users.write', 'Modify other users (role, capability grants/revokes)', '2026-06-06 18:04:06.825478+00', NULL);
INSERT INTO public.capabilities VALUES ('roles.read', 'List available roles and their capabilities', '2026-06-06 18:04:06.825478+00', NULL);
INSERT INTO public.capabilities VALUES ('caps.read', 'List defined capability codes', '2026-06-06 18:04:06.825478+00', NULL);
INSERT INTO public.capabilities VALUES ('teams.read', 'List teams and view team membership', '2026-06-06 18:04:07.512666+00', NULL);
INSERT INTO public.capabilities VALUES ('teams.create', 'Create new teams', '2026-06-06 18:04:07.512666+00', NULL);
INSERT INTO public.capabilities VALUES ('teams.admin', 'Edit any team (rename, re-parent, delete, manage members)', '2026-06-06 18:04:07.512666+00', NULL);
INSERT INTO public.capabilities VALUES ('workflow.admin', 'Manage workflow_states and workflow_transitions', '2026-06-06 18:04:07.602388+00', NULL);
INSERT INTO public.capabilities VALUES ('posts.publish', 'Move a post into the published state', '2026-06-06 18:04:07.602388+00', NULL);
INSERT INTO public.capabilities VALUES ('assets.submit', 'Submit an asset for review (draft → pending_review)', '2026-06-06 18:04:07.602388+00', NULL);
INSERT INTO public.capabilities VALUES ('assets.review', 'Approve or reject an asset in review (pending_review → published)', '2026-06-06 18:04:07.602388+00', NULL);
INSERT INTO public.capabilities VALUES ('assets.publish', 'Publish an asset directly without review (draft → published)', '2026-06-06 18:04:07.602388+00', NULL);
INSERT INTO public.capabilities VALUES ('assets.archive', 'Archive a published asset (published → archived)', '2026-06-06 18:04:07.602388+00', NULL);
INSERT INTO public.capabilities VALUES ('assets.unarchive', 'Restore an archived asset (archived → published)', '2026-06-06 18:04:07.602388+00', NULL);
INSERT INTO public.capabilities VALUES ('posts.comment', 'Write comments on posts', '2026-06-06 18:04:07.650956+00', NULL);
INSERT INTO public.capabilities VALUES ('posts.like', 'Like (and unlike) posts and comments', '2026-06-06 18:04:07.650956+00', NULL);
INSERT INTO public.capabilities VALUES ('comments.delete.own', 'Delete a comment you authored', '2026-06-06 18:04:07.650956+00', NULL);
INSERT INTO public.capabilities VALUES ('comments.delete.any', 'Delete any comment (moderator)', '2026-06-06 18:04:07.650956+00', NULL);
INSERT INTO public.capabilities VALUES ('users.profile.edit.any', 'Edit any user''s profile (moderator)', '2026-06-06 18:04:07.681466+00', NULL);
INSERT INTO public.capabilities VALUES ('system.config.read', 'View system configuration (site, SMTP, auth, AI providers).', '2026-06-06 18:04:07.700808+00', NULL);
INSERT INTO public.capabilities VALUES ('system.config.write', 'Modify system configuration.', '2026-06-06 18:04:07.700808+00', NULL);
INSERT INTO public.capabilities VALUES ('system.auth.write', 'Modify authentication / SSO configuration.', '2026-06-06 18:04:07.700808+00', NULL);
INSERT INTO public.capabilities VALUES ('system.ai.write', 'Modify AI provider configuration.', '2026-06-06 18:04:07.700808+00', NULL);
INSERT INTO public.capabilities VALUES ('system.appearance.write', 'Modify the per-install brand and typography settings.', '2026-06-06 18:04:07.703148+00', NULL);
INSERT INTO public.capabilities VALUES ('users.approve', 'Approve, suspend, or restore user accounts (lifecycle state machine)', '2026-06-06 18:04:07.809993+00', NULL);
INSERT INTO public.capabilities VALUES ('users.password.reset', 'Issue a one-shot password reset for any user (admin helpdesk action)', '2026-06-06 18:04:07.81184+00', NULL);
INSERT INTO public.capabilities VALUES ('system.asset_types.admin', 'Edit asset_type definitions and manage their per-type ACLs', '2026-06-06 18:04:07.818966+00', NULL);
INSERT INTO public.capabilities VALUES ('system.audit.read', 'Read the system-wide audit event log via the admin viewer', '2026-06-06 18:04:07.833355+00', NULL);
INSERT INTO public.capabilities VALUES ('system.sso.ldap.read', 'View LDAP/AD identity-provider configuration', '2026-06-06 18:04:07.836887+00', 'sso_ldap');
INSERT INTO public.capabilities VALUES ('system.sso.ldap.write', 'Configure LDAP/AD identity-provider connections', '2026-06-06 18:04:07.836887+00', 'sso_ldap');
INSERT INTO public.capabilities VALUES ('system.sso.saml.read', 'View SAML 2.0 IdP trust configuration', '2026-06-06 18:04:07.836887+00', 'sso_saml');
INSERT INTO public.capabilities VALUES ('system.sso.saml.write', 'Configure SAML 2.0 IdP trust + service-provider metadata', '2026-06-06 18:04:07.836887+00', 'sso_saml');
INSERT INTO public.capabilities VALUES ('system.tenancy.read', 'View multi-tenant deployment configuration', '2026-06-06 18:04:07.836887+00', 'multi_tenant');
INSERT INTO public.capabilities VALUES ('system.tenancy.write', 'Manage tenants, quotas, and per-tenant administration', '2026-06-06 18:04:07.836887+00', 'multi_tenant');

INSERT INTO public.field_definition VALUES ('7cf56f14-f68b-43ef-9b52-ca349bb836b5', 'title', 'Title', 'Primary display title for the asset.', 'text', '{}', true, true, '{}', NULL, NULL, NULL, 10, 'core', '{"tag": "ObjectName", "type": "iptc"}', 'active', NULL, NULL, '2026-06-06 18:04:07.364619+00', '2026-06-06 18:04:07.364619+00', NULL, NULL);
INSERT INTO public.field_definition VALUES ('42b3bb29-8807-4946-abc8-fdaa3d890f50', 'description', 'Description', 'Long-form description of the work.', 'longtext', '{}', false, true, '{}', NULL, NULL, NULL, 20, 'core', NULL, 'active', NULL, NULL, '2026-06-06 18:04:07.364619+00', '2026-06-06 18:04:07.364619+00', NULL, NULL);
INSERT INTO public.field_definition VALUES ('e86aa34e-820f-4a29-93cb-01d5d6a2141f', 'credit', 'Credit', 'Person or studio credited for the work.', 'text', '{}', false, true, '{}', NULL, NULL, NULL, 10, 'rights', '{"tag": "Credit", "type": "iptc"}', 'active', NULL, NULL, '2026-06-06 18:04:07.364619+00', '2026-06-06 18:04:07.364619+00', NULL, NULL);
INSERT INTO public.field_definition VALUES ('ccbdecf3-bddc-4316-928b-3e7333378cd9', 'copyright', 'Copyright', 'Copyright notice / rights statement.', 'text', '{}', false, true, '{}', NULL, NULL, NULL, 20, 'rights', '{"tag": "dc:rights", "type": "xmp"}', 'active', NULL, NULL, '2026-06-06 18:04:07.364619+00', '2026-06-06 18:04:07.364619+00', NULL, NULL);
INSERT INTO public.field_definition VALUES ('e4bc6773-025a-4277-a0b8-a0b71163134a', 'capture_date', 'Capture date', 'When the original was captured (EXIF).', 'datetime', '{}', false, true, '{}', NULL, NULL, NULL, 10, 'technical', '{"tag": "DateTimeOriginal", "type": "exif"}', 'active', NULL, NULL, '2026-06-06 18:04:07.364619+00', '2026-06-06 18:04:07.364619+00', NULL, NULL);
INSERT INTO public.field_definition VALUES ('7d8c0ca9-aff8-482f-8d20-348304aeec75', 'keywords', 'Keywords', 'Multi-value tagging.', 'multi_select', '{}', false, true, '{}', NULL, NULL, NULL, 30, 'core', '{"tag": "Keywords", "type": "iptc"}', 'active', NULL, NULL, '2026-06-06 18:04:07.364619+00', '2026-06-06 18:04:07.364619+00', NULL, NULL);
INSERT INTO public.field_definition VALUES ('da8dc72c-3b1d-4d90-b0a6-c38f53d87e46', 'country', 'Country', 'Country / region / city tree.', 'tree', '{}', false, true, '{}', NULL, NULL, NULL, 40, 'general', '{"tag": "Country-PrimaryLocationName", "type": "iptc"}', 'active', NULL, NULL, '2026-06-06 18:04:07.364619+00', '2026-06-06 18:04:07.364619+00', NULL, NULL);

INSERT INTO public.roles VALUES ('80ec6003-7fd5-4dac-9415-d26d39169d42', NULL, 'Base', 'Minimal sign-in user; can read public catalogs', NULL, '2026-06-06 18:04:06.825478+00', '2026-06-06 18:04:06.825478+00');
INSERT INTO public.roles VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', '80ec6003-7fd5-4dac-9415-d26d39169d42', 'Admin', 'Full administrative access', NULL, '2026-06-06 18:04:06.825478+00', '2026-06-06 18:04:06.825478+00');
INSERT INTO public.roles VALUES ('a09769d4-968f-4df8-881f-d5b0822fa62d', NULL, 'Anonymous', 'Synthetic role for unauthenticated requests; caps gate which public surfaces anonymous users may read', NULL, '2026-06-06 18:04:07.649058+00', '2026-06-06 18:04:07.649058+00');

INSERT INTO public.role_capabilities VALUES ('80ec6003-7fd5-4dac-9415-d26d39169d42', 'caps.read');
INSERT INTO public.role_capabilities VALUES ('80ec6003-7fd5-4dac-9415-d26d39169d42', 'roles.read');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'users.read');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'users.write');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'system.admin');
INSERT INTO public.role_capabilities VALUES ('80ec6003-7fd5-4dac-9415-d26d39169d42', 'teams.read');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'teams.create');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'teams.admin');
INSERT INTO public.role_capabilities VALUES ('80ec6003-7fd5-4dac-9415-d26d39169d42', 'assets.submit');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'posts.publish');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'assets.review');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'assets.publish');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'assets.archive');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'assets.unarchive');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'workflow.admin');
INSERT INTO public.role_capabilities VALUES ('80ec6003-7fd5-4dac-9415-d26d39169d42', 'posts.comment');
INSERT INTO public.role_capabilities VALUES ('80ec6003-7fd5-4dac-9415-d26d39169d42', 'posts.like');
INSERT INTO public.role_capabilities VALUES ('80ec6003-7fd5-4dac-9415-d26d39169d42', 'comments.delete.own');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'comments.delete.any');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'users.profile.edit.any');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'system.config.read');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'system.config.write');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'system.auth.write');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'system.ai.write');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'system.appearance.write');

INSERT INTO public.workflow_states VALUES ('48a7ec39-9ab8-463e-984a-9f0c3037fee1', 'post', 'published', 'Published', 0, true, false, true, '2026-06-06 18:04:07.602388+00', 'check-circle', '#16a34a', false);
INSERT INTO public.workflow_states VALUES ('a4fb6ed4-16f0-405b-a172-f0049a07feda', 'asset:1', 'draft', 'Draft', 0, true, false, false, '2026-06-06 18:04:07.602388+00', 'file-edit', '#64748b', false);
INSERT INTO public.workflow_states VALUES ('2c68c34f-fdbf-4080-a958-e8418b4e4def', 'asset:1', 'pending_review', 'Pending Review', 1, false, false, false, '2026-06-06 18:04:07.602388+00', 'clock', '#f59e0b', false);
INSERT INTO public.workflow_states VALUES ('daf8045b-0b32-49c9-87eb-e7ff72db206c', 'asset:1', 'published', 'Published', 2, false, false, true, '2026-06-06 18:04:07.602388+00', 'check-circle', '#16a34a', false);
INSERT INTO public.workflow_states VALUES ('def32f01-1912-4d43-8c51-95f527d163dd', 'asset:1', 'archived', 'Archived', 3, false, false, false, '2026-06-06 18:04:07.602388+00', 'archive', '#0ea5e9', false);
INSERT INTO public.workflow_states VALUES ('0489ffd2-9ec4-454f-9604-02f8c4390a7b', 'asset:1', 'deleted', 'Deleted', 4, false, true, false, '2026-06-06 18:04:07.602388+00', 'trash-2', '#ef4444', false);
INSERT INTO public.workflow_states VALUES ('3c318b8b-572c-4ed8-a87f-6f531ce42028', 'post', 'wip', 'WIP', -10, false, false, true, '2026-06-06 18:04:07.691197+00', 'pencil-line', '#f59e0b', false);

INSERT INTO public.workflow_transitions VALUES ('6af6c8d9-8d19-4976-b060-3121153f874c', 'a4fb6ed4-16f0-405b-a172-f0049a07feda', '2c68c34f-fdbf-4080-a958-e8418b4e4def', 'assets.submit', false);
INSERT INTO public.workflow_transitions VALUES ('036187b0-0065-4834-93d1-c5cc6a7b19c1', '2c68c34f-fdbf-4080-a958-e8418b4e4def', 'daf8045b-0b32-49c9-87eb-e7ff72db206c', 'assets.review', true);
INSERT INTO public.workflow_transitions VALUES ('f8c0b52b-ef0c-406d-be49-bcba2935bd58', '2c68c34f-fdbf-4080-a958-e8418b4e4def', 'a4fb6ed4-16f0-405b-a172-f0049a07feda', 'assets.review', true);
INSERT INTO public.workflow_transitions VALUES ('c5411fe5-bf35-487e-9c71-761ebb418e58', 'a4fb6ed4-16f0-405b-a172-f0049a07feda', 'daf8045b-0b32-49c9-87eb-e7ff72db206c', 'assets.publish', true);
INSERT INTO public.workflow_transitions VALUES ('6cd17ebe-c92b-4c1c-be81-d2080944985d', 'daf8045b-0b32-49c9-87eb-e7ff72db206c', 'def32f01-1912-4d43-8c51-95f527d163dd', 'assets.archive', true);
INSERT INTO public.workflow_transitions VALUES ('d6310dbd-8a3e-4289-9726-ea2e477f3fb6', 'def32f01-1912-4d43-8c51-95f527d163dd', 'daf8045b-0b32-49c9-87eb-e7ff72db206c', 'assets.unarchive', true);
INSERT INTO public.workflow_transitions VALUES ('db548e79-b233-4396-82bf-bdc4302f1112', NULL, 'a4fb6ed4-16f0-405b-a172-f0049a07feda', NULL, false);
INSERT INTO public.workflow_transitions VALUES ('13681f73-8ff7-4d69-a805-084365305467', NULL, '48a7ec39-9ab8-463e-984a-9f0c3037fee1', NULL, false);
INSERT INTO public.workflow_transitions VALUES ('a9d5c140-ba58-4bb9-9643-3b5880a93c41', NULL, '3c318b8b-572c-4ed8-a87f-6f531ce42028', NULL, false);
INSERT INTO public.workflow_transitions VALUES ('8b398b4a-4a35-49f8-922a-370910176ef3', '3c318b8b-572c-4ed8-a87f-6f531ce42028', '48a7ec39-9ab8-463e-984a-9f0c3037fee1', 'posts.publish', false);
INSERT INTO public.workflow_transitions VALUES ('23306b7c-c570-4d50-9c22-875f778111b2', '48a7ec39-9ab8-463e-984a-9f0c3037fee1', '3c318b8b-572c-4ed8-a87f-6f531ce42028', 'posts.publish', false);

--
-- Name: resource_type_ref_seq; Type: SEQUENCE SET; Schema: public; Owner: -
--





-- federation_dispatch_state cursor row — required for the dispatcher to start.
-- Originated in 00006_federation_dispatch_triggers.sql per the pre-squash sequence.
INSERT INTO public.federation_dispatch_state (id) VALUES (1);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- (Down is intentionally empty for a baseline migration.
--  Rolling back means restoring from a backup, not running
--  migrations. See ADR 0046 §"When a squash is allowed".)
-- +goose StatementEnd
