// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1243 — THE AI MARKER, IN EVERY DENSITY, AND THE TWO STATES THAT
// MUST NEVER PRODUCE ONE.
//
// # ⛔ Why the mode loop is the point of this file
//
// `ViewMode` is `grid | masonry | thumbnail | list | feed`, the default
// is **grid**, and the card chrome differs per mode to the point of
// being a different component in one of them:
//
//	grid       PostCard — the #1111 overlay; NO band, NO metadata stack
//	masonry    PostCard — the overlay when wide, NOTHING when compact
//	thumbnail  PostCard — `thumb-band-top` + the metadata stack
//	feed       PostCard — the bottom-right corner badge
//	list       PostListTable — a table; no card exists at all
//
// ADR 0094's amendment measured a THUMBNAIL tile and named "the band's
// currently-empty middle". Implemented literally that answers one mode
// of five and not the one users land on, so every mode is asserted here
// and the two that deliberately draw nothing say why.
//
// # ⛔ The prohibition, pinned as an ABSENCE OF TEXT and not just of a badge
//
// `none` and `null` are different facts and NEITHER is ever shown —
// including in a tooltip or an accessible name. Asserting "no badge
// element" is the weak half: it passes on a card that renders the word
// "No AI" as a plain `<span>`. So the negative cases scan the ENTIRE
// rendered card — text, every attribute value — for any of the strings
// the `none` state could produce, which is the assertion that would
// have caught a `NoAI` badge added later by someone reading the enum
// and not the ADR.
//
// # ⭐ assisted ≠ generated is pinned BOTH WAYS
//
// "generated draws Bot" passes on a component that always draws Bot.
// Every glyph and every accessible name is therefore asserted against
// the OTHER state as well.

import { fireEvent, render } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import PostCard from './PostCard.svelte';
import AssetCard from './AssetCard.svelte';
import PostListTable from './PostListTable.svelte';
import { cardTooltip } from '$stores/cardTooltip.svelte';
import type { CardAsset } from './cardAsset';
import type { ViewMode } from '$stores/browseView.svelte';

const ASSET_ID = '3f1b8e2c-0000-4000-8000-0000000000f1';
const POST_ID = '3f1b8e2c-0000-4000-8000-0000000000f2';

/** Every string a `none`/`null` state could plausibly leak. If a future
 *  pass adds a "NoAI" affordance, it fails here rather than in review. */
const FORBIDDEN = ['no ai', 'noai', 'not ai', 'no generative', 'human-made', 'human made'];

function asset(overrides: Partial<CardAsset> = {}): CardAsset {
  return {
    id: ASSET_ID,
    title: 'Stocking render',
    asset_type: 1,
    created_at: '2026-08-01T12:00:00.000Z',
    file_hash: 'c'.repeat(64),
    file_extension: 'png',
    thumbhash: null,
    preview_available: true,
    ladder_available: true,
    scrub_available: false,
    pixel_width: 1200,
    pixel_height: 1600,
    ...overrides,
  };
}

function post(ai: string | null) {
  return {
    id: POST_ID,
    title: 'A single picture',
    created_at: '2026-08-01T12:00:00.000Z',
    posted_at: '2026-08-01T12:00:00.000Z',
    author_user_ref: 1,
    description: '',
    visibility: 'public',
    tags: [],
    updated_at: '2026-08-01T12:00:00.000Z',
    like_count: 4,
    comment_count: 2,
    ai_provenance: ai,
    members: [{ asset_id: ASSET_ID, sort_order: 0, asset: asset() }],
  };
}

const postCard = (mode: ViewMode, ai: string | null) =>
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  render(PostCard, { post: post(ai) as any, mode, feed: mode === 'feed' }).container;

const assetCard = (mode: ViewMode, ai: string | null) =>
  render(AssetCard, { asset: asset({ ai_provenance: ai }), mode }).container;

const listTable = (ai: string | null) =>
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  render(PostListTable, { items: [post(ai)] as any }).container;

const mark = (c: HTMLElement) =>
  c.querySelector<HTMLElement>('[data-testid="card-ai-provenance"]');

/** Everything a reader could possibly perceive: rendered text plus every
 *  attribute value in the subtree. */
function allProse(c: HTMLElement): string {
  const parts: string[] = [c.textContent ?? ''];
  for (const el of c.querySelectorAll('*')) {
    for (const a of el.attributes) parts.push(a.value);
  }
  return parts.join(' ').toLowerCase();
}

function expectSaysNothingAboutAi(c: HTMLElement, why: string) {
  expect(mark(c), why).toBeNull();
  const prose = allProse(c);
  for (const bad of FORBIDDEN) {
    expect(prose, `${why}: rendered "${bad}"`).not.toContain(bad);
  }
}

