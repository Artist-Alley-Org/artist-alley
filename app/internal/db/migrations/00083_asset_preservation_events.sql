-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00083_asset_preservation_events.sql
--
-- #1598 (core): an asset's preservation history, as PREMIS-style events
-- (ADR 0101, spec v1 §17.4), starting with the fixity checks the
-- checksum sweep already performs.
--
-- asset_preservation_events: append-only, one row per event per asset.
-- A checksum sweep reads each stored (object, variant) once; every asset
-- that references the object (its primary file, an alternate or a
-- companion) gets its own event, so deduplicated bytes shared by two
-- assets appear in both histories.
--
-- This core records ONE kind of event: this instance's own fixity check
-- (source 'local', event_type 'fixity-check'). The columns and the
-- outcome vocabulary below describe a fixity check and nothing else, so
-- the table refuses every other source or type until a later migration
-- gives them honest columns (other event types #1594, peer claims #1577).
--
-- Columns:
--   occurred_at      when the observation was made.
--   outcome          success | failure | warning, and outcome_reason, a
--                    closed vocabulary (never free text, never a path),
--                    each reason bound to what it claims:
--                      digest_match        success: the variant has a
--                                          recorded digest, the bytes read
--                                          hash to it, and the row is
--                                          verified
--                      no_recorded_digest  warning: the bytes were read and
--                                          hashed, but the row has no
--                                          recorded digest to prove them
--                                          against (legacy rows, original
--                                          or derived); it stays
--                                          unverifiable, never success
--                      checksum_mismatch   failure: the bytes hash to
--                                          something other than the
--                                          recorded digest, or, for an
--                                          original, other than its
--                                          content address (whatever its
--                                          recorded digest says: a
--                                          digest rewritten to match
--                                          corrupted bytes does not make
--                                          them the object)
--                      size_mismatch       failure: the byte count is wrong
--                      missing_object      failure: no bytes on the backend
--                      read_error          failure: the backend could not
--                                          deliver the bytes; the error
--                                          itself is operator-only (the
--                                          sweep run's error) and never
--                                          stored here
--   object_hash / variant_key   the safe reference to the file and
--                    version: a content address and a variant name,
--                    never a storage path.
--   observed_generation / observed_sha256 / observed_size_bytes /
--   recorded_sha256 / fixity_status
--                    what was seen: the variant row's generation (#1597),
--                    the digest and size of the bytes read (none when
--                    missing or unreadable), the digest the row recorded,
--                    and the row's fixity status after the check.
--   sweep_run_id     the sweep run that made the observation: an
--                    immutable snapshot, kept when the run row is later
--                    deleted, because preservation history outlives sweep
--                    bookkeeping.
--   sweep_run_ref    the same run as a foreign key while the run row
--                    exists (ON DELETE SET NULL). An event is written with
--                    sweep_run_ref = sweep_run_id, so it names a run that
--                    existed when it was recorded.
--   agent_name / agent_version  the software agent (§17.4). The version
--                    may be absent; an operator may omit it (ADR 0101).
--
-- Idempotency: one event per (run, asset, object, variant, generation,
-- reason), keyed on the run SNAPSHOT so it holds after the run is gone.
-- A sweep batch retried after an interruption writes nothing new; a new
-- run, a new generation of the variant, or a different result (a read
-- error, then a clean read) is a new event.
--
-- Append-only: no UPDATE except the run's own ON DELETE SET NULL of
-- sweep_run_ref, and no DELETE while the event's asset exists. The rows
-- go only with their asset (ON DELETE CASCADE: erasure of the asset).
--
-- storage_sweep_findings gains the same retry safety: one finding per
-- (run, object, variant, finding). Exact duplicates already present (a
-- batch retried before this migration) are collapsed to the earliest.

-- +goose Up

CREATE TABLE public.asset_preservation_events (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    asset_id uuid NOT NULL,
    source text DEFAULT 'local'::text NOT NULL,
    event_type text DEFAULT 'fixity-check'::text NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    outcome text NOT NULL,
    outcome_reason text NOT NULL,
    algorithm text,
    object_hash text NOT NULL,
    variant_key text NOT NULL,
    observed_generation bigint NOT NULL,
    observed_sha256 text,
    observed_size_bytes bigint,
    recorded_sha256 text,
    fixity_status text NOT NULL,
    sweep_run_id uuid,
    sweep_run_ref uuid,
    agent_name text NOT NULL,
    agent_version text,
    CONSTRAINT asset_preservation_events_pkey PRIMARY KEY (id),
    CONSTRAINT asset_preservation_events_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id) ON DELETE CASCADE,
    CONSTRAINT asset_preservation_events_sweep_run_ref_fkey FOREIGN KEY (sweep_run_ref) REFERENCES public.storage_sweep_runs(id) ON DELETE SET NULL,
    -- Only this instance's own fixity checks, for now (see the header).
    CONSTRAINT asset_preservation_events_source_check CHECK (source = 'local'::text),
    CONSTRAINT asset_preservation_events_event_type_check CHECK (event_type = 'fixity-check'::text),
    -- Each reason is bound to its outcome and to the evidence it claims.
    CONSTRAINT asset_preservation_events_outcome_check
        CHECK (CASE outcome_reason
                   WHEN 'digest_match' THEN outcome = 'success'::text AND recorded_sha256 IS NOT NULL
                        AND observed_sha256 IS NOT NULL AND observed_sha256 = recorded_sha256
                        AND fixity_status = 'verified'::text
                   WHEN 'no_recorded_digest' THEN outcome = 'warning'::text AND recorded_sha256 IS NULL
                        AND observed_sha256 IS NOT NULL AND fixity_status = 'unverifiable'::text
                   WHEN 'checksum_mismatch' THEN outcome = 'failure'::text AND observed_sha256 IS NOT NULL
                        AND ((variant_key = 'original'::text AND observed_sha256 <> object_hash)
                             OR (recorded_sha256 IS NOT NULL AND observed_sha256 <> recorded_sha256))
                   WHEN 'size_mismatch' THEN outcome = 'failure'::text AND observed_sha256 IS NOT NULL
                   WHEN 'missing_object' THEN outcome = 'failure'::text AND observed_sha256 IS NULL AND fixity_status = 'missing'::text
                   WHEN 'read_error' THEN outcome = 'failure'::text AND observed_sha256 IS NULL
                   ELSE false
               END),
    CONSTRAINT asset_preservation_events_algorithm_check
        CHECK (algorithm IS NULL OR algorithm = 'sha256'::text),
    -- A digest is reported with its algorithm and size, or not at all.
    CONSTRAINT asset_preservation_events_observed_check
        CHECK (((observed_sha256 IS NULL) = (algorithm IS NULL)) AND ((observed_sha256 IS NULL) = (observed_size_bytes IS NULL))),
    CONSTRAINT asset_preservation_events_hash_check
        CHECK (object_hash ~ '^[0-9a-f]{64}$'::text
               AND (observed_sha256 IS NULL OR observed_sha256 ~ '^[0-9a-f]{64}$'::text)
               AND (recorded_sha256 IS NULL OR recorded_sha256 ~ '^[0-9a-f]{64}$'::text)),
    CONSTRAINT asset_preservation_events_variant_key_check
        CHECK (length(variant_key) >= 1 AND length(variant_key) <= 256),
    CONSTRAINT asset_preservation_events_sizes_check
        CHECK (observed_generation >= 0 AND (observed_size_bytes IS NULL OR observed_size_bytes >= 0)),
    CONSTRAINT asset_preservation_events_fixity_status_check
        CHECK (fixity_status = ANY (ARRAY['verified'::text, 'unverifiable'::text, 'mismatch'::text, 'missing'::text])),
    -- The live reference, while it exists, is the snapshot.
    CONSTRAINT asset_preservation_events_sweep_run_check
        CHECK (sweep_run_ref IS NULL OR sweep_run_ref = sweep_run_id),
    CONSTRAINT asset_preservation_events_agent_check
        CHECK (length(agent_name) >= 1 AND length(agent_name) <= 128 AND (agent_version IS NULL OR length(agent_version) <= 64))
);

CREATE UNIQUE INDEX asset_preservation_events_sweep_item_key
    ON public.asset_preservation_events USING btree (sweep_run_id, asset_id, object_hash, variant_key, observed_generation, outcome_reason)
    WHERE (sweep_run_id IS NOT NULL);
CREATE INDEX asset_preservation_events_asset_idx
    ON public.asset_preservation_events USING btree (asset_id, occurred_at DESC);
CREATE INDEX asset_preservation_events_sweep_run_ref_idx
    ON public.asset_preservation_events USING btree (sweep_run_ref) WHERE (sweep_run_ref IS NOT NULL);

COMMENT ON TABLE public.asset_preservation_events IS 'An asset''s preservation history (#1598, ADR 0101, spec v1 §17.4): append-only PREMIS-style events, one per asset per observation. Only local fixity-check events today. object_hash/variant_key are the only file reference: no storage path or free text. sweep_run_id is an immutable snapshot that outlives the run row; sweep_run_ref is the live FK.';

-- +goose StatementBegin
CREATE FUNCTION public.asset_preservation_events_append_only() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        -- A run snapshot is taken from a run that exists (the FK checks
        -- sweep_run_ref), never asserted on its own.
        IF NEW.sweep_run_id IS DISTINCT FROM NEW.sweep_run_ref THEN
            RAISE EXCEPTION 'asset_preservation_events: sweep_run_id must name an existing run (#1598)'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        -- The one allowed change: the run row is gone and its ON DELETE
        -- SET NULL clears the live reference. Everything else, the
        -- snapshot included, stays as recorded.
        IF OLD.sweep_run_ref IS NOT NULL AND NEW.sweep_run_ref IS NULL
           AND (to_jsonb(NEW) - 'sweep_run_ref') = (to_jsonb(OLD) - 'sweep_run_ref')
           AND NOT EXISTS (SELECT 1 FROM public.storage_sweep_runs WHERE id = OLD.sweep_run_ref) THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'asset_preservation_events is append-only (#1598)'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    -- A DELETE is allowed only as its asset goes (the cascade runs after
    -- the asset row is gone).
    IF EXISTS (SELECT 1 FROM public.assets WHERE id = OLD.asset_id) THEN
        RAISE EXCEPTION 'asset_preservation_events is append-only (#1598)'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN OLD;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER asset_preservation_events_append_only_trg
    BEFORE INSERT OR UPDATE OR DELETE ON public.asset_preservation_events
    FOR EACH ROW EXECUTE FUNCTION public.asset_preservation_events_append_only();

-- Sweep findings: one per (run, object, variant, finding), so a retried
-- batch cannot duplicate one. Collapse any exact duplicates first.
DELETE FROM public.storage_sweep_findings f
 USING public.storage_sweep_findings keep
 WHERE f.run_id = keep.run_id
   AND f.object_hash = keep.object_hash
   AND f.variant_key = keep.variant_key
   AND f.finding = keep.finding
   AND (f.detected_at, f.id) > (keep.detected_at, keep.id);

CREATE UNIQUE INDEX storage_sweep_findings_item_key
    ON public.storage_sweep_findings USING btree (run_id, object_hash, variant_key, finding);

-- +goose Down

DROP INDEX IF EXISTS public.storage_sweep_findings_item_key;
DROP TABLE IF EXISTS public.asset_preservation_events;
DROP FUNCTION IF EXISTS public.asset_preservation_events_append_only();
