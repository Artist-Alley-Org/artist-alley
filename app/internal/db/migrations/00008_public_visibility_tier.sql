-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00008_public_visibility_tier.sql
--
-- Add the `public` visibility tier (#414, P0a).
--
-- Neither collections nor posts could express "anyone may read this":
-- both CHECKs allowed only private | org-only | followers |
-- explicit-share. That had two consequences.
--
-- 1. Public mode (#413 arc) had nothing to express. The collection
--    branch of visibility.ToSQL short-circuited anonymous callers to
--    `FALSE` because no collection could ever qualify.
-- 2. The POST branch was dead code: it filtered `visibility = 'public'`,
--    a value this very constraint forbade, so it could only ever match
--    zero rows. It looked like working anonymous support and was not.
--
-- This migration only widens the domain. It deliberately does NOT
-- rewrite any existing row: adding the tier and opting content into it
-- are separate decisions, and the second one belongs to an operator,
-- not to a migration. Every existing collection and post keeps exactly
-- the visibility it has, so this is inert until someone chooses
-- otherwise.
--
-- Assets are untouched — they express the same idea through
-- `sensitivity` ('public' already exists there) plus `status`.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE public.collections DROP CONSTRAINT IF EXISTS collections_visibility_check;
ALTER TABLE public.collections ADD CONSTRAINT collections_visibility_check
    CHECK ((visibility = ANY (ARRAY[
        'private'::text,
        'org-only'::text,
        'followers'::text,
        'explicit-share'::text,
        'public'::text
    ])));
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE public.posts DROP CONSTRAINT IF EXISTS posts_visibility_check;
ALTER TABLE public.posts ADD CONSTRAINT posts_visibility_check
    CHECK ((visibility = ANY (ARRAY[
        'private'::text,
        'org-only'::text,
        'followers'::text,
        'explicit-share'::text,
        'public'::text
    ])));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Narrowing the domain again fails loudly if any row has opted into
-- 'public' — which is correct: silently rewriting live visibility on a
-- rollback would be a leak in the other direction (content the operator
-- published becoming private is bad, but content becoming MORE visible
-- would be worse; either way the operator decides, not the migration).
ALTER TABLE public.collections DROP CONSTRAINT IF EXISTS collections_visibility_check;
ALTER TABLE public.collections ADD CONSTRAINT collections_visibility_check
    CHECK ((visibility = ANY (ARRAY[
        'private'::text, 'org-only'::text, 'followers'::text, 'explicit-share'::text
    ])));
ALTER TABLE public.posts DROP CONSTRAINT IF EXISTS posts_visibility_check;
ALTER TABLE public.posts ADD CONSTRAINT posts_visibility_check
    CHECK ((visibility = ANY (ARRAY[
        'private'::text, 'org-only'::text, 'followers'::text, 'explicit-share'::text
    ])));
-- +goose StatementEnd
