-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00092_federation_received_asset_versions.sql
--
-- #1575 (spec v1 §17.2.1): a receiver keeps every version of an asset it
-- accepted, and accepts a later version only as the exact successor of
-- the one it holds. Until now one live record existed per (peer, asset)
-- and any other version id was refused (version_unsupported).
--
-- Each received version stays its own immutable record. What changes:
--   - A record names the version it must follow: predecessor_version_uri
--     is set when the record was bound as the successor of a version this
--     instance had accepted (NULL for the first version received of the
--     asset, which may have any number: a share can start at version 3).
--     predecessor_id links that predecessor's record, once, as the
--     successor completes; a trigger checks it is the same peer's record
--     of the same asset, one number lower, with that version id.
--   - The new state superseded: a complete version that a verified
--     successor replaced. It keeps its originals verified and pinned
--     (§17.2.1: "keeps every version it accepted and never replaces one in
--     place") and is final except for a withdrawal. Activation is one
--     transaction: the predecessor becomes superseded and the successor
--     complete, and the commit check requires every superseded record to
--     have its successor.
--   - Uniqueness moves from one live record per (peer, asset) to: one
--     record per version number and per version id (rejected ones
--     aside), one complete record (the current version), one first
--     version, and one successor per version.
--   - Three reasons: awaiting_predecessor (a successor that arrived
--     before its predecessor was complete waits, pending), predecessor_missing
--     (the wait ran out) and version_stale (older than the first version
--     this instance holds of the asset; never received, so no record ever
--     goes back to an older version).
--   - A withdrawal (00091) also withdraws superseded records.

-- +goose Up

ALTER TABLE public.federation_received_assets ADD COLUMN IF NOT EXISTS predecessor_version_uri text;
ALTER TABLE public.federation_received_assets ADD COLUMN IF NOT EXISTS predecessor_id uuid;
ALTER TABLE public.federation_received_assets ADD COLUMN IF NOT EXISTS superseded_at timestamp with time zone;

ALTER TABLE public.federation_received_assets DROP CONSTRAINT IF EXISTS federation_received_assets_predecessor_fkey;
ALTER TABLE public.federation_received_assets ADD CONSTRAINT federation_received_assets_predecessor_fkey
    FOREIGN KEY (predecessor_id) REFERENCES public.federation_received_assets(id);

ALTER TABLE public.federation_received_assets DROP CONSTRAINT federation_received_assets_state_check;
ALTER TABLE public.federation_received_assets ADD CONSTRAINT federation_received_assets_state_check CHECK (state = ANY (ARRAY['pending'::text, 'complete'::text, 'superseded'::text, 'rejected'::text, 'withdrawn'::text]));

-- complete and superseded: no reason, a commit time and the grant;
-- superseded also when it was replaced. withdrawn keeps whatever it had.
ALTER TABLE public.federation_received_assets DROP CONSTRAINT federation_received_assets_state_shape_check;
ALTER TABLE public.federation_received_assets ADD CONSTRAINT federation_received_assets_state_shape_check CHECK (CASE state
        WHEN 'complete' THEN reason IS NULL AND completed_at IS NOT NULL AND authorizing_grant_id IS NOT NULL AND withdrawn_at IS NULL AND superseded_at IS NULL
        WHEN 'superseded' THEN reason IS NULL AND completed_at IS NOT NULL AND authorizing_grant_id IS NOT NULL AND withdrawn_at IS NULL AND superseded_at IS NOT NULL
        WHEN 'rejected' THEN reason IS NOT NULL AND completed_at IS NULL AND authorizing_grant_id IS NULL AND withdrawn_at IS NULL AND superseded_at IS NULL
        WHEN 'withdrawn' THEN reason = 'withdrawn'::text AND withdrawn_at IS NOT NULL
        ELSE completed_at IS NULL AND authorizing_grant_id IS NULL AND withdrawn_at IS NULL AND superseded_at IS NULL
    END);

-- A record links a predecessor only if it was bound as a successor, and a
-- successor that holds its bytes (complete or superseded) is linked.
ALTER TABLE public.federation_received_assets DROP CONSTRAINT IF EXISTS federation_received_assets_chain_check;
ALTER TABLE public.federation_received_assets ADD CONSTRAINT federation_received_assets_chain_check CHECK (
        (predecessor_id IS NULL OR predecessor_version_uri IS NOT NULL)
        AND (predecessor_version_uri IS NULL OR version_number > 1)
        AND (state <> ALL (ARRAY['complete'::text, 'superseded'::text]) OR predecessor_version_uri IS NULL OR predecessor_id IS NOT NULL));

ALTER TABLE public.federation_received_assets DROP CONSTRAINT federation_received_assets_reason_check;
ALTER TABLE public.federation_received_assets ADD CONSTRAINT federation_received_assets_reason_check CHECK (reason IS NULL OR reason = ANY (ARRAY[
        'no_inbound_grant'::text, 'fixity_mismatch'::text, 'length_mismatch'::text, 'origin_refused'::text,
        'revoked_during_fetch'::text, 'too_large'::text, 'sensitivity_refused'::text, 'version_conflict'::text,
        'version_unsupported'::text, 'redirect_refused'::text, 'class_not_available'::text, 'foreign_origin'::text,
        'foreign_signer'::text, 'invalid_request'::text, 'http_status'::text, 'timeout'::text,
        'transport_error'::text, 'lease_lost'::text, 'storage_error'::text, 'peer_unavailable'::text,
        'profile_not_negotiated'::text, 'archival_invalid'::text, 'awaiting_grant'::text, 'withdrawn'::text,
        'awaiting_predecessor'::text, 'predecessor_missing'::text, 'version_stale'::text]));

ALTER TABLE public.federation_transfer_events DROP CONSTRAINT federation_transfer_events_reason_check;
ALTER TABLE public.federation_transfer_events ADD CONSTRAINT federation_transfer_events_reason_check CHECK ((outcome = 'success'::text) = (reason IS NULL) AND (reason IS NULL OR reason = ANY (ARRAY[
        'no_inbound_grant'::text, 'fixity_mismatch'::text, 'length_mismatch'::text, 'origin_refused'::text,
        'revoked_during_fetch'::text, 'too_large'::text, 'sensitivity_refused'::text, 'version_conflict'::text,
        'version_unsupported'::text, 'redirect_refused'::text, 'class_not_available'::text, 'foreign_origin'::text,
        'foreign_signer'::text, 'invalid_request'::text, 'http_status'::text, 'timeout'::text,
        'transport_error'::text, 'lease_lost'::text, 'storage_error'::text, 'peer_unavailable'::text,
        'profile_not_negotiated'::text, 'archival_invalid'::text, 'awaiting_grant'::text, 'withdrawn'::text,
        'awaiting_predecessor'::text, 'predecessor_missing'::text, 'version_stale'::text])));

DROP INDEX IF EXISTS public.federation_received_assets_live_key;
-- One record per version number and per version id of an asset (a
-- rejected one never blocks a later announcement).
CREATE UNIQUE INDEX IF NOT EXISTS federation_received_assets_version_key
    ON public.federation_received_assets USING btree (peer_id, origin_asset_uri, version_number)
    WHERE (state <> 'rejected'::text);
CREATE UNIQUE INDEX IF NOT EXISTS federation_received_assets_version_uri_key
    ON public.federation_received_assets USING btree (peer_id, origin_asset_uri, version_uri)
    WHERE (state <> 'rejected'::text);
-- One current version, one first version, one successor per version.
CREATE UNIQUE INDEX IF NOT EXISTS federation_received_assets_current_key
    ON public.federation_received_assets USING btree (peer_id, origin_asset_uri)
    WHERE (state = 'complete'::text);
CREATE UNIQUE INDEX IF NOT EXISTS federation_received_assets_first_key
    ON public.federation_received_assets USING btree (peer_id, origin_asset_uri)
    WHERE (state <> 'rejected'::text AND predecessor_version_uri IS NULL);
CREATE UNIQUE INDEX IF NOT EXISTS federation_received_assets_successor_key
    ON public.federation_received_assets USING btree (predecessor_id)
    WHERE (predecessor_id IS NOT NULL);

COMMENT ON COLUMN public.federation_received_assets.predecessor_version_uri IS 'The version id this record must follow (#1575, spec v1 §17.2.1): set when it was bound as the successor of a version this instance had accepted of the asset; NULL for the first version received of it.';
COMMENT ON COLUMN public.federation_received_assets.predecessor_id IS 'The record of that predecessor, linked once, as this record completes and the predecessor becomes superseded (#1575).';
COMMENT ON COLUMN public.federation_received_assets.superseded_at IS 'When a verified successor replaced this complete version (#1575). A superseded record keeps its originals verified and pinned.';

-- +goose StatementBegin
-- The predecessor link names the same peer's record of the same asset,
-- one version number lower, with the version id the record must follow.
CREATE OR REPLACE FUNCTION public.federation_received_assets_chain_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    p record;
BEGIN
    IF NEW.predecessor_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT peer_id, origin_asset_uri, version_number, version_uri INTO p
      FROM public.federation_received_assets WHERE id = NEW.predecessor_id;
    IF NOT FOUND OR p.peer_id <> NEW.peer_id OR p.origin_asset_uri <> NEW.origin_asset_uri
       OR p.version_number <> NEW.version_number - 1 OR p.version_uri IS DISTINCT FROM NEW.predecessor_version_uri THEN
        RAISE EXCEPTION 'received asset %: % is not the previous version of the same asset from the same peer (#1575)', NEW.id, NEW.predecessor_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS federation_received_assets_chain_check_trg ON public.federation_received_assets;
CREATE TRIGGER federation_received_assets_chain_check_trg BEFORE INSERT OR UPDATE OF predecessor_id ON public.federation_received_assets
    FOR EACH ROW EXECUTE FUNCTION public.federation_received_assets_chain_check();

-- +goose StatementBegin
-- A complete record is final: its claims, file set and grant are never
-- rewritten, and it never goes back to pending or rejected. It is
-- withdrawn (00091) or superseded by its verified successor (00092);
-- superseded is final except for a withdrawal. A record is linked to its
-- predecessor once, as it completes.
CREATE OR REPLACE FUNCTION public.federation_received_assets_update_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.state = 'withdrawn' THEN
        RAISE EXCEPTION 'received asset %: a withdrawn record is not changed (#1575)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state = 'rejected' THEN
        RAISE EXCEPTION 'received asset %: a rejected record is not changed (#1574)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state = 'superseded' AND (NEW.state <> 'withdrawn'
       OR NEW.completed_at IS DISTINCT FROM OLD.completed_at
       OR NEW.authorizing_grant_id IS DISTINCT FROM OLD.authorizing_grant_id
       OR NEW.attempts <> OLD.attempts
       OR NEW.superseded_at IS DISTINCT FROM OLD.superseded_at) THEN
        RAISE EXCEPTION 'received asset %: a superseded version is kept as it is, only withdrawn (#1575)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state = 'complete' AND (NEW.state <> ALL (ARRAY['withdrawn'::text, 'superseded'::text])
       OR NEW.completed_at IS DISTINCT FROM OLD.completed_at
       OR NEW.authorizing_grant_id IS DISTINCT FROM OLD.authorizing_grant_id
       OR NEW.attempts <> OLD.attempts) THEN
        RAISE EXCEPTION 'received asset %: a complete record is not changed, only withdrawn or superseded (#1574, #1575)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.predecessor_id IS DISTINCT FROM OLD.predecessor_id
       AND NOT (OLD.state = 'pending' AND NEW.state = 'complete' AND OLD.predecessor_id IS NULL) THEN
        RAISE EXCEPTION 'received asset %: a record is linked to its predecessor once, as it completes (#1575)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.superseded_at IS DISTINCT FROM OLD.superseded_at
       AND NOT (OLD.state = 'complete' AND NEW.state = 'superseded') THEN
        RAISE EXCEPTION 'received asset %: superseded_at is set only as a complete version is superseded (#1575)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.peer_id <> OLD.peer_id OR NEW.activity_uri <> OLD.activity_uri OR NEW.actor_uri <> OLD.actor_uri
       OR NEW.origin_asset_uri <> OLD.origin_asset_uri OR NEW.origin_asset_id <> OLD.origin_asset_id
       OR NEW.version_uri <> OLD.version_uri OR NEW.version_number <> OLD.version_number
       OR NEW.file_set_sha256 <> OLD.file_set_sha256 OR NEW.record <> OLD.record
       OR NEW.sensitivity <> OLD.sensitivity OR NEW.signer_key_id <> OLD.signer_key_id
       OR NEW.signer_actor_uri IS DISTINCT FROM OLD.signer_actor_uri OR NEW.received_at <> OLD.received_at
       OR NEW.created_xid <> OLD.created_xid
       OR NEW.predecessor_version_uri IS DISTINCT FROM OLD.predecessor_version_uri THEN
        RAISE EXCEPTION 'received asset %: only its state, reason, attempts and commit are updated (#1574)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The commit-time invariant of one record: a complete or superseded
