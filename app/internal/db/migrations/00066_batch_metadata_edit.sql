-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00066_batch_metadata_edit.sql
--
-- The batch metadata edit (ADR 0019, #1173, #1119): one capability and
-- one table.
--
-- ## assets.metadata.bulk_edit — the INSTRUMENT, not the authority
--
-- Editing one asset's field value and editing a thousand of them are
-- the same authorisation question asked a thousand times, plus one
-- more: may this operator reach for the instrument at all. The five
-- gates the batch runs per target (bulk instrument in the target's
-- scope; ordinary subject authority; applicability; the field's own
-- write_capability; the field's read_capability) are the shipped rules,
-- and this capability composes with them rather than replacing any of
-- them. A caller holding it who fails the ordinary owner-or-team
-- mutation rule on a target still cannot write that target.
--
-- TEAM-SCOPE-AWARE, unlike `fields.vocabulary.extend` beside it. The
-- reason is the shape of the risk: a bulk edit's blast radius is the
-- selection, and an operator trusted to reshape one team's catalogue
-- is not thereby trusted to reshape another's. `Can(code, InTeam(t))`
-- is the per-target test, and a TEAM-LESS asset has no scope for a
-- scoped grant to match — only a GLOBAL holding reaches it. That is
-- the nullable trap visibility.MayMutate documents, and it is why a
-- scoped grant must never be read as "no scope required, therefore
-- anyone passes".
--
-- Seeded to `Admin` ONLY. Deliberately narrower than 00057's extend,
-- and for the opposite reason: extend had to preserve behaviour that
-- already shipped ungated, whereas nothing can bulk-edit today, so
-- there is no behaviour to preserve and the conservative default costs
-- nobody anything. Operators who want a bulk editor grant it — one
-- row, or one team-scoped grant.
--
-- NOT added to capLicenseFeatures: this is a content operation, not an
-- install-tier feature.
--
-- ## metadata_batch_preview — why the preview is DURABLE
--
-- The apply endpoint takes a token and nothing else of substance: no
-- mode, no value, no target list. Everything the apply needs is bound
-- into the token at preview time, which is what makes "was the
-- previewed set the applied set" answerable at all.
--
-- That binding has to live somewhere, and the alternative — a signed,
-- self-contained token carrying a thousand target UUIDs with their
-- partitions — was rejected on two counts. It puts tens of kilobytes
-- of opaque base64 on the wire for every apply, and it STILL needs a
-- durable row, because the token is SINGLE-USE and single-use is a
-- fact about the world rather than about the bearer. A server-side row
-- is the narrower representation: the wire carries 32 random bytes and
-- the binding stays where it can be transactionally consumed.
--
-- `token_hash` and not the token: the row is a credential lookup, and
-- a database that never holds the bearer secret cannot leak it. The
-- caller's bytes are hashed and compared, exactly as a session token
-- is.
--
-- `caller_user_ref` is the binding that makes the enumeration oracle
-- impossible to build. A token that resolves to a row belonging to
-- somebody else answers exactly as a token that resolves to no row at
-- all — one byte-identical 403 — so the presence, mode, expiry and
-- consumption state of another caller's preview are unobservable.
--
-- `consumed_at` is the single-use latch, and it is set INSIDE the same
-- transaction as the field writes and the audit envelope. That is the
-- whole point of the column: consumption, the durable mutations and
-- the envelope are one committed outcome, so a lost HTTP response can
-- never make a spent token spendable, and a refusal that wrote nothing
-- can never spend one.
--
-- `payload` holds the canonical value, the field's configuration
-- fingerprint (including its vocabulary options document) and the
-- ordered target set with each target's partition. jsonb rather than
-- columns because none of it is ever queried BY — it is read whole,
-- exactly once, by the apply that owns it.
--
-- ON DELETE CASCADE from field_definition: a preview for a field that
-- no longer exists cannot be applied under any reading, and leaving
-- orphans to be refused later is a worse answer than not keeping them.
--
-- Rows are swept opportunistically by the preview endpoint, well past
-- expiry, so the table stays bounded without a scheduler. A token whose
-- row has been swept answers 403 rather than 409, which is the correct
-- answer for a credential the server can no longer attribute.

-- +goose Up

INSERT INTO public.capabilities (code, description, created_at, required_license_feature)
VALUES (
    'assets.metadata.bulk_edit',
    'Use the batch metadata editor to change one field across many assets at once. Team-scope aware: a scoped grant reaches only assets in that team, and a team-less asset requires a global holding. Composes with — and never replaces — the ordinary per-asset mutation rule and the field''s own read and write capabilities.',
    now(),
    NULL
);

-- Admin only. See the header for why this is narrower than 00057.
INSERT INTO public.role_capabilities (role_id, capability_code)
VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'assets.metadata.bulk_edit');

CREATE TABLE public.metadata_batch_preview (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    token_hash bytea NOT NULL,
    caller_user_ref bigint NOT NULL,
    field_id uuid NOT NULL,
    mode text NOT NULL,
    would_change integer NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone,
    CONSTRAINT metadata_batch_preview_mode_check
        CHECK ((mode = ANY (ARRAY['overwrite'::text, 'fill_empties'::text, 'append'::text, 'remove'::text]))),
    CONSTRAINT metadata_batch_preview_would_change_check CHECK ((would_change >= 0))
);

ALTER TABLE ONLY public.metadata_batch_preview
    ADD CONSTRAINT metadata_batch_preview_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.metadata_batch_preview
    ADD CONSTRAINT metadata_batch_preview_token_hash_key UNIQUE (token_hash);

ALTER TABLE ONLY public.metadata_batch_preview
    ADD CONSTRAINT metadata_batch_preview_field_id_fkey
        FOREIGN KEY (field_id) REFERENCES public.field_definition(id) ON DELETE CASCADE;

CREATE INDEX metadata_batch_preview_expires_at_idx
    ON public.metadata_batch_preview USING btree (expires_at);

-- +goose Down

DROP TABLE IF EXISTS public.metadata_batch_preview;

DELETE FROM public.role_capabilities
 WHERE capability_code = 'assets.metadata.bulk_edit';

DELETE FROM public.user_capability_grants
 WHERE capability_code = 'assets.metadata.bulk_edit';

DELETE FROM public.user_capability_revokes
 WHERE capability_code = 'assets.metadata.bulk_edit';

DELETE FROM public.capabilities
 WHERE code = 'assets.metadata.bulk_edit';