/** The tooltip store is FINE-POINTER-ONLY (`hover: hover` + `pointer:
 *  fine`), and vitest-setup stubs `matchMedia` to answer `false` for
 *  everything — so without this the store silently no-ops and both
 *  masonry cases below would pass on an empty payload, which is the
 *  vacuous-green trap in miniature. */
function withFinePointer() {
  const prev = window.matchMedia;
  Object.defineProperty(window, 'matchMedia', {
    writable: true,
    configurable: true,
    value: (query: string) => ({
      matches: query.includes('hover: hover'),
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }),
  });
  return () => {
    Object.defineProperty(window, 'matchMedia', {
      writable: true,
      configurable: true,
      value: prev,
    });
  };
}

/** The element PostCard hangs `tipEnter` on — the stretched card link
 *  inside CardThumb, not the card root. */
const cardLink = (c: HTMLElement) => c.querySelector<HTMLElement>('a.inset-0')!;

afterEach(() => {
  vi.useRealTimers();
  cardTooltip.leave('');
});

// ---------------------------------------------------------------------
// The five modes
// ---------------------------------------------------------------------

describe('#1243 — the marker across all five view modes', () => {
  // grid is the DEFAULT_MODE, so this is the one that decides whether the
  // feature reaches anybody at all.
  for (const mode of ['grid', 'thumbnail', 'feed', 'list'] as const) {
    it(`⭐ marks a generated post in ${mode}`, () => {
      const c = postCard(mode, 'generated');
      const m = mark(c);
      expect(m, `${mode} drew no marker`).toBeTruthy();
      expect(m!.getAttribute('data-ai')).toBe('generated');
      expect(m!.getAttribute('aria-label')).toBe('AI-generated');
    });

    it(`draws NOTHING for an undeclared post in ${mode}`, () => {
      expectSaysNothingAboutAi(postCard(mode, null), `${mode}, undeclared`);
    });

    it(`draws NOTHING for a post declared \`none\` in ${mode}`, () => {
      expectSaysNothingAboutAi(postCard(mode, 'none'), `${mode}, declared none`);
    });
  }

  // `list` above is PostCard's list FALLBACK (#1137) — the tiles a
  // surface gets when it supplies no table snippet. The browse page's
  // real list mode renders PostListTable, a different component with no
  // card chrome at all, so it needs its own case or the fifth mode is
  // asserted against something the reader never sees.
  it('⭐⭐ marks a generated post in the real LIST view (PostListTable)', () => {
    const c = listTable('generated');
    const m = mark(c);
    expect(m, 'the list table drew no marker').toBeTruthy();
    expect(m!.getAttribute('data-ai')).toBe('generated');
    expect(m!.getAttribute('aria-label')).toBe('AI-generated');
    // Beside the title, which is where the other four densities put it.
    expect(m!.parentElement!.textContent).toContain('A single picture');
  });

  it('the list table draws NOTHING for `none` or undeclared', () => {
    expectSaysNothingAboutAi(listTable(null), 'list table, undeclared');
    expectSaysNothingAboutAi(listTable('none'), 'list table, declared none');
  });

  // ⚠️ MASONRY IS THE ONE DENSITY THAT CANNOT CARRY A BADGE, and this
  // case exists so its absence reads as a decision rather than a gap.
  // jsdom/happy-dom compute no layout, so an unmeasured tile is always
  // the minimal tier = `compact`, where #652 suppresses the kind badge
  // too (on a 60px tile it collides with the ⋮ menu). The facts live in
  // that density's hover tooltip instead — so the assertion is that the
  // TOOLTIP carries it, not that nothing happened.
  it('⭐ compact masonry carries the declaration in its TOOLTIP, not as a badge', async () => {
    const restore = withFinePointer();
    vi.useFakeTimers();
    const c = postCard('masonry', 'generated');
    expect(mark(c), 'a 60px tile has no room for a badge').toBeNull();

    await fireEvent.mouseEnter(cardLink(c), { clientX: 10, clientY: 10 });
    vi.advanceTimersByTime(1000);
    // ⚠️ FIRST, not merely present. CardTooltip renders `meta` as ONE
    // `truncate` line inside an 18rem box — measured in a real browser,
    // the line read "PNG · 1024 × 1024 · 4 assets in this pos…" with the
    // declaration appended at the end, so `toContain` passed while
    // nothing appeared on screen. Position is the assertion.
    expect(cardTooltip.meta[0]).toBe('AI-generated');
    restore();
  });

  it('⭐ ...and its tooltip says NOTHING for `none` or undeclared', async () => {
    for (const value of [null, 'none']) {
      const restore = withFinePointer();
      vi.useFakeTimers();
      const c = postCard('masonry', value);
      await fireEvent.mouseEnter(cardLink(c), { clientX: 10, clientY: 10 });
      vi.advanceTimersByTime(1000);
      // Non-empty first: an empty payload passes every `not.toContain`
      // below and proves nothing at all.
      expect(cardTooltip.meta.length, 'the tooltip did not commit').toBeGreaterThan(0);
      const joined = cardTooltip.meta.join(' ').toLowerCase();
      for (const bad of [...FORBIDDEN, 'ai-generated', 'ai-assisted']) {
        expect(joined, `masonry tooltip, ${String(value)}: said "${bad}"`).not.toContain(bad);
      }
      vi.useRealTimers();
      restore();
    }
  });
});