-- record has at least one original and every original verified, pinned
-- under it at its size and identified, and a superseded one has its
-- successor; a pending, rejected or withdrawn record holds no pin and no
-- verified file. A record that no longer exists is not checked.
CREATE OR REPLACE FUNCTION public.federation_received_asset_require_consistent(p_record uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    r record;
BEGIN
    SELECT id, state INTO r FROM public.federation_received_assets WHERE id = p_record;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    IF r.state = ANY (ARRAY['complete'::text, 'superseded'::text]) THEN
        IF NOT EXISTS (SELECT 1 FROM public.federation_received_asset_files WHERE received_asset_id = r.id AND role = 'original') THEN
            RAISE EXCEPTION 'received asset %: % without an original file (#1574)', r.id, r.state
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF EXISTS (
            SELECT 1 FROM public.federation_received_asset_files f
             WHERE f.received_asset_id = r.id AND f.role = 'original'
               AND (f.state <> 'verified' OR NOT EXISTS (
                    SELECT 1 FROM public.storage_pins p
                      JOIN public.storage_objects o ON o.hash = p.object_hash
                     WHERE p.pin_subject_type = 'federation_received_asset' AND p.pin_subject_id = r.id::text
                       AND p.object_hash = f.object_hash AND o.size_bytes = f.size_bytes))) THEN
            RAISE EXCEPTION 'received asset %: % with an original that is not verified and pinned (#1574)', r.id, r.state
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF EXISTS (SELECT 1 FROM public.federation_received_asset_files f
                    WHERE f.received_asset_id = r.id AND f.role = 'original' AND f.detected_media_type IS NULL) THEN
            RAISE EXCEPTION 'received asset %: % with an original that was not identified (#1574)', r.id, r.state
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF r.state = 'superseded' AND NOT EXISTS (
            SELECT 1 FROM public.federation_received_assets s
             WHERE s.predecessor_id = r.id AND s.state = ANY (ARRAY['complete'::text, 'superseded'::text, 'withdrawn'::text])) THEN
            RAISE EXCEPTION 'received asset %: superseded without a successor (#1575)', r.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN;
    END IF;
    IF EXISTS (SELECT 1 FROM public.storage_pins WHERE pin_subject_type = 'federation_received_asset' AND pin_subject_id = r.id::text)
       OR EXISTS (SELECT 1 FROM public.federation_received_asset_files WHERE received_asset_id = r.id AND state = 'verified') THEN
        RAISE EXCEPTION 'received asset %: % but holding a pin or a verified file (#1574)', r.id, r.state
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down

-- The 00091 schema has one live record per (peer, asset). It cannot hold
-- a version history (a superseded version and its successor, or a
-- successor waiting for its predecessor), so Down refuses while one
-- exists rather than discard what this instance accepted.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.federation_received_assets WHERE state <> 'rejected'
                GROUP BY peer_id, origin_asset_uri HAVING count(*) > 1) THEN
        RAISE EXCEPTION '00092 down: a received asset holds more than one version; the 00091 schema keeps one live record per asset (#1575)'
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS federation_received_assets_chain_check_trg ON public.federation_received_assets;
DROP FUNCTION IF EXISTS public.federation_received_assets_chain_check();

