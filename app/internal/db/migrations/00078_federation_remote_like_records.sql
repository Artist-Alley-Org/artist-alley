-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00078_federation_remote_like_records.sql
--
-- #1572 (first slice): an inbound Like is bound to its activity id, so a
-- later Undo can retract exactly that Like and nothing else.
--
-- federation_remote_likes: one row per inbound Like activity, keyed by
-- the sending peer, the signed actor and the Like's activity id. The
-- aggregate `likes` row for (target, peer, actor) exists exactly while
-- at least one of that actor's records for the target is live
-- (undone_at IS NULL), so one actor sending two distinct Likes of the
-- same target still counts once, and an Undo of one of them leaves the
-- like in place while the other is live.
--
-- An Undo that arrives before its Like (or names an id this instance
-- never saw) stores a tombstone: binding 'tombstone', no target, already
-- undone. A Like that later arrives with the same peer, actor and id is
-- recorded undone and creates no aggregate row and no notification. An
-- Undo from another actor of the same peer never matches the record,
-- because the actor is part of the key; it only stores its own
-- tombstone, so the answer is the same whether or not the id exists.
--
-- Retractions need no live share: they only withdraw the actor's own
-- earlier contribution (spec v1 §7.1, §7.5.4).
--
-- Backfill of likes received before this migration: a record is made
-- only from a processed federation_inbox Like row with the same peer,
-- actor and target as an existing `likes` row (binding 'backfilled').
-- A remote `likes` row with no such inbox row is kept and bound to a
-- synthetic id 'urn:aa:legacy-like:<likes.id>' (binding
-- 'legacy_unbound'). An Undo's object must be an https activity URI on
-- the actor's origin, so no Undo can name it, and it keeps that actor's
-- like of the target live: an Undo of a later, bound Like by the same
-- actor leaves the like in place. No federated activity removes it.
-- The number of each kind is reported by a NOTICE when this migration
-- runs.

-- +goose Up

CREATE TABLE public.federation_remote_likes (
    peer_id uuid NOT NULL,
    actor_uri text NOT NULL,
    activity_uri text NOT NULL,
    target_kind text,
    target_id uuid,
    binding text NOT NULL,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    undone_at timestamp with time zone,
    undo_activity_uri text,
    CONSTRAINT federation_remote_likes_pkey PRIMARY KEY (peer_id, actor_uri, activity_uri),
    CONSTRAINT federation_remote_likes_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE,
    CONSTRAINT federation_remote_likes_binding_check CHECK ((binding = ANY (ARRAY['received'::text, 'backfilled'::text, 'legacy_unbound'::text, 'tombstone'::text]))),
    CONSTRAINT federation_remote_likes_target_check CHECK (((target_kind IS NULL) = (target_id IS NULL))),
    CONSTRAINT federation_remote_likes_target_kind_check CHECK (((target_kind IS NULL) OR (target_kind = ANY (ARRAY['post'::text, 'asset'::text, 'comment'::text])))),
    CONSTRAINT federation_remote_likes_undo_check CHECK (((undone_at IS NULL) = (undo_activity_uri IS NULL))),
    CONSTRAINT federation_remote_likes_tombstone_check CHECK (((binding = 'tombstone'::text) = (target_kind IS NULL))),
    CONSTRAINT federation_remote_likes_tombstone_undone_check CHECK (((binding <> 'tombstone'::text) OR (undone_at IS NOT NULL))),
    CONSTRAINT federation_remote_likes_legacy_live_check CHECK (((binding <> 'legacy_unbound'::text) OR (undone_at IS NULL)))
);

CREATE INDEX federation_remote_likes_live_idx ON public.federation_remote_likes USING btree (target_kind, target_id, peer_id, actor_uri) WHERE ((undone_at IS NULL) AND (target_id IS NOT NULL));

COMMENT ON TABLE public.federation_remote_likes IS 'Inbound Like activities bound to (peer, signed actor, Like activity id) (#1572). The aggregate likes row for (target, peer, actor) exists while at least one record is live. An Undo of an unseen id stores a tombstone (binding tombstone, no target, undone); a later matching Like is recorded undone and creates no like and no notification. Retractions need no live share.';

COMMENT ON COLUMN public.federation_remote_likes.binding IS 'received: recorded on arrival. backfilled: from a processed federation_inbox Like row (migration 00078). legacy_unbound: a like received before 00078 with no inbox evidence, bound to the synthetic urn:aa:legacy-like:<likes.id>, which no Undo can name. tombstone: an Undo whose Like has not arrived.';

INSERT INTO public.federation_remote_likes (peer_id, actor_uri, activity_uri, target_kind, target_id, binding, received_at)
SELECT DISTINCT ON (i.peer_id, i.actor_uri, i.activity_uri)
       i.peer_id, i.actor_uri, i.activity_uri, l.target_kind, l.target_id, 'backfilled',
       COALESCE(i.processed_at, i.received_at)
  FROM public.federation_inbox i
  JOIN public.likes l
    ON l.peer_id = i.peer_id
   AND l.actor_uri = i.actor_uri
   AND l.target_kind = i.object_kind
   AND l.target_id = i.object_id
 WHERE i.activity_type = 'Like'
   AND i.status = 'processed'
 ORDER BY i.peer_id, i.actor_uri, i.activity_uri;

INSERT INTO public.federation_remote_likes (peer_id, actor_uri, activity_uri, target_kind, target_id, binding, received_at)
SELECT l.peer_id, l.actor_uri, 'urn:aa:legacy-like:' || l.id::text, l.target_kind, l.target_id, 'legacy_unbound', l.liked_at
  FROM public.likes l
 WHERE l.peer_id IS NOT NULL
   AND NOT EXISTS (
       SELECT 1 FROM public.federation_remote_likes r
        WHERE r.peer_id = l.peer_id AND r.actor_uri = l.actor_uri
          AND r.target_kind = l.target_kind AND r.target_id = l.target_id);

-- +goose StatementBegin
DO $$
DECLARE
    n_backfilled bigint;
    n_unbound bigint;
BEGIN
    SELECT count(*) INTO n_backfilled FROM public.federation_remote_likes WHERE binding = 'backfilled';
    SELECT count(*) INTO n_unbound FROM public.federation_remote_likes WHERE binding = 'legacy_unbound';
    RAISE NOTICE '00078 federation_remote_likes: % like record(s) backfilled from processed inbox rows; % remote like(s) with no inbox evidence bound as legacy_unbound (no Undo can remove them)', n_backfilled, n_unbound;
END
$$;
-- +goose StatementEnd

-- +goose Down

DROP TABLE IF EXISTS public.federation_remote_likes;
