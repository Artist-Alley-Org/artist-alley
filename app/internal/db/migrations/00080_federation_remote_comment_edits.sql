-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00080_federation_remote_comment_edits.sql
--
-- #1572 (third slice): a federated Update (edit) of a comment.
--
-- An edit is bound like a Delete: to the comment with the same peer,
-- signed actor and Create id (comments.peer_id, actor_uri, activity_uri).
-- Edits are ordered by the Update's signed `published`, which a sender
-- makes strictly increasing per comment (spec v1 §7.5.5); a tie, which a
-- conforming sender never produces, is broken by the Update's activity
-- id, so every receiver keeps the same edit whatever the delivery order.
--
-- federation_remote_comment_edits: the edit a remote comment currently
-- shows, one row per edited comment, cascading with the comment. An
-- Update applies only when its (published, id) is greater; an older or
-- repeated one changes nothing. Local comments never have a row. (A
-- table of its own rather than columns on comments, so no query that
-- reads comments changes shape.)
--
-- federation_remote_comment_pending_edits: an Update whose Create has not
-- arrived, keyed by (peer, actor, Create id), holding only the newest
-- edit. The Create that arrives later is stored with that edit's content.
-- A Delete of the comment discards it (a deleted comment is never brought
-- back by an edit), and an Update of a comment already deleted, or
-- tombstoned, is not kept. Rows go only with their peer.

-- +goose Up

CREATE TABLE public.federation_remote_comment_edits (
    comment_id uuid NOT NULL,
    update_activity_uri text NOT NULL,
    published timestamp with time zone NOT NULL,
    applied_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT federation_remote_comment_edits_pkey PRIMARY KEY (comment_id),
    CONSTRAINT federation_remote_comment_edits_comment_id_fkey FOREIGN KEY (comment_id) REFERENCES public.comments(id) ON DELETE CASCADE
);

COMMENT ON TABLE public.federation_remote_comment_edits IS 'The federated Update (#1572) a remote comment currently shows: its signed published time and id, the order key. An Update applies only when its (published, id) is greater. No row until the first edit, and never for a local comment.';

CREATE TABLE public.federation_remote_comment_pending_edits (
    peer_id uuid NOT NULL,
    actor_uri text NOT NULL,
    create_activity_uri text NOT NULL,
    update_activity_uri text NOT NULL,
    content text NOT NULL,
    published timestamp with time zone NOT NULL,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT federation_remote_comment_pending_edits_pkey PRIMARY KEY (peer_id, actor_uri, create_activity_uri),
    CONSTRAINT federation_remote_comment_pending_edits_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE
);

COMMENT ON TABLE public.federation_remote_comment_pending_edits IS 'The newest federated Update of a comment whose Create has not arrived (#1572), keyed by (peer, signed actor, Create id). The Create is stored with this content. A Delete of the comment discards it. Goes only with its peer.';

-- +goose Down

DROP TABLE IF EXISTS public.federation_remote_comment_pending_edits;
DROP TABLE IF EXISTS public.federation_remote_comment_edits;