DROP INDEX IF EXISTS public.federation_received_assets_successor_key;
DROP INDEX IF EXISTS public.federation_received_assets_first_key;
DROP INDEX IF EXISTS public.federation_received_assets_current_key;
DROP INDEX IF EXISTS public.federation_received_assets_version_uri_key;
DROP INDEX IF EXISTS public.federation_received_assets_version_key;
CREATE UNIQUE INDEX IF NOT EXISTS federation_received_assets_live_key
    ON public.federation_received_assets USING btree (peer_id, origin_asset_uri)
    WHERE (state <> 'rejected'::text);

-- The 00091 sets and shapes again, for new writes. NOT VALID: a rejected
-- record or ledger row that already names a 00092 reason keeps it.
ALTER TABLE public.federation_transfer_events DROP CONSTRAINT federation_transfer_events_reason_check;
ALTER TABLE public.federation_transfer_events ADD CONSTRAINT federation_transfer_events_reason_check CHECK ((outcome = 'success'::text) = (reason IS NULL) AND (reason IS NULL OR reason = ANY (ARRAY[
        'no_inbound_grant'::text, 'fixity_mismatch'::text, 'length_mismatch'::text, 'origin_refused'::text,
        'revoked_during_fetch'::text, 'too_large'::text, 'sensitivity_refused'::text, 'version_conflict'::text,
        'version_unsupported'::text, 'redirect_refused'::text, 'class_not_available'::text, 'foreign_origin'::text,
        'foreign_signer'::text, 'invalid_request'::text, 'http_status'::text, 'timeout'::text,
        'transport_error'::text, 'lease_lost'::text, 'storage_error'::text, 'peer_unavailable'::text,
        'profile_not_negotiated'::text, 'archival_invalid'::text, 'awaiting_grant'::text, 'withdrawn'::text]))) NOT VALID;

