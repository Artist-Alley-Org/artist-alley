// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Single asset → AssetPlaylist source adapter (#475, ADR 0067).
//
// The standalone /assets/[id] route is a "playlist of 1": one asset,
// no sibling context (prev/next through a collection is a v0.6.0
// follow-up via CollectionHost — ADR 0067). This is the asset-shaped
// twin of createPostPlaylistSource: fetch the asset, map it to the
// single PlaylistItem the AssetPlaylist shell renders.
//
// It accepts an optional seed so the route's +page.ts load (which has
// already fetched + 404-gated the asset) can hand the data straight in
// without a second round-trip; reload() re-fetches on demand.

import { api } from '$api/client';
import { t } from '$stores/lang.svelte';
import type { PlaylistSource, PlaylistItem } from './types';
import type { ViewAsset } from '$components/viewers/controller';

// The subset of the API Asset shape the viewer needs. Local, like
// PostForPlaylist, to avoid importing the full generated type for four
// fields.
export interface AssetForPlaylist {
  id: string;
  title?: string | null;
  file_extension?: string | null;
  file_hash?: string | null;
  asset_type?: number | null;
  metadata?: Record<string, unknown> | null;
  preview_available?: boolean;
  /** #899 — set when the caller may not see this asset's columns.
   *  Every field above is then absent from the payload. */
  restricted?: boolean;
  owner_display_name?: string | null;
  /** #981 — the owner's ref, present on a readable payload only. See
   *  ViewAsset.owner_user_ref: it is what the delete affordance is
   *  gated on, and it is deliberately absent from a withheld one. */
  owner_user_ref?: number | null;
}

function toItem(a: AssetForPlaylist): PlaylistItem {
  const asset: ViewAsset = {
    id: a.id,
    title: a.title ?? '',
    file_extension: a.file_extension ?? null,
    file_hash: a.file_hash ?? null,
    asset_type: a.asset_type ?? null,
    metadata: a.metadata ?? null,
    preview_available: a.preview_available ?? false,
    restricted: !!a.restricted,
    owner_display_name: a.owner_display_name ?? null,
    // #981 — carried so a viewer host can decide whether to offer the
    // delete affordance. Null on a withheld payload by construction:
    // the placeholder's allow-list is the owner's NAME, not their ref.
    owner_user_ref: a.owner_user_ref ?? null,
  };
  // #899 — the standalone route reaches the SAME restricted plate the
  // post route has shown since #883. Threading these two through here is
  // what stops /assets/{id} being the surface where a withheld asset
  // renders as a broken viewer instead of a placeholder.
  return {
    id: a.id,
    asset,
    restricted: !!a.restricted,
    ownerDisplayName: a.owner_display_name ?? null,
  };
}

/** Reactive single-asset playlist source. Same factory-over-a-$state
 *  object shape as createPostPlaylistSource — see that file for the
 *  rationale on why a factory rather than a class. */
export function createAssetPlaylistSource(assetId: string, seed?: AssetForPlaylist) {
  const state = $state<PlaylistSource>({
    kind: 'single',
    id: assetId,
    title: seed?.title || '',
    items: seed ? [toItem(seed)] : [],
    cursor: 0,
    loading: !seed,
    error: null,
    removeItem,
  });

  const aux = $state<{ asset: AssetForPlaylist | null }>({
    asset: seed ?? null,
  });

  /** PlaylistSource.removeItem — see types.ts for why the source owns
   *  this instead of the shell splicing the array itself.
   *
   *  For a playlist of 1 this always empties the list, and the shell
   *  reads the 0 and closes. Written generically anyway: the value of
   *  the contract is that both sources answer the same question the
   *  same way, and a special case here is a place for them to diverge. */
  function removeItem(itemId: string): number {
    const idx = state.items.findIndex((i) => i.id === itemId);
    if (idx >= 0) {
      state.items.splice(idx, 1);
      // Clamp: dropping the item under the cursor would otherwise leave
      // it one past the end, and the shell would render nothing at all.
      if (state.cursor > state.items.length - 1) {
        state.cursor = Math.max(0, state.items.length - 1);
      }
    }
    return state.items.length;
  }

  let generation = 0;
  let currentAssetId = assetId;

  async function load() {
    const gen = ++generation;
    // Atomic swap: keep the previous item visible while re-fetching, so
    // the viewer never tears down. Only flip loading on a cold start.
    if (state.items.length === 0) state.loading = true;
    state.error = null;
    try {
      const { data, error: apiErr } = await api.GET('/assets/{id}', {
        params: { path: { id: currentAssetId } },
      });
      if (gen !== generation) return; // stale — newer fetch in flight
      if (apiErr || !data) {
        throw new Error(
          (apiErr as { error?: string } | undefined)?.error ?? t('common.failed_to_load'),
        );
      }
      const asset = data as AssetForPlaylist;
      aux.asset = asset;
      state.id = asset.id;
      state.title = asset.title || t('common.untitled');
      state.items = [toItem(asset)];
      state.cursor = 0;
    } catch (e) {
      if (gen !== generation) return;
      state.error = e instanceof Error ? e.message : t('common.failed_to_load');
    } finally {
      if (gen === generation) state.loading = false;
    }
  }

  /** Re-target at a different asset without recreating the factory. */
  function setAssetId(nextAssetId: string) {
    if (nextAssetId === currentAssetId && state.items.length > 0) return;
    currentAssetId = nextAssetId;
    void load();
  }

  // Fetch only if we weren't seeded — the route load already has the
  // data on first paint.
  if (!seed) void load();

  return {
    source: state,
    aux,
    reload: load,
    setAssetId,
  };
}
