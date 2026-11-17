-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00077_federation_inbound_grants.sql
--
-- #1571: the receiver's own record of what paired peers granted it.
--
-- federation_inbound_grants: one row per aa:Share applied under the
-- share-lifecycle-1 profile, keyed by the sender's activity URI. A grant
-- is live or ended; nothing is ever deleted except with its peer (ON
-- DELETE CASCADE) or its bound recipient. Order comes only from the
-- signed references (aa:supersedes, aa:revokes), never from a clock.
-- The grant tuple is (peer, grantor actor, object, recipient), the
-- origin's own unique slot (federation_shares_active_uniq_idx: grantor,
-- object, peer, recipient), so two grantors' live shares of one object
-- coexist and either one authorises. At most one live grant per tuple.
--
-- The recipient is bound ONCE, when the grant is applied, to the local
-- user's immutable ref (usernames are immutable, spec §8.4). A target
-- user that does not exist stores the grant ended (recipient_unbound):
-- a user created later with that name gets nothing. NULL means any user
-- of this instance (a peer-wide grant).
--
-- federation_inbound_tombstones: an aa:Share referenced (revoked or
-- superseded) before it arrived. When it does arrive it is stored ended.
--
-- federation_inbound_memberships: which origin assets a granted
-- container (a collection) holds, as the origin announced them. Created
-- EMPTY here; authenticated membership ingest is #1574. Until a member is
-- recorded, a container grant authorises no file of it.
--
-- Authorisation reads these tables on every decision (no positive
-- cache): live, unexpired by THIS database's clock, on an enabled and
-- connected peer, for the asking recipient. Ending a grant takes its row
-- lock, which an in-flight import's final check holds FOR SHARE until
-- its commit (federation/casfetch state 7).

-- +goose Up

CREATE TABLE public.federation_inbound_grants (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    peer_id uuid NOT NULL,
    grant_activity_uri text NOT NULL,
    grantor_actor_uri text NOT NULL,
    object_kind text NOT NULL,
    object_uri text NOT NULL,
    target_user_uri text,
    recipient_user_ref bigint,
    scope text NOT NULL,
    allow_original boolean DEFAULT false NOT NULL,
    expires_at timestamp with time zone,
    supersedes_activity_uri text,
    state text NOT NULL,
    end_reason text,
    ended_by_activity_uri text,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    ended_at timestamp with time zone,
    CONSTRAINT federation_inbound_grants_pkey PRIMARY KEY (id),
    CONSTRAINT federation_inbound_grants_activity_key UNIQUE (peer_id, grant_activity_uri),
    CONSTRAINT federation_inbound_grants_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE,
    CONSTRAINT federation_inbound_grants_recipient_user_ref_fkey FOREIGN KEY (recipient_user_ref) REFERENCES public."user"(ref) ON DELETE CASCADE,
    CONSTRAINT federation_inbound_grants_object_kind_check CHECK ((object_kind = ANY (ARRAY['asset'::text, 'post'::text, 'collection'::text, 'workspace'::text, 'brand_kit'::text, 'user'::text]))),
    CONSTRAINT federation_inbound_grants_scope_check CHECK ((scope = ANY (ARRAY['view'::text, 'comment'::text, 'annotate'::text, 'remix'::text]))),
    CONSTRAINT federation_inbound_grants_allow_original_kind_check CHECK (((NOT allow_original) OR (object_kind = ANY (ARRAY['asset'::text, 'collection'::text])))),
    CONSTRAINT federation_inbound_grants_state_check CHECK ((state = ANY (ARRAY['live'::text, 'ended'::text]))),
    CONSTRAINT federation_inbound_grants_end_reason_check CHECK ((end_reason = ANY (ARRAY['revoked'::text, 'superseded'::text, 'ambiguous_supersede'::text, 'recipient_unbound'::text, 'tombstoned_on_arrival'::text, 'expired_on_arrival'::text]))),
    CONSTRAINT federation_inbound_grants_state_reason_check CHECK ((((state = 'live'::text) AND (end_reason IS NULL) AND (ended_at IS NULL)) OR ((state = 'ended'::text) AND (end_reason IS NOT NULL) AND (ended_at IS NOT NULL)))),
    CONSTRAINT federation_inbound_grants_unbound_check CHECK (((recipient_user_ref IS NULL) OR (target_user_uri IS NOT NULL)))
);

CREATE UNIQUE INDEX federation_inbound_grants_live_tuple_idx ON public.federation_inbound_grants USING btree (peer_id, grantor_actor_uri, object_uri, COALESCE(target_user_uri, ''::text)) WHERE (state = 'live'::text);

CREATE INDEX federation_inbound_grants_authz_idx ON public.federation_inbound_grants USING btree (peer_id, object_uri) WHERE (state = 'live'::text);

COMMENT ON TABLE public.federation_inbound_grants IS 'Grants a paired peer gave this instance (#1571), one row per applied aa:Share under share-lifecycle-1. Live or ended; ordered only by the signed aa:supersedes / aa:revokes references. Tuple (peer, grantor, object, recipient) has at most one live grant. Read on every authorisation (no positive cache), with expiry by this database clock and the peer enabled and connected. Ending a grant stops new access; it recalls nothing already delivered.';

COMMENT ON COLUMN public.federation_inbound_grants.recipient_user_ref IS 'The local user the grant is for, bound once at apply time from target_user_uri to the immutable user ref. NULL with target_user_uri NULL: any local user (peer-wide). NULL with target_user_uri set: the name did not resolve, so the grant is ended (recipient_unbound) and never activates.';

CREATE TABLE public.federation_inbound_tombstones (
    peer_id uuid NOT NULL,
    grant_activity_uri text NOT NULL,
    kind text NOT NULL,
    by_activity_uri text NOT NULL,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT federation_inbound_tombstones_pkey PRIMARY KEY (peer_id, grant_activity_uri),
    CONSTRAINT federation_inbound_tombstones_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE,
    CONSTRAINT federation_inbound_tombstones_kind_check CHECK ((kind = ANY (ARRAY['revoked'::text, 'superseded'::text])))
);

COMMENT ON TABLE public.federation_inbound_tombstones IS 'An aa:Share a peer revoked (aa:revokes) or superseded (aa:supersedes) before it arrived here (#1571). The grant is stored ended when it arrives, so a reordered delivery never grants.';

CREATE TABLE public.federation_inbound_memberships (
    peer_id uuid NOT NULL,
    container_uri text NOT NULL,
    member_uri text NOT NULL,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT federation_inbound_memberships_pkey PRIMARY KEY (peer_id, container_uri, member_uri),
    CONSTRAINT federation_inbound_memberships_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE
);

CREATE INDEX federation_inbound_memberships_member_idx ON public.federation_inbound_memberships USING btree (peer_id, member_uri);

COMMENT ON TABLE public.federation_inbound_memberships IS 'Origin assets a peer''s container (collection) holds, as that peer announced them (#1571 schema; written only by authenticated membership ingest, #1574). Empty until then: a container grant authorises a file only through a recorded membership.';

-- +goose Down

DROP TABLE IF EXISTS public.federation_inbound_memberships;
DROP TABLE IF EXISTS public.federation_inbound_tombstones;
DROP TABLE IF EXISTS public.federation_inbound_grants;