ALTER TABLE public.federation_received_assets DROP CONSTRAINT federation_received_assets_reason_check;
ALTER TABLE public.federation_received_assets ADD CONSTRAINT federation_received_assets_reason_check CHECK (reason IS NULL OR reason = ANY (ARRAY[
        'no_inbound_grant'::text, 'fixity_mismatch'::text, 'length_mismatch'::text, 'origin_refused'::text,
        'revoked_during_fetch'::text, 'too_large'::text, 'sensitivity_refused'::text, 'version_conflict'::text,
        'version_unsupported'::text, 'redirect_refused'::text, 'class_not_available'::text, 'foreign_origin'::text,
        'foreign_signer'::text, 'invalid_request'::text, 'http_status'::text, 'timeout'::text,
        'transport_error'::text, 'lease_lost'::text, 'storage_error'::text, 'peer_unavailable'::text,
        'profile_not_negotiated'::text, 'archival_invalid'::text, 'awaiting_grant'::text, 'withdrawn'::text])) NOT VALID;

ALTER TABLE public.federation_received_assets DROP CONSTRAINT IF EXISTS federation_received_assets_chain_check;

ALTER TABLE public.federation_received_assets DROP CONSTRAINT federation_received_assets_state_shape_check;
ALTER TABLE public.federation_received_assets ADD CONSTRAINT federation_received_assets_state_shape_check CHECK (CASE state
        WHEN 'complete' THEN reason IS NULL AND completed_at IS NOT NULL AND authorizing_grant_id IS NOT NULL AND withdrawn_at IS NULL
        WHEN 'rejected' THEN reason IS NOT NULL AND completed_at IS NULL AND authorizing_grant_id IS NULL AND withdrawn_at IS NULL
        WHEN 'withdrawn' THEN reason = 'withdrawn'::text AND withdrawn_at IS NOT NULL
        ELSE completed_at IS NULL AND authorizing_grant_id IS NULL AND withdrawn_at IS NULL
    END) NOT VALID;

