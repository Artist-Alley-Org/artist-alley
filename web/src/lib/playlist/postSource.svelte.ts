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

  async function load() {
    state.loading = true;
    state.error = null;
    state.items = [];
    state.cursor = 0;
    aux.post = null;
    try {
      const { data, error: apiErr } = await api.GET('/posts/{id}', {
        params: { path: { id: postId } },
      });
      if (apiErr || !data) {
        throw new Error(
          (apiErr as { error?: string } | undefined)?.error ?? 'Failed to load post',
        );
      }
      const post = data as PostForPlaylist;
      aux.post = post;
      state.title = post.title || 'Untitled';
      state.items = (post.members ?? []).map(
        (m): PlaylistItem => ({
          id: m.asset_id,
          asset: {
            id: m.asset_id,
            title: m.asset?.title ?? '',
            file_extension: m.asset?.file_extension ?? null,
            file_hash: m.asset?.file_hash ?? null,
            metadata: m.asset?.metadata ?? null,
          },
        }),
      );
      // Default cursor to the cover member if pinned, else 0. Same
      // policy PostModal used.
      if (post.cover_asset_id) {
        const idx = state.items.findIndex((it) => it.id === post.cover_asset_id);
        if (idx >= 0) state.cursor = idx;
      }
    } catch (e) {
      state.error = e instanceof Error ? e.message : 'Failed to load';
    } finally {
      state.loading = false;
    }
  }

  // Fire the fetch eagerly. Callers that need to refetch (postId
  // changes — currently rare; could happen if a future "browse to
  // next post" affordance lands) re-instantiate the factory.
  void load();

  return {
    source: state,
    aux,
    reload: load,
  };
}
