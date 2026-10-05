-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00059_post_draft_state.sql
--
-- The `wip` post state becomes reachable (ADR 0091 decision 7, #1161).
--
-- # What was actually there
--
-- The `post` workflow domain has had exactly two states since the
-- 00001 baseline — `wip` and `published` — with `published` as the
-- initial one, and both edges between them already defined in
-- `workflow_transitions` guarded by `posts.publish`. None of it was
-- reachable: `workflow.Service.Transition()` had zero callers, and no
-- read path anywhere in the product filtered on `posts.state_id`. The
-- column appeared in create/get/update/list projections and nowhere
-- else. A post was therefore born published and could never be
-- anything else, which is why ADR 0091's decision 6 ("unpublishing
-- returns a post to its author, intact") could not be implemented: it
-- had nowhere to return TO.
--
-- Two things stood between the data and the model. This migration is
-- both of them.
--
-- # 1. Rows with no state at all
--
-- `POST /posts` wrote whatever `state_id` the request body carried and
-- NULL when it carried none, so every post ever created through the
-- API has a NULL state; only the seeder set one. On the development
-- instance that is 228 of 1075 rows.
--
-- NULL cannot be left to mean "published by implication". The read
-- rule this migration exists to enable is written the fail-CLOSED way
-- round — a post is on a shared surface when its state IS the
-- `published` state, rather than when it is NOT the `wip` state —
-- because the two disagree precisely when the state is unknown, and
-- the safe answer to "I do not know whether this is published" is to
-- withhold it rather than to show it. `posts_state_id_fkey` is
-- ON DELETE SET NULL, so deleting a state row is exactly the event
-- that produces unknown states in bulk; under the other spelling that
-- event would publish every draft on the instance silently.
--
-- So the backfill is not cosmetic. Every existing post is published —
-- that is what "born published" means — and after this runs every
-- existing post SAYS so.
--
-- # 2. Nobody could publish anything
--
-- Both `wip` → `published` and `published` → `wip` require the
-- `posts.publish` capability, and `posts.publish` is granted to the
-- Admin role alone. The capability has been inert (its only reader,
-- Transition, was never called), so this has never bitten; the moment
-- publishing runs through the state machine it would mean no ordinary
-- artist can publish their own work.
--
-- Granting it to Base is the right fix rather than dropping the
-- requirement from the transitions. The capability keeps meaning what
-- its description says — "Move a post into the published state" — and
-- an operator who wants publication to be an approval step revokes it
-- from Base, at which point the edge closes for everyone at once and
-- the API answers 403 rather than half-working. Dropping it from the
-- transitions would delete that lever permanently.
--
-- The capability is NOT authorship. It says whether a caller may
-- publish at all; whether they may publish THIS post is the handler's
-- gate (author / global posts.admin / system.admin — the same narrow
-- one that guards a change of `visibility`, because publishing widens
-- who can reach the post). Anonymous does not get it: the Anonymous
-- role is a separate row and is untouched here.
--
-- # 3. A post born with no state at all
--
-- The backfill fixes the rows that exist. It does not stop the NEXT
-- INSERT that forgets the column — and there is more than one: the
-- seeder sets `state_id`, `POST /posts` now sets it, and every test
-- fixture in the tree writes `INSERT INTO posts (id, author_user_ref,
-- title, visibility)` and nothing else. Under a fail-closed read rule
-- a row with no state is invisible to everybody, so "forgot the
-- column" would present as "the post does not exist".
--
-- So the column gets a DEFAULT, and the model's own sentence — a post
-- is born published — becomes a property of the SCHEMA rather than a
-- thing each writer has to remember. It names the domain's INITIAL
-- state rather than `published` by literal, for the same reason
-- posts.createStateID does: an install that moved its entry point
-- should be obeyed.
--
-- A DEFAULT cannot contain a sub-select, so it is a STABLE function —
-- the only shape Postgres allows, and a cheap one: a unique-index
-- probe on a table with single-digit rows.

-- +goose Up

-- Every post that predates the draft model is published. See §1: this
-- is what makes the fail-closed read rule safe to turn on, and it must
-- run before any code reads the column.
UPDATE public.posts
   SET state_id = (SELECT id FROM public.workflow_states
                    WHERE domain = 'post' AND code = 'published')
 WHERE state_id IS NULL;

-- §2. Base is every signed-in user's floor role, so this is "an artist
-- may publish their own work". ON CONFLICT because a re-run (or an
-- install that already granted it) must not fail the migration.
INSERT INTO public.role_capabilities (role_id, capability_code)
SELECT r.id, 'posts.publish'
  FROM public.roles r
 WHERE r.name = 'Base'
ON CONFLICT (role_id, capability_code) DO NOTHING;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.post_initial_state_id() RETURNS uuid
    LANGUAGE sql STABLE
    AS $$
    SELECT id FROM public.workflow_states
     WHERE domain = 'post' AND is_initial
     LIMIT 1
$$;
-- +goose StatementEnd

ALTER TABLE public.posts
    ALTER COLUMN state_id SET DEFAULT public.post_initial_state_id();

COMMENT ON FUNCTION public.post_initial_state_id() IS
    'The `post` workflow domain''s entry-point state, as the DEFAULT for posts.state_id (ADR 0091 decision 7). A function because a column DEFAULT cannot hold a sub-select. Its job is to make "a post is born published" true of the SCHEMA rather than of each INSERT path in turn — the seeder, the API handler and every test fixture would otherwise each have to remember, and under the fail-closed read rule forgetting means the post is invisible to everybody including its author. Explicit writers (posts.createStateID) still choose, which is how a draft gets created.';

COMMENT ON COLUMN public.posts.state_id IS
    'The post''s workflow state, domain = ''post'' (ADR 0091 decision 7). Two states are reachable: `published` — the post is on shared surfaces — and `wip`, the DRAFT state, which is visible to its author and to a posts.admin holder and appears on no shared surface at all. Set by POST /posts from the request''s `draft` flag and moved only by workflow.Service.Transition (POST /posts/{id}/publish and /unpublish), which validates the edge and writes workflow_audit; no request body accepts this column. READ FAIL-CLOSED: visibility.postPublishedExpr asks `state_id = <published>`, so a NULL or unrecognised state withholds the post rather than showing it — the FK is ON DELETE SET NULL, and the other spelling would publish every draft the moment a state row was deleted. This is the ONE place a workflow state decides publication, and it is deliberate: the `post` domain has exactly these two states and ADR 0091 identifies them with draft/published. An ASSET''s workflow state means something else entirely — where the file is in its production process — and must never be read this way.';

-- +goose Down

-- The grant goes; the backfill does not. Reverting to "state_id is
-- NULL for API-created posts" would mean re-introducing rows the read
-- rule cannot classify, and under fail-closed reading that HIDES them.
-- Every post being explicitly published is correct under the old model
-- too, where nothing read the column at all.
DELETE FROM public.role_capabilities
 WHERE capability_code = 'posts.publish'
   AND role_id IN (SELECT id FROM public.roles WHERE name = 'Base');

ALTER TABLE public.posts ALTER COLUMN state_id DROP DEFAULT;
DROP FUNCTION IF EXISTS public.post_initial_state_id();

COMMENT ON COLUMN public.posts.state_id IS NULL;
