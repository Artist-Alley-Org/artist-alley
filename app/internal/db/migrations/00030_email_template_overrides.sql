-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00030_email_template_overrides.sql
--
-- Operator-authored email templates (#795, ADR 0081 §2 as amended
-- 2026-07-31).
--
-- One row per (template_name, part). `template_name` is the same stable
-- string the notification job + the shipped registry already key on
-- (admin_test, notification_generic, register_verify, …); `part` is one
-- of the three faces every template has — subject / text / html. An
-- override replaces exactly that one face; the other two, and every
-- template with no row here, render from the binary-embedded shipped
-- template. Missing override → shipped, mirroring templateForVerb's
-- own "no per-verb template → notification_generic" fallback.
--
-- DELIBERATELY PER-ROW, NOT A JSONB DOCUMENT (the #794/site_text
-- posture). Two operators editing two different parts touch two
-- different primary keys and cannot lose each other's write — no
-- whole-document read-modify-write race (#737).
--
-- No `language` column THIS sprint: Render takes no locale and
-- notifications are English-only until #289. The PK is (template_name,
-- part); adding `language` later widens the key without reshaping the
-- table — cheap to add, expensive to remove once written against.
--
-- The set of legal template_names + parts is enforced in Go against a
-- build-embedded view-model (ADR 0081 §2: the documented field set IS
-- the security boundary), not by a CHECK constraint here — the same
-- reason site_text validates keys against the embedded catalogue rather
-- than the database: the binary owns the truth and can name the exact
-- field that was wrong.
--
-- Plain DDL, so no StatementBegin/End markers — those exist for
-- plpgsql bodies whose semicolons goose would otherwise split on.

-- +goose Up

CREATE TABLE IF NOT EXISTS public.email_template (
    template_name       text        NOT NULL,
    part                text        NOT NULL,
    body                text        NOT NULL,
    updated_at          timestamptz NOT NULL DEFAULT now(),
    -- SET NULL rather than CASCADE: deleting the operator who wrote an
    -- override must not silently delete the operator's wording.
    updated_by_user_ref bigint      REFERENCES public."user"(ref) ON DELETE SET NULL,
    CONSTRAINT email_template_pkey PRIMARY KEY (template_name, part)
);

-- No secondary index, deliberately. The only read is "every override",
-- rebuilt wholesale on invalidation and served from the process cache
-- thereafter (ADR 0013) — a sequential scan of a table holding at most
-- a couple dozen rows an operator populates by hand is cheaper than the
-- index that would serve a filtered read this API does not have.

-- +goose Down

DROP TABLE IF EXISTS public.email_template;
