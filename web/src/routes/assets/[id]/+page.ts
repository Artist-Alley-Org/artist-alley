// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Standalone asset page load (#475, ADR 0067).
//
// Fetches the asset up front so a missing / forbidden one renders the
// normal SvelteKit error page rather than an in-shell error state. The
// visibility predicate already returns 404 for an asset the caller may
// not see (ADR 0064), so 404 here covers both "gone" and "not yours" —
// which is the point: the page must not confirm a hidden asset exists.
//
// The resolved asset is handed to +page.svelte so the single-asset
// playlist source can seed from it without a second round-trip.

import { error } from '@sveltejs/kit';
import { api } from '$api/client';
import type { AssetForPlaylist } from '$lib/playlist/assetSource.svelte';

// Dynamic id — opt out of the layout's prerender. adapter-static serves
// this via the SPA fallback and the load runs client-side (ssr=false
// inherited from the layout).
export const prerender = false;

export async function load({ params, fetch }): Promise<{ asset: AssetForPlaylist }> {
  const { data, error: apiErr, response } = await api.GET('/assets/{id}', {
    params: { path: { id: params.id } },
    fetch,
  });
  if (apiErr || !data) {
    // 404 for a genuinely-missing asset AND for one the predicate hid;
    // any non-2xx collapses to "not found" so existence is never
    // confirmed. A 401 (session expired mid-navigation) also lands
    // here as 404 rather than leaking that the id is real.
    throw error(404, response?.status === 404 ? 'Asset not found' : 'Asset unavailable');
  }
  return { asset: data as AssetForPlaylist };
}
