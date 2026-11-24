-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00100_federation_metadata_producers.sql
--
-- #1577 (spec v1 §18.3): the producers of durable metadata work
-- (federation_metadata_work, migration 00099). Each trigger adds, inside
-- the transaction of the change, at most two rows naming UUIDs and
-- nothing else, so the obligation commits or rolls back with the change.
-- A failed insert fails the change. Nothing here reads a grant, a peer's
-- profiles or a value to decide whether anyone will receive it: a change
-- that saw no eligible peer could race a grant whose refresh read the old
-- value, so every relevant change queues work and the recorder decides
-- (assetsend.RecordMetadataUpdate). No row is merged into another.
--
--   1. assets UPDATE: title, description, or a column the recorder's
--      eligibility reads (origin_server_id, deleted_at, sensitivity,
--      owner_user_ref, asset_type) actually changed: one 'asset' row.
--      Flags, tags and timestamps alone queue nothing; an INSERT queues
--      nothing (a new asset's first announcement carries its snapshot).
--   2. asset_field_value INSERT / UPDATE / DELETE of the two seeded
--      fields (rights_uri, required_statement, by id): the affected
--      asset(s), at most two, when the asset, the field or value_text
--      changed. A value deleted because its definition was deleted queues
--      nothing: the definition's own trigger queues one 'all' row instead.
--   3. field_definition INSERT / UPDATE / DELETE of a seeded field or of
--      any title / description mirror, old or new: one 'all' row when an
--      attribute the capture reads changed (id, code, status, type,
--      subject_kind, mirrors_column, read_capability, applies_to).
--   4. federation_peers INSERT / UPDATE that makes a peer enabled,
--      connected and holding all three stored profiles: one 'all' row,
--      so edits made while it could not receive them reach it.
--   5. federation_shares INSERT / UPDATE / DELETE of an asset share: the
--      affected asset(s), at most two, when an attribute of the grant
--      changed. An INSERT matters: a regrant of an already held version
--      refreshes metadata the peer holds.
--
-- Up also queues ONE 'all' row, so metadata already held before these
-- producers existed is brought up to date; nothing is scanned here.
-- The functions run as the invoker; no role or privilege changes.

-- +goose Up

-- +goose StatementBegin
CREATE FUNCTION public.federation_metadata_asset_changed() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO public.federation_metadata_work (kind, asset_id) VALUES ('asset', NEW.id);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER federation_metadata_asset_changed
    AFTER UPDATE ON public.assets
    FOR EACH ROW
    WHEN ((OLD.origin_server_id IS NULL OR NEW.origin_server_id IS NULL)
          AND (OLD.title IS DISTINCT FROM NEW.title
               OR OLD.description IS DISTINCT FROM NEW.description
               OR OLD.origin_server_id IS DISTINCT FROM NEW.origin_server_id
               OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
               OR OLD.sensitivity IS DISTINCT FROM NEW.sensitivity
               OR OLD.owner_user_ref IS DISTINCT FROM NEW.owner_user_ref
               OR OLD.asset_type IS DISTINCT FROM NEW.asset_type))
    EXECUTE FUNCTION public.federation_metadata_asset_changed();

-- +goose StatementBegin
CREATE FUNCTION public.federation_metadata_value_changed() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    -- The seeded rights_uri and required_statement fields (migration 00097).
    seeded CONSTANT uuid[] := ARRAY['56d71791-189d-429c-b7e4-e837baf91ddb', '6c3e25be-83ed-4762-b636-a1e37cdbb74c']::uuid[];
    old_seeded boolean := FALSE;
    new_seeded boolean := FALSE;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        old_seeded := OLD.field_id = ANY (seeded);
    END IF;
    IF TG_OP <> 'DELETE' THEN
        new_seeded := NEW.field_id = ANY (seeded);
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.asset_id = NEW.asset_id AND OLD.field_id = NEW.field_id
       AND OLD.value_text IS NOT DISTINCT FROM NEW.value_text THEN
        RETURN NULL;
    END IF;
    IF old_seeded THEN
        -- A value deleted with its definition: the definition queues one
        -- 'all' row in this transaction, so the cascade adds none per value.
        IF TG_OP = 'DELETE' AND NOT EXISTS (SELECT 1 FROM public.field_definition WHERE id = OLD.field_id) THEN
            RETURN NULL;
        END IF;
        INSERT INTO public.federation_metadata_work (kind, asset_id) VALUES ('asset', OLD.asset_id);
        IF TG_OP = 'UPDATE' AND OLD.asset_id = NEW.asset_id THEN
            RETURN NULL;
        END IF;
    END IF;
    IF new_seeded THEN
        INSERT INTO public.federation_metadata_work (kind, asset_id) VALUES ('asset', NEW.asset_id);
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER federation_metadata_value_changed
    AFTER INSERT OR UPDATE OR DELETE ON public.asset_field_value
    FOR EACH ROW
    EXECUTE FUNCTION public.federation_metadata_value_changed();

-- +goose StatementBegin
CREATE FUNCTION public.federation_metadata_definition_changed() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    seeded CONSTANT uuid[] := ARRAY['56d71791-189d-429c-b7e4-e837baf91ddb', '6c3e25be-83ed-4762-b636-a1e37cdbb74c']::uuid[];
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NOT (OLD.id = ANY (seeded) OR OLD.mirrors_column IS NOT NULL
                OR NEW.id = ANY (seeded) OR NEW.mirrors_column IS NOT NULL) THEN
            RETURN NULL;
        END IF;
        IF OLD.id IS NOT DISTINCT FROM NEW.id AND OLD.code IS NOT DISTINCT FROM NEW.code
           AND OLD.status IS NOT DISTINCT FROM NEW.status AND OLD.type IS NOT DISTINCT FROM NEW.type
           AND OLD.subject_kind IS NOT DISTINCT FROM NEW.subject_kind
           AND OLD.mirrors_column IS NOT DISTINCT FROM NEW.mirrors_column
           AND OLD.read_capability IS NOT DISTINCT FROM NEW.read_capability
           AND OLD.applies_to IS NOT DISTINCT FROM NEW.applies_to THEN
            RETURN NULL;
        END IF;
    ELSIF TG_OP = 'INSERT' THEN
        IF NOT (NEW.id = ANY (seeded) OR NEW.mirrors_column IS NOT NULL) THEN
            RETURN NULL;
        END IF;
    ELSIF NOT (OLD.id = ANY (seeded) OR OLD.mirrors_column IS NOT NULL) THEN
        RETURN NULL;
    END IF;
    INSERT INTO public.federation_metadata_work (kind) VALUES ('all');
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER federation_metadata_definition_changed
    AFTER INSERT OR UPDATE OR DELETE ON public.field_definition
    FOR EACH ROW
    EXECUTE FUNCTION public.federation_metadata_definition_changed();

-- +goose StatementBegin
CREATE FUNCTION public.federation_metadata_peer_eligible() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    -- asset-metadata-1, archival-1 and original-transfer-1, as stored.
    now_eligible boolean := NEW.enabled AND NEW.status = 'connected'
        AND NEW.capabilities ? 'asset-metadata-1' AND NEW.capabilities ? 'archival-1'
        AND NEW.capabilities ? 'original-transfer-1';
BEGIN
    IF NOT now_eligible THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        IF OLD.enabled AND OLD.status = 'connected'
           AND OLD.capabilities ? 'asset-metadata-1' AND OLD.capabilities ? 'archival-1'
           AND OLD.capabilities ? 'original-transfer-1' THEN
            RETURN NULL;
        END IF;
    END IF;
    INSERT INTO public.federation_metadata_work (kind) VALUES ('all');
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER federation_metadata_peer_eligible
    AFTER INSERT OR UPDATE ON public.federation_peers
    FOR EACH ROW
    EXECUTE FUNCTION public.federation_metadata_peer_eligible();

-- +goose StatementBegin
CREATE FUNCTION public.federation_metadata_share_changed() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.object_kind = 'asset' THEN
            INSERT INTO public.federation_metadata_work (kind, asset_id) VALUES ('asset', NEW.object_id);
        END IF;
        RETURN NULL;
    END IF;
    IF TG_OP = 'DELETE' THEN
        IF OLD.object_kind = 'asset' THEN
            INSERT INTO public.federation_metadata_work (kind, asset_id) VALUES ('asset', OLD.object_id);
        END IF;
        RETURN NULL;
    END IF;
    IF OLD.object_kind IS NOT DISTINCT FROM NEW.object_kind
       AND OLD.object_id IS NOT DISTINCT FROM NEW.object_id
       AND OLD.peer_id IS NOT DISTINCT FROM NEW.peer_id AND OLD.target_user_url IS NOT DISTINCT FROM NEW.target_user_url
       AND OLD.scope IS NOT DISTINCT FROM NEW.scope AND OLD.allow_original IS NOT DISTINCT FROM NEW.allow_original
       AND OLD.revoked_at IS NOT DISTINCT FROM NEW.revoked_at AND OLD.expires_at IS NOT DISTINCT FROM NEW.expires_at
       AND OLD.granted_activity_id IS NOT DISTINCT FROM NEW.granted_activity_id THEN
        RETURN NULL;
    END IF;
    IF OLD.object_kind = 'asset' THEN
        INSERT INTO public.federation_metadata_work (kind, asset_id) VALUES ('asset', OLD.object_id);
    END IF;
    IF NEW.object_kind = 'asset' AND NOT (OLD.object_kind = 'asset' AND OLD.object_id = NEW.object_id) THEN
        INSERT INTO public.federation_metadata_work (kind, asset_id) VALUES ('asset', NEW.object_id);
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER federation_metadata_share_changed
    AFTER INSERT OR UPDATE OR DELETE ON public.federation_shares
    FOR EACH ROW
    EXECUTE FUNCTION public.federation_metadata_share_changed();

-- The one bootstrap obligation (file header).
INSERT INTO public.federation_metadata_work (kind) VALUES ('all');

-- +goose Down

-- Down removes only these producers. The ledger and any pending work,
-- the bootstrap row included, stay: 00099's Down refuses to discard them.
DROP TRIGGER IF EXISTS federation_metadata_share_changed ON public.federation_shares;
DROP TRIGGER IF EXISTS federation_metadata_peer_eligible ON public.federation_peers;
DROP TRIGGER IF EXISTS federation_metadata_definition_changed ON public.field_definition;
DROP TRIGGER IF EXISTS federation_metadata_value_changed ON public.asset_field_value;
DROP TRIGGER IF EXISTS federation_metadata_asset_changed ON public.assets;
DROP FUNCTION IF EXISTS public.federation_metadata_share_changed();
DROP FUNCTION IF EXISTS public.federation_metadata_peer_eligible();
DROP FUNCTION IF EXISTS public.federation_metadata_definition_changed();
DROP FUNCTION IF EXISTS public.federation_metadata_value_changed();
DROP FUNCTION IF EXISTS public.federation_metadata_asset_changed();