ALTER TABLE public.federation_received_assets DROP CONSTRAINT federation_received_assets_state_check;
ALTER TABLE public.federation_received_assets ADD CONSTRAINT federation_received_assets_state_check CHECK (state = ANY (ARRAY['pending'::text, 'complete'::text, 'rejected'::text, 'withdrawn'::text])) NOT VALID;

ALTER TABLE public.federation_received_assets DROP CONSTRAINT IF EXISTS federation_received_assets_predecessor_fkey;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.federation_received_assets_update_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.state = 'withdrawn' THEN
        RAISE EXCEPTION 'received asset %: a withdrawn record is not changed (#1575)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state = 'rejected' THEN
        RAISE EXCEPTION 'received asset %: a rejected record is not changed (#1574)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state = 'complete' AND (NEW.state <> 'withdrawn'
       OR NEW.completed_at IS DISTINCT FROM OLD.completed_at
       OR NEW.authorizing_grant_id IS DISTINCT FROM OLD.authorizing_grant_id
       OR NEW.attempts <> OLD.attempts) THEN
        RAISE EXCEPTION 'received asset %: a complete record is not changed, only withdrawn (#1574, #1575)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.peer_id <> OLD.peer_id OR NEW.activity_uri <> OLD.activity_uri OR NEW.actor_uri <> OLD.actor_uri
       OR NEW.origin_asset_uri <> OLD.origin_asset_uri OR NEW.origin_asset_id <> OLD.origin_asset_id
       OR NEW.version_uri <> OLD.version_uri OR NEW.version_number <> OLD.version_number
       OR NEW.file_set_sha256 <> OLD.file_set_sha256 OR NEW.record <> OLD.record
       OR NEW.sensitivity <> OLD.sensitivity OR NEW.signer_key_id <> OLD.signer_key_id
       OR NEW.signer_actor_uri IS DISTINCT FROM OLD.signer_actor_uri OR NEW.received_at <> OLD.received_at
       OR NEW.created_xid <> OLD.created_xid THEN
        RAISE EXCEPTION 'received asset %: only its state, reason, attempts and commit are updated (#1574)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.federation_received_asset_require_consistent(p_record uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    r record;
