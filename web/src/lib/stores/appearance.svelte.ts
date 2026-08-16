// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Appearance store — keeps the install-wide font choices in sync with
// the DOM. Reads /appearance at boot (a public endpoint), caches the
// payload to localStorage so the next page load can apply fonts
// before the network round-trip lands, and applies the resolved fonts
// as CSS custom properties on <html>.
//
// CSS variables written:
//   --font-brand    logo / hero
//   --font-display  H1-H3
//   --font-sans     body / UI (also drives Tailwind's default sans)
//   --font-mono     code / tabular
//
// The default body styling in app.css falls back to the system stack
// when none of these vars are set, so the page renders fine before
// the store has booted.

import { browser } from '$app/environment';
import { api } from '$api/client';
import { site } from '$stores/site.svelte';
import {
  DEFAULT_BY_SLOT,
  fontById,
  resolveSlots,
  type FontEntry,
  type FontSlot,
} from '$lib/fonts/catalogue';

const STORAGE_KEY = 'aa_appearance';

export interface AppearancePicks {
  brand_font: string;
  display_font: string;
  body_font: string;
  mono_font: string;
}

const EMPTY: AppearancePicks = {
  brand_font: '',
  display_font: '',
  body_font: '',
  mono_font: '',
};

/**
 * URL of the operator-uploaded instance logo, or '' for "use the
 * shipped default mark" (#517).
 *
 * Cached alongside the fonts for the same reason they are: the mark is
 * in the navbar of every page, so reading it from localStorage on the
 * first paint avoids a visible swap from the default logo to the
 * operator's one after the boot fetch lands.
 */
const LOGO_STORAGE_KEY = 'aa_instance_logo';

function readLogoCache(): string {
  if (!browser) return '';
  try {
    return localStorage.getItem(LOGO_STORAGE_KEY) ?? '';
  } catch {
    return '';
  }
}

function writeLogoCache(url: string): void {
  if (!browser) return;
  try {
    if (url) localStorage.setItem(LOGO_STORAGE_KEY, url);
    else localStorage.removeItem(LOGO_STORAGE_KEY);
  } catch {
    // localStorage may be disabled / quota'd — ignore.
  }
}

function readCache(): AppearancePicks {
  if (!browser) return EMPTY;
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return EMPTY;
    const parsed = JSON.parse(raw) as Partial<AppearancePicks>;
    return { ...EMPTY, ...parsed };
  } catch {
    return EMPTY;
  }
}

function writeCache(picks: AppearancePicks): void {
  if (!browser) return;
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(picks));
  } catch {
    // localStorage may be disabled / quota'd — ignore.
  }
}

/** Track which stylesheet URLs we've already injected. */
const loadedStylesheets = new Set<string>();

function ensureStylesheet(url: string): void {
  if (!browser) return;
  if (loadedStylesheets.has(url)) return;
  // Guard against duplicates across HMR / multiple store inits.
  if (document.querySelector(`link[data-aa-font="${url}"]`)) {
    loadedStylesheets.add(url);
    return;
  }
  const link = document.createElement('link');
  link.rel = 'stylesheet';
  link.href = url;
  link.crossOrigin = 'anonymous';
  link.dataset.aaFont = url;
  document.head.appendChild(link);
  loadedStylesheets.add(url);
}

function applyToDom(picks: AppearancePicks): void {
  if (!browser) return;
  const resolved = resolveSlots(picks);
  const root = document.documentElement;
  (Object.entries(resolved) as Array<[FontSlot, FontEntry]>).forEach(([slot, entry]) => {
    root.style.setProperty(`--font-${slot}`, entry.family);
    if (entry.stylesheet) ensureStylesheet(entry.stylesheet);
  });
}

class AppearanceState {
  picks = $state<AppearancePicks>({ ...EMPTY });
  loaded = $state(false);

  /**
   * URL of the operator's instance logo, or '' when the install uses
   * the shipped default. Consumed by BrandMark, which falls back to
   * /logo.svg — so '' is a complete, valid state, not a missing value.
   */
  logoUrl = $state('');

  /** Initial paint: apply cached picks, then refresh from /appearance. */
  init(): void {
    const cached = readCache();
    this.picks = cached;
    this.logoUrl = readLogoCache();
    applyToDom(cached);
    void this.refresh();
  }

  async refresh(): Promise<void> {
    try {
      const { data } = await api.GET('/appearance');
      if (!data) return;
      const next: AppearancePicks = {
        brand_font: data.brand_font ?? '',
        display_font: data.display_font ?? '',
        body_font: data.body_font ?? '',
        mono_font: data.mono_font ?? '',
      };
      this.picks = next;
      writeCache(next);
      applyToDom(next);
      // The site display name rides this same public boot fetch — hand
      // it to the site store so the wordmark / titles reflect the
      // operator-configured name without a second request.
      site.setName(data.site_name);
      // Demo mode rides the same boot fetch — surface it so the login
      // card and read-only banner can react without a second request.
      site.setDemoMode(data.demo_mode);
      // Whether this install has a reverse-image channel at all (#1163)
      // rides the same fetch, so /search/advanced can omit that section
      // instead of the frontend learning it from a failed upload.
      site.setVisualSearchEnabled(data.visual_search_enabled);
      // Absent logo_url is the shipped-default state, so normalise to
      // '' rather than leaving a stale URL in place — an operator who
      // reverts to the default must actually see it come back.
      this.setLogoUrl(data.logo_url ?? '');
    } finally {
      this.loaded = true;
    }
  }

  /**
   * Record the active instance logo. Called by the boot fetch and by
   * the admin logo card after an upload/select/revert, so the navbar
   * updates without a page reload.
   */
  setLogoUrl(url: string): void {
    this.logoUrl = url;
    writeLogoCache(url);
  }

  /** Apply a preview without persisting — used by the admin picker. */
  preview(next: Partial<AppearancePicks>): void {
    const merged = { ...this.picks, ...next };
    this.picks = merged;
    applyToDom(merged);
  }

  /** Persist via PATCH and broadcast to other tabs via storage event. */
  async save(next: AppearancePicks): Promise<void> {
    const { error } = await api.PATCH('/admin/system/appearance', {
      body: next as never,
    });
    if (error) throw error;
    this.picks = next;
    writeCache(next);
    applyToDom(next);
  }
}

export const appearance = new AppearanceState();

// Convenience exports for the admin picker.
export { fontById, fontsForSlot, FONTS, DEFAULT_BY_SLOT } from '$lib/fonts/catalogue';
