// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Asset edit page load (#549).
//
// Same 404 gate as the sibling detail load, for the same reason: the
// visibility predicate already answers 404 for an asset the caller may
// not SEE (ADR 0064), so collapsing every non-2xx to 404 keeps this
// route from confirming a hidden asset exists. Whether the caller may
// MUTATE what they can see is a separate question, answered on the page
// itself — a reader who may not edit gets a page that says so, not a
// 404 that pretends the asset is gone.
//
// The full Asset row is fetched here (not the narrower playlist shape)
// because the form needs `updated_at` for the optimistic-concurrency
// baseline, `owner_user_ref`/`team_id` for the mutation gate, and
// `status` to know which transitions are transitions at all.

import { error } from '@sveltejs/kit';
import { api } from '$api/client';
import type { components } from '$api/schema';

export type EditableAsset = components['schemas']['Asset'];

// Dynamic id — opt out of the layout's prerender, same as the sibling.
export const prerender = false;

export async function load({ params, fetch }): Promise<{ asset: EditableAsset }> {
  const { data, error: apiErr, response } = await api.GET('/assets/{id}', {
    params: { path: { id: params.id } },
    fetch,
  });
  if (apiErr || !data) {
    throw error(404, response?.status === 404 ? 'Asset not found' : 'Asset unavailable');
  }
  return { asset: data as EditableAsset };
}
