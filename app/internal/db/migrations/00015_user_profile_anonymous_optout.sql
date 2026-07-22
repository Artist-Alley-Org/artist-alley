-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00015_user_profile_anonymous_optout.sql
--
-- Owner opt-out from anonymous profile exposure (#478, ADR 0070 → ADR 0024).
--
-- Public user-profile pages are anonymous-visible when public mode is on
-- (ADR 0063). ADR 0024's privacy frame says anonymous exposure is
-- default-off with a per-user opt-out, and ADR 0070 is explicit that the
-- opt-out "must be wired before profiles go anonymous, not after."
--
-- `hide_from_anonymous` is that opt-out: default FALSE (a user's profile
-- is anonymous-visible under public mode like the rest of the surface);
-- when a user sets it TRUE, an anonymous viewer gets a 404 for their
-- profile even with public mode on. It changes nothing for authenticated
-- viewers, and nothing about WHAT content a profile shows — the content
-- lists remain gated by the visibility predicate (ADR 0063). It only
-- decides whether an anonymous viewer may see the profile at all.

-- +goose Up
ALTER TABLE public.user_profiles
    ADD COLUMN hide_from_anonymous boolean DEFAULT false NOT NULL;

-- +goose Down
ALTER TABLE public.user_profiles
    DROP COLUMN hide_from_anonymous;
