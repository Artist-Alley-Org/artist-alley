-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00013_audit_retention.sql
--
-- Audit retention + legal hold (#467, epic #52, ADR 0032 §Retention).
--
-- Three additions:
--
--   1. audit_events.legal_hold — a per-row exemption. A held row is
--      never purged regardless of its category's policy (ADR 0032:
--      "per-row retention overrides for legal-hold events").
--
--   2. audit_events.category — a STORED generated column, the first
--      dotted segment of event_type. Retention policy is keyed by
--      category, and the purge scans by (category, occurred_at); a
--      generated+indexed column makes that scan index-driven instead
--      of a split_part() over every row. The current events use
--      prefixes like login / admin / federation / session — the ADR's
--      auth.* / commerce.* taxonomy is aspirational, so the policy is
--      keyed on whatever prefix the row actually carries.
--
--   3. audit_retention_policy — per-category retention durations.
--      A TABLE, not a system_config blob (design call, see the PR):
--      the config has structure (category, duration, who changed it,
--      when), the purge reads it as rows, and an admin edit surface is
--      natural CRUD. A category with no row falls to the code default
--      of 7 years (ADR 0032's legal floor).
--
-- WHAT THIS DOES NOT DO: it adds no license_kid column and no signing
-- machinery. Signed export is Enterprise-tier (ADR 0017, #27-gated) and
-- stays on epic #52 in v0.7.0 — adding license_kid here speculatively
-- would be dead schema until then.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE public.audit_events
    ADD COLUMN legal_hold boolean NOT NULL DEFAULT false;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE public.audit_events
    ADD COLUMN category text
    GENERATED ALWAYS AS (split_part(event_type, '.', 1)) STORED;
-- +goose StatementEnd

-- The purge's due-scan: rows of one category older than a cutoff that
-- are NOT held. Partial on legal_hold = false so held rows never enter
-- the index the purge walks.
-- +goose StatementBegin
CREATE INDEX audit_events_retention_idx
    ON public.audit_events (category, occurred_at)
    WHERE legal_hold = false;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE public.audit_retention_policy (
    category   text PRIMARY KEY,
    retention  interval    NOT NULL,
    updated_by bigint,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT audit_retention_policy_positive CHECK (retention > interval '0')
);
-- +goose StatementEnd

-- Seed the two ADR examples mapped onto the prefixes this install
-- actually emits: authentication churn is high-volume and low-value
-- past a short window, so login + session age out at 90 days;
-- everything unlisted keeps the 7-year default enforced in code.
-- commerce is seeded forward-looking (10 years, financial-record floor)
-- even though no commerce events exist yet, so the policy is right the
-- day the first one lands.
-- +goose StatementBegin
INSERT INTO public.audit_retention_policy (category, retention) VALUES
    ('login',    interval '90 days'),
    ('session',  interval '90 days'),
    ('logout',   interval '90 days'),
    ('commerce', interval '10 years')
ON CONFLICT (category) DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE public.audit_retention_policy;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS public.audit_events_retention_idx;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE public.audit_events
    DROP COLUMN category,
    DROP COLUMN legal_hold;
-- +goose StatementEnd
