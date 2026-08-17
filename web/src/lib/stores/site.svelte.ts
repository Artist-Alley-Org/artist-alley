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

  /** True when this install runs in demo mode (env AA_DEMO_MODE). Rides
   *  the same public /appearance boot fetch as the name. Drives the
   *  login-card demo-credential hint and the read-only banner. Not
   *  cached — it defaults to false until the boot fetch confirms it, so
   *  a normal install never briefly flashes demo chrome. */
  demoMode = $state(false);

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

  /** Called by the appearance store after the public boot fetch. */
  setDemoMode(next: boolean | null | undefined): void {
    this.demoMode = next === true;
  }

  /** True when this install can answer a reverse-image search (#1163).
   *  Rides the same public /appearance boot fetch as the name and demo
   *  flag, and is the RESOLVED capability rather than the config knob —
   *  an install whose CLIP sidecar failed to start is `false` here even
   *  with `search.visual.enabled` on, because the endpoint would answer
   *  501 either way.
   *
   *  Uncached and defaulting to FALSE, the same rule demoMode follows
   *  and for the same reason: a surface that appears and then vanishes
   *  is worse than one that arrives a beat late, and the install this
   *  flag exists for is the one WITHOUT the channel. The by-image
   *  component keeps its 501 handling regardless — this hides the arm,
   *  it does not become the only thing standing between a click and an
   *  error. */
  visualSearchEnabled = $state(false);

  /** Called by the appearance store after the public boot fetch. */
  setVisualSearchEnabled(next: boolean | null | undefined): void {
    this.visualSearchEnabled = next === true;
  }

  /** True when this install lets anonymous visitors browse (#1195 —
   *  `public_mode`, the #445/#709 switch). Rides the same public
   *  /appearance boot fetch as the flags above.
   *
   *  It is here rather than read from the public-mode endpoint because
   *  that endpoint is gated on `system.config.read`: the person who
   *  needs the answer is a curator choosing a visibility tier, who has
   *  no admin capability and should not need one to be told whether
   *  "Public" means anything on this instance.
   *
   *  Uncached and defaulting to FALSE, the same rule the two flags
   *  above follow. The consequence is worth stating plainly, because it
   *  is the opposite of the usual one: defaulting false HIDES a
   *  control, so on a public install the Public option arrives a beat
   *  after the modal opens. That is the right way round — a tier option
   *  that appears and then vanishes would let a curator click a control
   *  that is about to stop existing, and the visibility of somebody's
   *  work is not a good place for a flicker.
   *
   *  It gates an OPTION, never an outcome. A collection already at
   *  `public` keeps that tier when the flag is false; the modal says so
   *  rather than quietly presenting it as something else. */
  publicModeEnabled = $state(false);

  /** Called by the appearance store after the public boot fetch. */
  setPublicModeEnabled(next: boolean | null | undefined): void {
    this.publicModeEnabled = next === true;
  }
}

export const site = new SiteState();