BEGIN
    SELECT id, state INTO r FROM public.federation_received_assets WHERE id = p_record;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    IF r.state = 'complete' THEN
        IF NOT EXISTS (SELECT 1 FROM public.federation_received_asset_files WHERE received_asset_id = r.id AND role = 'original') THEN
            RAISE EXCEPTION 'received asset %: complete without an original file (#1574)', r.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF EXISTS (
            SELECT 1 FROM public.federation_received_asset_files f
             WHERE f.received_asset_id = r.id AND f.role = 'original'
               AND (f.state <> 'verified' OR NOT EXISTS (
                    SELECT 1 FROM public.storage_pins p
                      JOIN public.storage_objects o ON o.hash = p.object_hash
                     WHERE p.pin_subject_type = 'federation_received_asset' AND p.pin_subject_id = r.id::text
                       AND p.object_hash = f.object_hash AND o.size_bytes = f.size_bytes))) THEN
            RAISE EXCEPTION 'received asset %: complete with an original that is not verified and pinned (#1574)', r.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF EXISTS (SELECT 1 FROM public.federation_received_asset_files f
                    WHERE f.received_asset_id = r.id AND f.role = 'original' AND f.detected_media_type IS NULL) THEN
            RAISE EXCEPTION 'received asset %: complete with an original that was not identified (#1574)', r.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN;
    END IF;
    IF EXISTS (SELECT 1 FROM public.storage_pins WHERE pin_subject_type = 'federation_received_asset' AND pin_subject_id = r.id::text)
       OR EXISTS (SELECT 1 FROM public.federation_received_asset_files WHERE received_asset_id = r.id AND state = 'verified') THEN
        RAISE EXCEPTION 'received asset %: % but holding a pin or a verified file (#1574)', r.id, r.state
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
END;
$$;
-- +goose StatementEnd

-- predecessor_version_uri, predecessor_id and superseded_at are kept, as
-- data: the old code ignores them, and a later Up reuses them (IF NOT
-- EXISTS).
