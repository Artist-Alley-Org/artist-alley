-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00033_theme_system_preference.sql
--
-- Let an account store "follow the OS" as a theme, distinct from
-- storing nothing at all (#677).
--
-- The frontend has always had three theme states — light, dark, and
-- `system`, offered in the user menu, the mobile drawer and account
-- preferences — but they only ever lived in the `aa_theme` cookie, so
-- the choice stopped at the browser that made it. Wiring the account
-- copy up is what #677 asks for, and the moment it is wired the
-- column's two-and-a-half states stop being enough:
--
--   ''      the account has NO stored preference
--   'light' explicit
--   'dark'  explicit
--
-- With `system` collapsed onto '', a user who deliberately picks
-- "follow my OS" stores the same byte as a user who has never touched
-- the setting. Their second device cannot tell the two apart, so it
-- either ignores a real choice or overrides a non-choice — and the
-- non-choice fallback is dark (#590), which is exactly the value a
-- light-OS user picked `system` to avoid. That ambiguity is the same
-- class of bug #677 is about, so it gets fixed in the same migration
-- rather than left as a footnote.
--
-- Nothing needs backfilling. Existing '' rows keep meaning "no stored
-- preference", which is what they have always meant; `system` is a new
-- value only a deliberate choice writes.
--
-- Plain DDL, so no StatementBegin/End markers — those exist for
-- plpgsql bodies whose semicolons goose would otherwise split on.

-- +goose Up

ALTER TABLE public.user_profiles
    DROP CONSTRAINT user_profiles_theme_check;

ALTER TABLE public.user_profiles
    ADD CONSTRAINT user_profiles_theme_check
    CHECK (theme = ANY (ARRAY[''::text, 'light'::text, 'dark'::text, 'system'::text]));

-- +goose Down

-- Any row holding the value this migration introduced has to go
-- somewhere the old constraint accepts, and '' is the honest landing
-- spot: it is what "follow the OS" meant before the value existed.
UPDATE public.user_profiles SET theme = '' WHERE theme = 'system';

ALTER TABLE public.user_profiles
    DROP CONSTRAINT user_profiles_theme_check;

ALTER TABLE public.user_profiles
    ADD CONSTRAINT user_profiles_theme_check
    CHECK (theme = ANY (ARRAY[''::text, 'light'::text, 'dark'::text]));
