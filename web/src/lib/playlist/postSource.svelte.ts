// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Post → AssetPlaylist source adapter.
//
// A Post is, structurally, an AssetPlaylist with social skin:
//   - items[]  = post.members
//   - cursor   = which member is showing
//   - title    = post.title
// Plus a sidebar of author / description / likes / comments / tags
// that the host (PostHost.svelte) provides via the contextSlot prop.
//
// This file is just the wiring: fetch the post, map members to
// PlaylistItem shape, expose the post object so the host can build
// its sidebar. The actual playlist behaviour (navigation, cursor
// persistence, filmstrip render) lives in AssetPlaylist.svelte.

import { api } from '$api/client';
import { t } from '$stores/lang.svelte';
import type { PlaylistSource, PlaylistItem } from './types';

// Subset of the API's Post shape we actually use here. Keeping it
// local avoids importing the full openapi-generated type just to
// read four fields.
export interface PostForPlaylist {
  id: string;
  author_user_ref: number;
  title: string;
  description: string;
  visibility: 'private' | 'followers' | 'public';
  cover_asset_id?: string | null;
  posted_at: string;
  like_count: number;
  comment_count: number;
  tags: string[];
  members: Array<{
    asset_id: string;
    sort_order: number;
    asset: {
      id: string;
      title?: string;
      file_hash?: string | null;
      file_extension?: string | null;
      asset_type?: number | null;
      metadata?: Record<string, unknown> | null;
    };
  }>;
  team_id?: string | null;
  created_at: string;
  updated_at: string;
}

/** Reactive PostPlaylistSource. The state object satisfies the
 *  PlaylistSource contract and exposes the underlying post object
 *  separately so the host can build its sidebar without re-fetching.
 *
 *  Usage in a host:
 *    const src = createPostPlaylistSource(postId);
 *    <AssetPlaylist source={src.source} contextSlot={...}>
 *      {#if src.post}...author header etc...{/if}
 *
 *  Why a factory + reactive state object instead of a class: Svelte 5
 *  `$state` rune semantics work best on plain reactive objects. The
 *  factory closes over the fetch + writes into the same state object
 *  the shell is bound to.
 */
export function createPostPlaylistSource(postId: string) {
  // Reactive state container. The shell binds to `state` directly;
  // we mutate its fields and the shell re-renders.
  const state = $state<PlaylistSource>({
    kind: 'post',
    id: postId,
    title: '',
    items: [],
    cursor: 0,
    loading: true,
    error: null,
  });

  // Side-state the host (PostHost.svelte) needs for its sidebar but
  // the shell doesn't care about. Kept on the factory's return
  // value rather than on the PlaylistSource interface itself.
  const aux = $state<{
    post: PostForPlaylist | null;
  }>({
    post: null,
  });

  // Generation counter for stale-fetch protection. When the user
  // navigates between posts faster than the network resolves (←/→
  // hammered through the feed), each fetch tags itself with `gen`
  // at the start and bails on commit if `gen !== generation` —
  // prevents the older slower fetch from overwriting the newer one.
  let generation = 0;

  // Tracked separately from postId-the-parameter so callers can
  // re-target without recreating the factory. setPostId() updates
  // this and triggers a non-destructive fetch (old items stay on
  // screen until the new data arrives — see load()'s atomic swap).
  let currentPostId = postId;

  async function load() {
    const gen = ++generation;
    // Atomic swap pattern: don't wipe `items` / `aux.post` here.
    // The shell keeps showing the previous post while the fetch
    // resolves, so the AssetViewer + ViewerMenuBar stay mounted and
    // there's no flicker. We only flip `loading` true on the very
    // first load (when there's nothing to show); subsequent
    // navigations between posts are silent from the viewer's POV.
    if (state.items.length === 0) state.loading = true;
    state.error = null;
    try {
      const { data, error: apiErr } = await api.GET('/posts/{id}', {
        params: { path: { id: currentPostId } },
      });
      if (gen !== generation) return; // stale — newer fetch in flight
      if (apiErr || !data) {
        throw new Error(
          (apiErr as { error?: string } | undefined)?.error ?? t('playlist.err_load_post'),
        );
      }
      const post = data as PostForPlaylist;
      // Commit phase — all-or-nothing swap so the shell never sees
      // a half-loaded post.
      aux.post = post;
      state.id = post.id;
      state.title = post.title || t('common.untitled');
      state.items = (post.members ?? []).map(
        (m): PlaylistItem => ({
          id: m.asset_id,
          asset: {
            id: m.asset_id,
            title: m.asset?.title ?? '',
            file_extension: m.asset?.file_extension ?? null,
            file_hash: m.asset?.file_hash ?? null,
            asset_type: m.asset?.asset_type ?? null,
            metadata: m.asset?.metadata ?? null,
          },
        }),
      );
      // Reset cursor on post change — default to the cover member if
      // pinned, else 0. Same policy PostModal used.
      state.cursor = 0;
      if (post.cover_asset_id) {
        const idx = state.items.findIndex((it) => it.id === post.cover_asset_id);
        if (idx >= 0) state.cursor = idx;
      }
    } catch (e) {
      if (gen !== generation) return;
      state.error = e instanceof Error ? e.message : t('common.failed_to_load');
    } finally {
      if (gen === generation) state.loading = false;
    }
  }

  /** Re-target the source at a different post. Triggers a fresh
   *  fetch but keeps the previous post's items + sidebar data visible
   *  until the new data arrives — the shell never tears down its
   *  viewer, so menu bar / dialog chrome stay static. Used by hosts
   *  that swap posts in-place (browse-feed sibling navigation). */
  function setPostId(nextPostId: string) {
    if (nextPostId === currentPostId && state.items.length > 0) return;
    currentPostId = nextPostId;
    void load();
  }

  // Fire the first fetch eagerly.
  void load();

  return {
    source: state,
    aux,
    reload: load,
    setPostId,
  };
}
