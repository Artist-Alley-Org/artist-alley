-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00079_federation_remote_comment_tombstones.sql
--
-- #1572 (second slice): a federated Delete of a comment whose Create has
-- not arrived (or never will) is recorded, so the Create is never applied
-- after its own Delete, whatever the delivery order.
--
-- A remote comment is already bound to its Create: comments.activity_uri
-- (the Create's signed id), comments.peer_id and comments.actor_uri. A
-- Delete names that Create id and is applied only to the comment with the
-- same peer, actor and Create id (spec v1 §7.5.5). When there is no such
-- comment it stores a tombstone here, keyed by the same three values: a
-- Create with exactly that identity is then processed with no comment
-- row and no notification. The key includes the peer and the actor, so a
-- Delete from another actor or another peer never matches, can never
-- pre-empt the genuine Create, and is answered the same way as any other
-- unknown id.
--
-- Rows go only with their peer (ON DELETE CASCADE). No local target is
-- named, so nothing here is reached by, or needs, a content reset.

-- +goose Up

CREATE TABLE public.federation_remote_comment_tombstones (
    peer_id uuid NOT NULL,
    actor_uri text NOT NULL,
    create_activity_uri text NOT NULL,
    delete_activity_uri text NOT NULL,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT federation_remote_comment_tombstones_pkey PRIMARY KEY (peer_id, actor_uri, create_activity_uri),
    CONSTRAINT federation_remote_comment_tombstones_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE
);

COMMENT ON TABLE public.federation_remote_comment_tombstones IS 'A federated Delete of a comment whose Create this instance has not received (#1572), keyed by (peer, signed actor, Create activity id). A Create with exactly that identity is processed with no comment and no notification. Another actor or peer never matches. Goes only with its peer.';

-- +goose Down

DROP TABLE IF EXISTS public.federation_remote_comment_tombstones;