// ---------------------------------------------------------------------
// ⭐ assisted ≠ generated
// ---------------------------------------------------------------------

describe('#1243 — the two marked states are distinguishable', () => {
  it('⭐ different GLYPH, both ways round', () => {
    // `data-ai` is what the component chose; the glyph follows from it
    // in one place. Asserting the lucide markup would couple this file
    // to the icon package's internals, which is not its business —
    // asserting the CHOICE is (same reasoning as CardKindBadge's
    // `data-glyph`).
    const gen = mark(postCard('thumbnail', 'generated'))!;
    const asst = mark(postCard('thumbnail', 'assisted'))!;
    expect(gen.getAttribute('data-ai')).toBe('generated');
    expect(asst.getAttribute('data-ai')).toBe('assisted');
    expect(gen.getAttribute('data-ai')).not.toBe(asst.getAttribute('data-ai'));
    // ...and the drawn SVG differs, or `data-ai` would be a label on two
    // identical pictures. Bot and Sparkles have different path data.
    expect(gen.querySelector('svg')!.innerHTML).not.toBe(
      asst.querySelector('svg')!.innerHTML,
    );
  });

  it('⭐ different ACCESSIBLE NAME, both ways round', () => {
    // The screen-reader half. A shared marker with a shared name undoes
    // the reason the enum keeps three states.
    expect(mark(postCard('grid', 'generated'))!.getAttribute('aria-label')).toBe('AI-generated');
    expect(mark(postCard('grid', 'assisted'))!.getAttribute('aria-label')).toBe('AI-assisted');
  });

  it('the icon itself is hidden from the accessibility tree', () => {
    // The name is on the control; the glyph is decoration. Both being
    // announced would read the state twice.
    const m = mark(postCard('thumbnail', 'generated'))!;
    expect(m.querySelector('svg')!.getAttribute('aria-hidden')).toBe('true');
  });
});

// ---------------------------------------------------------------------
// The asset wall — the OTHER card, on its own declaration
// ---------------------------------------------------------------------

describe('#1243 — AssetCard marks the asset\'s OWN declaration', () => {
  for (const mode of ['grid', 'thumbnail', 'feed', 'list'] as const) {
    it(`marks a generated asset in ${mode}`, () => {
      const m = mark(assetCard(mode, 'generated'));
      expect(m, `${mode} drew no marker`).toBeTruthy();
      expect(m!.getAttribute('aria-label')).toBe('AI-generated');
    });
  }

  it('draws NOTHING for `none` or undeclared', () => {
    expectSaysNothingAboutAi(assetCard('thumbnail', null), 'asset card, undeclared');
    expectSaysNothingAboutAi(assetCard('thumbnail', 'none'), 'asset card, declared none');
  });
});

// ---------------------------------------------------------------------
// The band's shape — ADR 0094's named slot, and the chrome it must not
// disturb (CardChrome.placement.test.ts holds the rest)
// ---------------------------------------------------------------------

describe('#1243 — where the thumbnail band puts it', () => {
  const band = (c: HTMLElement) => c.querySelector<HTMLElement>('[data-testid="thumb-band-top"]')!;

  it('sits in the band, AFTER the kind badge', () => {
    const c = postCard('thumbnail', 'generated');
    const badge = band(c).querySelector('[data-testid^="card-kind"]')!;
    const m = mark(c)!;
    expect(band(c).contains(m)).toBe(true);
    expect(badge.compareDocumentPosition(m) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('⭐ is NOT inside the gapless kind+extension unit', () => {
    // That pair is one fact read as one thing (#1171). The declaration
    // is a second, unrelated fact; folding it in would make the band say
    // "png-and-AI" as a single reading, and would put a third element
    // inside a container whose whole purpose is that it has no gap.
    const c = postCard('thumbnail', 'generated');
    const badge = band(c).querySelector('[data-testid^="card-kind"]')!;
    expect(mark(c)!.parentElement).not.toBe(badge.parentElement);
  });

  it('the kind badge is STILL the band\'s first child', () => {
    // The constraint CardChrome.placement.test.ts holds, restated here
    // because this is the change that could have broken it.
    const c = postCard('thumbnail', 'generated');
    const badge = band(c).querySelector('[data-testid^="card-kind"]')!;
    expect(band(c).firstElementChild!.contains(badge)).toBe(true);
  });
});
