// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// AssetPlaylist source contract.
//
// An AssetPlaylist is the generic shell that hosts a sequence of
// assets and feeds them one-at-a-time to the AssetViewer. The shell
// is source-agnostic; it doesn't care whether the assets came from a
// post, a collection, a search, a review session, or a single-asset
// link. Hosts (PostHost, CollectionHost, etc.) build a PlaylistSource
// and hand it to the shell.
//
// Why this split? Quake's lesson: design the network protocol from
// day one. Here the "protocol" is PlaylistSource — every viewing
// surface in the app speaks it, so a future "open this collection in
// review mode" or "broadcast this playlist to the presentation room"
// is just a new source adapter, not a new viewer.

import type { Snippet } from 'svelte';
import type { ViewAsset } from '$components/viewers/controller';

/** Kinds of playlist sources the shell knows about. Mostly used for
    telemetry / URL state / "what kind of playlist am I in" UI hints;
    the shell itself treats all sources the same. */
export type SourceKind =
  | 'post'        // PostHost — post.members as the playlist
  | 'collection'  // CollectionHost — a collection's members as the playlist
  | 'review'      // ReviewHost — curated subset of any other source
  | 'search'      // SearchHost — search results as a playlist
  | 'gallery'     // UserHost — a user's uploads as a playlist
  | 'companions'  // CompanionHost — a 3D model's sidecars as a playlist
  | 'single';     // StandaloneAssetHost — playlist of 1, no nav chrome

/** A single position in a playlist. Keep this minimal: just enough
    for the shell to render the filmstrip + feed the viewer. Hosts
    that need richer per-item data (sort_order, added_by, version)
    read it out of their own source-of-truth, not the playlist item. */
export interface PlaylistItem {
  /** Stable per-item identifier. For most sources this is the
      asset_id; for sources that can carry the same asset more than
      once (a "favourites" playlist with dupes) it's an envelope id. */
  id: string;
  /** Asset shape the AssetViewer accepts. Pre-resolved so cursor
      changes don't pay an HTTP round-trip per nav. */
  asset: ViewAsset;
  /** The viewer may not see this member (#883). The server sent a
      placeholder — no title, no file hash, no metadata — so `asset`
      carries only the id, and the shell shows the restricted plate
      instead of mounting a view body. The item stays in the playlist on
      purpose: dropping it would renumber every later position and hide
      that a restriction exists at all. */
  restricted?: boolean;
  /** The owner's display name, the only asset-derived value a restricted
      placeholder carries. Null when the server could not resolve one. */
  ownerDisplayName?: string | null;
}

/** Reactive source the AssetPlaylist shell binds to.
 *
 *  Hosts construct one of these via a factory (createPostPlaylistSource,
 *  createCollectionPlaylistSource, etc.) and pass it as the shell's
 *  `source` prop. Items + cursor + loading are all reactive — Svelte 5
 *  `$state` runes inside the factory keep the shell in sync as the
 *  source's underlying data fetches resolve.
 */
export interface PlaylistSource {
  /** Categorical kind. Set at construction; doesn't change. */
  readonly kind: SourceKind;
  /** Current target id within the kind — usually the post/collection
      /etc id. For 'single' this is the asset id. For 'search' it's a
      stable hash of the query.

      MUTABLE — sources that support in-place re-target (host calls
      `setPostId(next)` / `setQuery(q)` etc.) update this when the
      target changes so consumers of `source.id` (URL state, telemetry,
      cache keys) see the current target without recreating the
      source instance. */
  id: string;
  /** Human-readable label the shell shows in the toolbar — "Wall Paint
      Gate", "Project Echo collection", "Review session #1", etc. */
  title: string;
  /** Reactive item array. Empty + loading=true means "still
      fetching"; empty + loading=false means "no items in this
      source" (the shell shows a friendly empty state). */
  items: PlaylistItem[];
  /** Current cursor position. Bindable from the shell — the shell
      mutates this when the user navigates. Hosts that want to
      persist position (URL ?pos=, localStorage) react to this. */
  cursor: number;
  /** True while the source is still fetching its initial item set.
      Set false by the source once items is populated (or the fetch
      failed and items is empty). */
  loading: boolean;
  /** Optional error message the shell can surface — e.g. "Post not
      found" or "Collection access denied". */
  error?: string | null;
  /** Optional infinite-scroll hook for search/gallery sources. The
      shell calls this when the cursor approaches items.length-1 so
      navigation can spill into the next page without the user
      hitting an artificial wall. */
  loadMore?: () => Promise<void>;
  /** Drop one item, in place, and leave `cursor` pointing at something
      real. Returns how many items are left, because the shell's very
      next decision is whether there is anything to keep showing.

      A METHOD RATHER THAN THE SHELL SPLICING (#991). The shell used to
      reach in and `source.items.splice(...)` after a delete. It worked,
      and it made Svelte log `ownership_invalid_mutation` on every
      delete: `source` is a prop, the state behind it belongs to the
      host that built it, and a child writing a parent's arrays is
      exactly what that warning is for. Handing the operation to the
      source puts the write back where the state lives, and the two
      implementations stay identical because the shell can only ask.

      Optional: a source whose items cannot be deleted (a fixed review
      set, a search page) simply omits it, and the shell reloads
      instead. */
  removeItem?: (itemId: string) => number;
}

/** Props the host wraps around the AssetPlaylist shell.
 *
 *  contextSlot is the host's sidebar content (author header, post
 *  description, like/comment counts for a post host; collection
 *  description for a collection host; etc.) — the shell threads it
 *  through to AssetViewer's existing metadataSlot prop without
 *  knowing what's inside.
 *
 *  toolbarActions is the host's top-of-viewer action buttons —
 *  Like/Comment/Approve/Reject etc. Optional; the shell omits the
 *  action group if nothing's provided.
 */
export interface AssetPlaylistProps {
  source: PlaylistSource;
  /** Sidebar content threaded into AssetViewer's metadataSlot. */
  contextSlot?: Snippet;
  /** Right-side toolbar actions specific to the host. */
  toolbarActions?: Snippet;
  /** Called when the user closes the playlist (× / ESC / backdrop). */
  onClose: () => void;
  /** True when the playlist is a full-page route (e.g. /posts/[id])
      rather than an overlay over the browse feed. Drives the close
      button affordance — back-arrow vs ×. */
  standalone?: boolean;
}
