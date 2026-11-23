-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00096_field_schema_import_receipt.sql
--
-- #1576 (ADR 0083, 2026-11-22 amendments): the durable record of an
-- applied `aa-field-schema/1` import, written in the same transaction as
-- the definitions it created. One receipt per applied preview, and one
-- item per incoming code.
--
-- This is receiver-recorded provenance: who applied what, when, the
-- digest of the exact document reviewed, the digest of the exact
-- decisions, and per code the decision, the outcome and the local
-- definition id at that moment. The source label and URI are copied
-- from the preview as what the operator typed: untrusted claims, never
-- fetched, never peer identity.
--
-- Items are keyed by (receipt_id, ordinal), the code's position in the
-- sorted decisions, 0 to 99. The code itself is plain TEXT with no index,
-- so a receipt imposes no limit on code length: a code too long to index
-- as a field can still be recorded as skipped. Code uniqueness within a
-- receipt follows from the strict document (unique codes) and the exact
-- decision key set.
--
-- preview_id UNIQUE is the replay key: an identical repeated apply
-- answers from this row, and the preview row itself is deleted on
-- success. field_id deliberately has no foreign key, so a later change
-- or removal of the definition cannot rewrite what the receipt says.
-- Receipts are not swept; they grow only by explicit applies.

-- +goose Up
CREATE TABLE public.field_schema_import_receipt (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    preview_id uuid NOT NULL,
    actor_ref bigint,
    applied_at timestamp with time zone NOT NULL,
    format text NOT NULL,
    document_digest text NOT NULL,
    decisions_digest text NOT NULL,
    decisions jsonb NOT NULL,
    source_label text,
    source_uri text,
    changed integer NOT NULL,
    CONSTRAINT field_schema_import_receipt_pkey PRIMARY KEY (id),
    CONSTRAINT field_schema_import_receipt_preview_key UNIQUE (preview_id),
    CONSTRAINT field_schema_import_receipt_actor_fkey FOREIGN KEY (actor_ref) REFERENCES public."user"(ref) ON DELETE SET NULL,
    CONSTRAINT field_schema_import_receipt_changed_check CHECK ((changed >= 0))
);

CREATE TABLE public.field_schema_import_receipt_item (
    receipt_id uuid NOT NULL,
    ordinal smallint NOT NULL,
    code text NOT NULL,
    decision text NOT NULL,
    outcome text NOT NULL,
    field_id uuid,
    incoming_field_digest text NOT NULL,
    CONSTRAINT field_schema_import_receipt_item_pkey PRIMARY KEY (receipt_id, ordinal),
    CONSTRAINT field_schema_import_receipt_item_ordinal_check CHECK (((ordinal >= 0) AND (ordinal <= 99))),
    CONSTRAINT field_schema_import_receipt_item_receipt_fkey FOREIGN KEY (receipt_id) REFERENCES public.field_schema_import_receipt(id) ON DELETE CASCADE,
    CONSTRAINT field_schema_import_receipt_item_decision_check CHECK ((decision = ANY (ARRAY['create'::text, 'use_existing'::text, 'keep_local'::text, 'skip'::text]))),
    CONSTRAINT field_schema_import_receipt_item_outcome_check CHECK ((outcome = ANY (ARRAY['created'::text, 'matched'::text, 'kept_local'::text, 'skipped'::text])))
);

-- The read-only history lists receipts newest first, keyset-paged on
-- (applied_at, id). No index on any code.
CREATE INDEX field_schema_import_receipt_applied_idx
    ON public.field_schema_import_receipt USING btree (applied_at DESC, id DESC);

-- +goose Down
DROP TABLE IF EXISTS public.field_schema_import_receipt_item;
DROP TABLE IF EXISTS public.field_schema_import_receipt;
