-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00095_field_schema_import_preview.sql
--
-- #1576 (ADR 0083, 2026-11-22 amendments): the operator's reviewed
-- preview of an `aa-field-schema/1` import. One row per preview.
--
-- A LIVE row holds what a later apply needs to bind the operator's
-- review: the canonical document bytes (bytea, so the exact bytes are
-- kept; jsonb would normalise them and refuses U+0000), the digest of
-- those bytes, the untrusted source claims the operator typed, the
-- decisions allowed per code, and the reviewed state of every local row
-- the preview read (a digest per present row, the list of absent codes).
--
-- An EXPIRED row is a compact tombstone: the payload and the source
-- claims are erased, and only the identity, actor, times and document
-- digest remain, so an apply can answer 410 while it is kept and 404
-- after it is deleted.
--
-- Admission counts every row still holding a payload, expired or not,
-- so retained payload stays bounded even when the cleanup is late.
--
-- No receipt or consumed state: those arrive with apply.

-- +goose Up
CREATE TABLE public.field_schema_import_preview (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    actor_ref bigint NOT NULL,
    created_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    state text NOT NULL,
    document bytea,
    document_digest text NOT NULL,
    source_label text,
    source_uri text,
    allowed jsonb,
    bound_state jsonb,
    tombstone_until timestamp with time zone,
    CONSTRAINT field_schema_import_preview_pkey PRIMARY KEY (id),
    CONSTRAINT field_schema_import_preview_actor_fkey FOREIGN KEY (actor_ref) REFERENCES public."user"(ref) ON DELETE CASCADE,
    CONSTRAINT field_schema_import_preview_state_check CHECK ((state = ANY (ARRAY['live'::text, 'expired'::text]))),
    CONSTRAINT field_schema_import_preview_expiry_check CHECK ((expires_at > created_at)),
    CONSTRAINT field_schema_import_preview_payload_check CHECK ((((state = 'live'::text) AND (document IS NOT NULL) AND (allowed IS NOT NULL) AND (bound_state IS NOT NULL) AND (tombstone_until IS NULL)) OR ((state = 'expired'::text) AND (document IS NULL) AND (allowed IS NULL) AND (bound_state IS NULL) AND (tombstone_until IS NOT NULL)))),
    CONSTRAINT field_schema_import_preview_label_check CHECK (((source_label IS NULL) OR ((char_length(source_label) >= 1) AND (char_length(source_label) <= 200)))),
    CONSTRAINT field_schema_import_preview_uri_check CHECK (((source_uri IS NULL) OR ((octet_length(source_uri) >= 1) AND (octet_length(source_uri) <= 2048))))
);

-- Admission counts payload-holding rows per actor and in total.
CREATE INDEX field_schema_import_preview_payload_idx
    ON public.field_schema_import_preview USING btree (actor_ref) WHERE (document IS NOT NULL);

-- The cleanup walks live rows by expiry and tombstones by retention.
CREATE INDEX field_schema_import_preview_sweep_idx
    ON public.field_schema_import_preview USING btree (state, expires_at);

-- +goose Down
DROP TABLE IF EXISTS public.field_schema_import_preview;
