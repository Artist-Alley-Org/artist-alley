// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Font catalogue — the curated set of fonts an admin can pick from
// per slot at /admin/system/themes.
//
// Each entry resolves to a CSS family value (what we write into the
// --font-* token) plus an optional `loader` callback that registers
// the font with the browser. System fonts have no loader (the OS
// already has them); webfonts pull from a self-hosted woff2 in
// /static/fonts/ once we ship the bundled woff2 set. For phase
// 1.17.A-3 we Google-Fonts-import the brand fonts that need a
// distinctive face (Limelight, Bebas Neue) so the system is wired
// end-to-end; a follow-up commit self-hosts the woff2 to drop the
// CDN dependency.

export type FontSlot = 'brand' | 'display' | 'sans' | 'mono';

export interface FontEntry {
  id: string;
  /** Human label for the picker. */
  label: string;
  /** Slots this font is suitable for. Brand fonts shouldn't be
   *  offered for `sans` because they're display-only. */
  slots: FontSlot[];
  /** CSS font-family value — what ends up in `--font-{slot}`. */
  family: string;
  /** Stylesheet URL appended to <head> once. Omit for system fonts.
   *  Loaders are de-duplicated by URL — picking the same font for
   *  two slots only adds one <link>. */
  stylesheet?: string;
}

// System stacks — defaults for the sans + mono slots. These never
// trigger a network fetch.
const SYSTEM_SANS =
  'ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, "Noto Sans", sans-serif, "Apple Color Emoji", "Segoe UI Emoji"';
const SYSTEM_MONO =
  'ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", "Courier New", monospace';

// Google Fonts CSS endpoint. Listed as the stylesheet for brand /
// display picks until the self-hosted woff2 bundle lands.
function gf(family: string, weights = '400'): string {
  const f = encodeURIComponent(family);
  return `https://fonts.googleapis.com/css2?family=${f}:wght@${weights}&display=swap`;
}

export const FONTS: FontEntry[] = [
  // ── System defaults (no network) ───────────────────────────────
  { id: 'system-sans', label: 'System sans (default)', slots: ['sans', 'display', 'brand'], family: SYSTEM_SANS },
  { id: 'system-mono', label: 'System mono (default)', slots: ['mono'], family: SYSTEM_MONO },

  // ── Brand / display fonts (Google Fonts, TODO: self-host) ──────
  { id: 'limelight',       label: 'Limelight',       slots: ['brand'],             family: '"Limelight", serif',                stylesheet: gf('Limelight') },
  { id: 'bebas-neue',      label: 'Bebas Neue',      slots: ['brand'],             family: '"Bebas Neue", sans-serif',          stylesheet: gf('Bebas+Neue') },
  { id: 'playfair-display', label: 'Playfair Display', slots: ['brand', 'display'], family: '"Playfair Display", serif',         stylesheet: gf('Playfair+Display', '400;600;700') },
  { id: 'cinzel',          label: 'Cinzel',          slots: ['brand', 'display'],  family: '"Cinzel", serif',                   stylesheet: gf('Cinzel', '400;600;700') },
  { id: 'abril-fatface',   label: 'Abril Fatface',   slots: ['brand'],             family: '"Abril Fatface", serif',            stylesheet: gf('Abril+Fatface') },
  { id: 'space-grotesk',   label: 'Space Grotesk',   slots: ['brand', 'display', 'sans'], family: '"Space Grotesk", sans-serif',  stylesheet: gf('Space+Grotesk', '400;500;700') },

  // ── Body / UI sans-serifs ──────────────────────────────────────
  { id: 'inter',           label: 'Inter',           slots: ['sans', 'display'],   family: '"Inter", ui-sans-serif, system-ui, sans-serif', stylesheet: gf('Inter', '400;500;600;700') },
  { id: 'plus-jakarta-sans', label: 'Plus Jakarta Sans', slots: ['sans', 'display'], family: '"Plus Jakarta Sans", ui-sans-serif, system-ui, sans-serif', stylesheet: gf('Plus+Jakarta+Sans', '400;500;700') },
  { id: 'manrope',         label: 'Manrope',         slots: ['sans', 'display'],   family: '"Manrope", ui-sans-serif, system-ui, sans-serif', stylesheet: gf('Manrope', '400;500;700') },
  { id: 'ibm-plex-sans',   label: 'IBM Plex Sans',   slots: ['sans', 'display'],   family: '"IBM Plex Sans", ui-sans-serif, system-ui, sans-serif', stylesheet: gf('IBM+Plex+Sans', '400;500;700') },
  { id: 'source-sans-3',   label: 'Source Sans 3',   slots: ['sans', 'display'],   family: '"Source Sans 3", ui-sans-serif, system-ui, sans-serif', stylesheet: gf('Source+Sans+3', '400;600;700') },

  // ── Monospace ──────────────────────────────────────────────────
  { id: 'jetbrains-mono',  label: 'JetBrains Mono',  slots: ['mono'],              family: '"JetBrains Mono", ui-monospace, monospace', stylesheet: gf('JetBrains+Mono') },
  { id: 'ibm-plex-mono',   label: 'IBM Plex Mono',   slots: ['mono'],              family: '"IBM Plex Mono", ui-monospace, monospace',  stylesheet: gf('IBM+Plex+Mono') },
  { id: 'fira-code',       label: 'Fira Code',       slots: ['mono'],              family: '"Fira Code", ui-monospace, monospace',      stylesheet: gf('Fira+Code') },
];

export const DEFAULT_BY_SLOT: Record<FontSlot, string> = {
  brand:   'limelight',
  display: 'inter',
  sans:    'system-sans',
  mono:    'system-mono',
};

export function fontById(id: string | undefined | null): FontEntry | undefined {
  if (!id) return undefined;
  return FONTS.find((f) => f.id === id);
}

export function fontsForSlot(slot: FontSlot): FontEntry[] {
  return FONTS.filter((f) => f.slots.includes(slot));
}

/**
 * Resolves the four slot IDs (potentially empty) to concrete font
 * entries, with sensible fallbacks.
 */
export function resolveSlots(picks: {
  brand_font?: string | null;
  display_font?: string | null;
  body_font?: string | null;
  mono_font?: string | null;
}): Record<FontSlot, FontEntry> {
  const pick = (slot: FontSlot, id: string | null | undefined): FontEntry => {
    const chosen = fontById(id) ?? fontById(DEFAULT_BY_SLOT[slot]);
    if (chosen && chosen.slots.includes(slot)) return chosen;
    // Last-resort: any font that supports the slot.
    return fontsForSlot(slot)[0]!;
  };
  return {
    brand:   pick('brand',   picks.brand_font),
    display: pick('display', picks.display_font),
    sans:    pick('sans',    picks.body_font),
    mono:    pick('mono',    picks.mono_font),
  };
}
