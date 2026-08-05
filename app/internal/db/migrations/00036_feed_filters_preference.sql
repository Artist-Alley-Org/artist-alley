-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00036_feed_filters_preference.sql
--
-- Give #891 somewhere to live: a per-user bag of BROWSE-FEED CONTENT
-- FILTERS, starting with "hide members I'm not entitled to see".
--
-- ## Why a jsonb column and not `show_restricted boolean`
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
-- build's default for this filter", so an existing row and a brand-new one
-- both read as THE DEFAULT FEED — which is what makes the guarantee a
-- property of the storage rather than of remembering to write it.
--
-- ## The key names carry the default; the storage never does (#921)
--
-- That guarantee is about ABSENCE, not about any particular behaviour, and
-- the two came apart at #921. This column shipped with one key,
-- `hide_restricted`, defaulting to false — and at the time "absent" and
-- "filter nothing" were the same sentence, so the comment above said both.
-- #921 made hiding restricted work the DEFAULT feed, which broke that
-- sentence in half: absent still had to mean the build's default, but the
-- build's default was no longer "filter nothing".
--
-- The fix went into the NAME, not the default. The key is now
-- `show_restricted`, still defaulting to false, so absent still decodes to
-- the Go zero value and the zero value is still the default experience.
-- The rule the next key here must follow:
--
--   NAME EACH KEY SO THAT `false` IS WHAT THIS BUILD DOES BY DEFAULT.
--
-- The alternative — leaving a key called `hide_restricted` and defaulting
-- it to true — would have left an absent key meaning the exact opposite of
-- what its name asserts, in a column whose entire contract is what absence
-- means. Pre-release, with no operators to migrate, the rename is free;
-- there is no compatibility shim and none is wanted.
--
-- Note there is no 00037. Nothing about the SHAPE changed: same jsonb
-- column, same default, a different key spelled inside it. Editing this
-- file in place is correct while the schema is still pre-release.

-- +goose Up

ALTER TABLE public.user_preferences
    ADD COLUMN feed_filters jsonb DEFAULT '{}'::jsonb NOT NULL;

-- +goose Down

ALTER TABLE public.user_preferences
    DROP COLUMN feed_filters;
