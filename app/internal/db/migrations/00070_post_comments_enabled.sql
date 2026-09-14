-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00070_post_comments_enabled.sql
--
-- A post carries its author's decision about whether it takes
-- comments (#1119, sprint 21d).
--
-- ## What it is, and what it is not
--
-- A SETTING OF THE POST. Not a user preference (two posts by one
-- author hold independent values), not a capability (`posts.comment`
-- says whether a CALLER may comment anywhere; this says whether THIS
-- POST accepts one from anybody), and not a workflow state (nothing
-- about publication moves when it flips). It is read by exactly one
-- gate, the ordinary comment-create path, and by the surfaces that
-- render or edit the post.
--
-- ## NOT NULL DEFAULT true, and why there is no third state
--
-- "Unset" is not a product state. A post either takes comments or it
-- does not, and a nullable column would make every reader decide what
-- NULL means, differently. The default is what every post has behaved
-- like since comments existed, so the migration changes nothing about
-- any existing post: they were all commentable before, they are all
-- commentable after, and the column says so explicitly rather than by
-- absence. No backfill statement is needed: ADD COLUMN with a
-- non-volatile DEFAULT writes the value for every existing row.
--
-- ## Creation only
--
-- Disabling withdraws the ability to ADD an ordinary comment or reply.
-- It deletes nothing, hides nothing, and changes no listing, deletion
-- or moderation rule: a thread that was readable stays readable, with
-- every row it had. Whiteboards and annotations share the `comments`
-- table but are their own surfaces with their own routes, and this
-- column is deliberately not consulted by them.
--
-- ## Read under the row lock at comment time
--
-- The comment-create transaction reads this column with
-- FOR NO KEY UPDATE before it inserts, which is the lock the
-- comment_count trigger's UPDATE on the same row takes anyway. A
-- disable that has committed is therefore seen by every comment
-- transaction that starts its gate read afterwards, and one that is
-- still open blocks the disable until it commits: a comment either
-- committed before the disable or cannot commit at all. There is no
-- window in which a check-then-insert lets one land after.
--
-- Plain DDL, so no StatementBegin/End markers.

-- +goose Up

ALTER TABLE public.posts
    ADD COLUMN comments_enabled boolean NOT NULL DEFAULT true;

COMMENT ON COLUMN public.posts.comments_enabled IS
    'Whether this post accepts NEW ordinary comments and replies (#1119 sprint 21d). A setting of the post, chosen by whoever may edit it: not a user preference (two posts by one author differ independently), not a capability (`posts.comment` says whether a caller may comment at all; this says whether THIS post takes one from anybody, and `system.admin` does not bypass it), and not a workflow state. NOT NULL because "unset" is not a product state; DEFAULT true because that is how every post behaved before the column existed. CREATION ONLY: false refuses POST /posts/{id}/comments with 409 `comments_disabled` and nothing else changes, so existing comments stay readable wherever the thread was readable, and listing, deletion and moderation are untouched. Whiteboards and annotations are separate paths and do not read this column. The comment-create transaction reads it FOR NO KEY UPDATE before inserting, so a committed disable is never raced by a check-then-insert.';

-- +goose Down

ALTER TABLE public.posts
    DROP COLUMN comments_enabled;
