// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Client-side cover cache for collection cards.
//
// The hub renders dozens of CollectionCards at once. Each wants the
// first few assets of its collection to compose a mosaic cover —
// without this cache that would be 1 HTTP request per card, every
// time the user toggles a tab.
//
// We memoise the first-page resources call per collection id with a
// short TTL (60 s). Invalidate when membership changes by calling
// `invalidate(id)` from the add/remove paths.
//
// This is deliberately not the SWR/TanStack-Query pattern — too much
// machinery for a single read shape. A flat Map<id, entry> is enough
// until we have a second cached endpoint.

import { api } from '$api/client';

interface CoverAsset {
  id: string;
  file_hash: string | null;
}

interface Entry {
  assets: CoverAsset[];
  fetchedAt: number;
  pending?: Promise<CoverAsset[]>;
}

const TTL_MS = 60_000;
const MAX_COVERS = 4;

const store = new Map<string, Entry>();

export function get(id: string): CoverAsset[] | null {
  const entry = store.get(id);
  if (!entry) return null;
  if (Date.now() - entry.fetchedAt > TTL_MS) return null;
  return entry.assets;
}

export async function fetchCovers(id: string): Promise<CoverAsset[]> {
  const hit = get(id);
  if (hit) return hit;
  // Coalesce concurrent fetches for the same id.
  const existing = store.get(id);
  if (existing?.pending) return existing.pending;

  const pending = (async () => {
    const { data, error } = await api.GET('/collections/{id}/resources', {
      params: { path: { id }, query: { limit: MAX_COVERS } },
    });
    if (error || !data) return [];
    const items = (data.items ?? []).slice(0, MAX_COVERS).map((r) => ({
      id: r.asset_id,
      file_hash: r.file_hash ?? null,
    }));
    store.set(id, { assets: items, fetchedAt: Date.now() });
    return items;
  })();

  store.set(id, { assets: [], fetchedAt: 0, pending });
  return pending;
}

export function invalidate(id: string): void {
  store.delete(id);
}

export function clear(): void {
  store.clear();
}
