-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00072_posts_recent_idx.sql
--
-- #1173, sprint 25b: an index for the post arm of a recent window.
--
-- `last:N` (`!lastN`) ranks the N newest eligible rows globally across
-- the searched entities and orders the page by recency. Assets and
-- collections already carry a descending index on their clock
-- (`assets_created_at_idx`, partial on `deleted_at IS NULL`, and
-- `collections_created_at_idx`). Posts rank on `posted_at`, the column
-- the browse feed orders by, and the only indexes over it are scoped to
-- one author (`posts_author_idx`) or to the public tier
-- (`posts_public_feed_idx`). A window formed for a signed-in reader
-- ranks every tier the post read rule admits, so neither serves it.
--
-- The index is `(posted_at DESC, id DESC)`, the exact keyset the arm
-- orders and positions on, partial on `deleted_at IS NULL` because every
-- surface that ranks posts excludes tombstones and the window's post
-- arm does too. No table or column changes, no backfill; plain DDL, so
-- no StatementBegin/End markers. At the corpus size this project runs
-- at (about a thousand posts) an ordinary CREATE INDEX inside goose's
-- transaction is the right shape.

-- +goose Up

CREATE INDEX posts_recent_idx ON public.posts USING btree (posted_at DESC, id DESC) WHERE (deleted_at IS NULL);

-- +goose Down

DROP INDEX IF EXISTS public.posts_recent_idx;
