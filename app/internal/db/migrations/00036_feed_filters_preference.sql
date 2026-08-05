-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00036_feed_filters_preference.sql
--
-- Give #891 somewhere to live: a per-user bag of BROWSE-FEED CONTENT
-- FILTERS, starting with "hide members I'm not entitled to see".
--
-- ## Why a jsonb column and not `hide_restricted boolean`
--
-- `user_preferences` is one jsonb bag PER CONCERN — notification_channels
-- (which channels for which event), default_views (which layout/tab/sort),
-- email_cadence (how often email fires). Each grew new keys over four
-- sub-phases without a migration, which is the entire point of the shape:
-- the typed Go struct in internal/userprefs/prefs.go enumerates the valid
-- keys, and openapi.yaml pins them for the client, so the DB never has to
-- learn about a new toggle.
--
-- "Which content the feed subtracts" is a new concern, not a new view
-- selection — default_views is about how the same set is ARRANGED, this is
-- about which rows reach the client at all — so it gets its own bag rather
-- than a fourth key in an existing one. `mute_tags`, `hide_muted_authors`
-- and friends land here as sibling keys with no further DDL.
--
-- ## Why NOT NULL DEFAULT '{}'
--
-- Same as its three siblings. An absent key inside the blob means "the
-- build's default for this filter", and every filter's default is OFF, so
-- an existing row and a brand-new one both read as "filter nothing" —
-- which is what makes the preference's default-off guarantee a property of
-- the storage rather than of remembering to write it.

-- +goose Up

ALTER TABLE public.user_preferences
    ADD COLUMN feed_filters jsonb DEFAULT '{}'::jsonb NOT NULL;

-- +goose Down

ALTER TABLE public.user_preferences
    DROP COLUMN feed_filters;
