// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// "Where is this file used" — the owner's view (#1237, ADR 0091
// decision 5).
//
// Loaded up front, like the two sibling asset routes, so a caller who
// may not ask gets the ordinary SvelteKit 404 page rather than an
// in-shell error state that has to be designed and then read.
//
// # Why every non-2xx collapses to 404
//
// Because that is what the ENDPOINT does, and this route must not be a
// better oracle than the thing it calls. `GET /assets/{id}/posts`
// answers 404 to a stranger with the SAME body a nonexistent asset id
// gets — not 403 — precisely so it cannot be walked to discover which
// assets exist, or to learn that someone else's file is in use. A route
// that distinguished "no such asset" from "not yours" would hand back
// exactly the distinction the endpoint refuses.
//
// # Why the asset is fetched too
//
// For the file's NAME in the heading, and for the link back to it. It is
// a second request rather than a field on the usage payload because the
// usage endpoint answers about POSTS; adding an asset summary to it
// would be a second place the asset's field-withholding rules have to be
// got right. Anyone this load admits can already read `GET /assets/{id}`
// — the usage gate (owner / assets.admin / system.admin) is strictly
// tighter — so the second call cannot fail on its own where the first
// succeeded, except by a race, which lands on the same 404.

import { error } from '@sveltejs/kit';
import { api } from '$api/client';
import type { components } from '$api/schema';

export type AssetPostUsage = components['schemas']['AssetPostUsage'];
export type UsageAsset = components['schemas']['Asset'];

// Dynamic id — opt out of the layout's prerender, same as the siblings.
export const prerender = false;

export async function load({
  params,
  fetch,
}): Promise<{ asset: UsageAsset; usage: AssetPostUsage }> {
  const [usageRes, assetRes] = await Promise.all([
    api.GET('/assets/{id}/posts', { params: { path: { id: params.id } }, fetch }),
    api.GET('/assets/{id}', { params: { path: { id: params.id } }, fetch }),
  ]);

  if (usageRes.error || !usageRes.data) {
    throw error(404, 'Asset not found');
  }
  if (assetRes.error || !assetRes.data) {
    throw error(404, 'Asset not found');
  }

  return {
    asset: assetRes.data as UsageAsset,
    usage: usageRes.data as AssetPostUsage,
  };
}
