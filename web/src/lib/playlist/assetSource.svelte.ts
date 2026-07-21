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
}

function toItem(a: AssetForPlaylist): PlaylistItem {
  const asset: ViewAsset = {
    id: a.id,
    title: a.title ?? '',
    file_extension: a.file_extension ?? null,
    file_hash: a.file_hash ?? null,
    asset_type: a.asset_type ?? null,
    metadata: a.metadata ?? null,
  };
  return { id: a.id, asset };
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
  });

  const aux = $state<{ asset: AssetForPlaylist | null }>({
    asset: seed ?? null,
  });

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
