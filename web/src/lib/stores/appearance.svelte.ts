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

  /** Initial paint: apply cached picks, then refresh from /appearance. */
  init(): void {
    const cached = readCache();
    this.picks = cached;
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
    } finally {
      this.loaded = true;
    }
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
