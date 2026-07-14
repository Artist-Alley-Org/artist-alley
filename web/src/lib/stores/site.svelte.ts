// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Site identity store — the operator-configured display name of this
// install ("Artist Alley" by default). Rendered in the navbar wordmark,
// the login card, and document <title> suffixes.
//
// The name rides the public /appearance boot fetch (see the appearance
// store, which calls `site.setName` on refresh) so it's available for
// anonymous visitors before sign-in and without a second request. A
// localStorage cache gives the correct name on first paint after a
// refresh; the fetch reconciles it. It is edited via Admin → System →
// Site (/admin/system/site), never here — this store is read-only from
// the frontend's perspective.

import { browser } from '$app/environment';

/** Product name, two words, no hyphen. Mirrors the Go-side
 *  sysconfig.DefaultSiteName fallback. */
export const DEFAULT_SITE_NAME = 'Artist Alley';

const STORAGE_KEY = 'aa_site_name';

function readCache(): string {
  if (!browser) return DEFAULT_SITE_NAME;
  try {
    return localStorage.getItem(STORAGE_KEY) || DEFAULT_SITE_NAME;
  } catch {
    return DEFAULT_SITE_NAME;
  }
}

class SiteState {
  name = $state(readCache());

  /** Called by the appearance store after the public boot fetch. A
   *  blank/absent value keeps the default rather than clearing it. */
  setName(next: string | null | undefined): void {
    const value = (next ?? '').trim() || DEFAULT_SITE_NAME;
    this.name = value;
    if (browser) {
      try {
        localStorage.setItem(STORAGE_KEY, value);
      } catch {
        // localStorage disabled / quota'd — ignore; in-memory value stands.
      }
    }
  }
}

export const site = new SiteState();
