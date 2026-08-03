// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Regression guard for the hover sprite-scrub preview (#595, #835).
//
// The bug this exists to prevent recurring: video and 3D tiles stopped
// showing their hover preview, and it went unnoticed through four
// consecutive card refactors because nothing asserted the behaviour at
// any layer. The eventual cause was NOT in the render path — it was a
// surface feeding the card an object with `file_extension` missing, so
// CardThumb saw an untyped asset and correctly rendered no scrub.
//
// So the guard is deliberately in two parts, because the bug had two
// halves and either can regress alone:
//
//   1. THE CONTRACT — cardAsset.ts makes the presentation fields
//      required, so a surface can no longer drop one silently. That is
//      enforced by svelte-check, not by this file. It is the half that
//      would have caught the actual #595 defect. `scrub_available` is
//      required for the same reason (#835).
//
//   2. THE RENDER PATH — these tests. Given a correctly-fed card, a
//      hover must actually produce an animating sprite layer with the
//      right frame geometry.
//
// WHAT CHANGED IN #835. The scrub is no longer driven by a grid
// hardcoded per file extension; it is driven by the cue list in
// `sprites.vtt`. So these tests now serve a VTT and assert the card
// cycles exactly the cells it declares — including the case the old
// code got wrong, a short clip whose sheet is mostly ffmpeg padding.
// Frame geometry is still asserted as concrete percentages, because
// getting the divisor wrong (cols vs cols-1) yields an animation that
// visibly clips the last frame — a regression a truthy check would
// wave through.

import { render, fireEvent } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import AssetCard from './AssetCard.svelte';
import type { CardAsset } from './cardAsset';
import { _resetSpriteCueCache } from '$lib/util/spriteCues';

const ASSET_ID = '3f1b8e2c-0000-4000-8000-000000000001';

function asset(overrides: Partial<CardAsset> = {}): CardAsset {
  return {
    id: ASSET_ID,
    title: 'Turntable',
    asset_type: 1,
    created_at: '2026-07-01T12:00:00.000Z',
    file_hash: 'a'.repeat(64),
    file_extension: 'glb',
    thumbhash: null,
    preview_available: true,
    ladder_available: true,
    scrub_available: true,
    pixel_width: null,
    pixel_height: null,
    ...overrides,
  };
}

/** Build the VTT the backend writes: `count` cues over a cols-wide grid
 *  of cellW x cellH cells. This is the real output shape — see
 *  preview/video.go writeSpriteSheet. */
function vtt(count: number, cols: number, cellW: number, cellH: number, interval = 0.3): string {
  const stamp = (s: number) => {
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    const sec = s - Math.floor(s / 60) * 60;
    return `${String(h).padStart(2, '0')}:${String(m).padStart(2, '0')}:${sec.toFixed(3).padStart(6, '0')}`;
  };
  let out = 'WEBVTT\n\n';
  for (let i = 0; i < count; i++) {
    const x = (i % cols) * cellW;
    const y = Math.floor(i / cols) * cellH;
    out += `${stamp(i * interval)} --> ${stamp((i + 1) * interval)}\n`;
    out += `sprites.jpg#xywh=${x},${y},${cellW},${cellH}\n\n`;
  }
  return out;
}

/** Serve a cue file for the sprite fetch. `null` = a 404, i.e. an asset
 *  the server has no sheet for. */
function stubCueFetch(body: string | null) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () =>
      body === null
        ? ({ ok: false, status: 404, text: async () => '' } as Response)
        : ({ ok: true, status: 200, text: async () => body } as Response),
    ),
  );
}

/** jsdom's `Image` never fetches, so the card's sheet measurement can
 *  never resolve on its own. Swap in a loader that reports the size the
 *  test names, which is how a real sheet of that shape would behave. */
function stubSpriteSheetSize(naturalWidth: number, naturalHeight: number) {
  class FakeImage {
    naturalWidth = 0;
    naturalHeight = 0;
    onload: (() => void) | null = null;
    #src = '';
    get src() {
      return this.#src;
    }
    set src(v: string) {
      this.#src = v;
      this.naturalWidth = naturalWidth;
      this.naturalHeight = naturalHeight;
      queueMicrotask(() => this.onload?.());
    }
  }
  vi.stubGlobal('Image', FakeImage);
}

/** The sprite layer is the element carrying the sprite sheet as its
 *  background — the same thing a user sees animate. Queried by that
 *  rather than by a class so a styling refactor doesn't silently
 *  neuter the test. */
function spriteLayer(container: HTMLElement): HTMLElement | null {
  return container.querySelector<HTMLElement>('div[style*="sprites.jpg"]');
}

/** Hover is delivered to the stretched navigation link, which is where
 *  the real pointer lands and where the handlers live. */
async function hoverCard(container: HTMLElement) {
  const link = container.querySelector('a[href^="/assets/"]');
  expect(link, 'card should expose a stretched navigation link').toBeTruthy();
  await fireEvent.mouseEnter(link!);
}

async function unhoverCard(container: HTMLElement) {
  const link = container.querySelector('a[href^="/assets/"]');
  await fireEvent.mouseLeave(link!);
}

/** Let the cue fetch, the stubbed image load, and the resulting state
 *  updates all land. The scrub layer is deliberately NOT painted until
 *  both halves have arrived (see CardThumb) — there is no sheet to show
 *  before the sheet is fetched, so waiting costs nothing and avoids
 *  guessing its scale. */
async function flushScrub() {
  for (let i = 0; i < 6; i++) {
    await Promise.resolve();
    await new Promise((r) => queueMicrotask(() => r(null)));
    await tick();
  }
}

beforeEach(() => {
  _resetSpriteCueCache();
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('CardThumb sprite scrub (#595, #835)', () => {
  it('renders no sprite layer at rest', () => {
    stubCueFetch(vtt(36, 6, 240, 240));
    stubSpriteSheetSize(1440, 1440);
    const { container } = render(AssetCard, { asset: asset() });
    expect(spriteLayer(container)).toBeNull();
  });

  it('plays the 6x6 turntable when a 3D tile is hovered', async () => {
    // 36 cues over a 6-wide grid of 240px square cells = the real
    // preview.3d sheet.
    stubCueFetch(vtt(36, 6, 240, 240));
    stubSpriteSheetSize(1440, 1440);

    const { container } = render(AssetCard, { asset: asset({ file_extension: 'glb' }) });
    await hoverCard(container);
    await flushScrub();

    const layer = spriteLayer(container);
    expect(layer, '3D tile should mount a sprite layer on hover').toBeTruthy();
    expect(layer!.style.backgroundImage).toContain(
      `/api/v1/assets/${ASSET_ID}/variants/sprites.jpg`,
    );
    // 6x6 sheet: the sheet is scaled to 600% so one cell fills the box.
    expect(layer!.style.backgroundSize).toBe('600% 600%');
    // Frame 0 sits at the sheet's origin.
    expect(layer!.style.backgroundPosition).toBe('0% 0%');
  });

  it('plays the 10x10 sheet when a video tile is hovered', async () => {
    stubCueFetch(vtt(100, 10, 240, 134));
    stubSpriteSheetSize(2400, 1340);

    const { container } = render(AssetCard, { asset: asset({ file_extension: 'mp4' }) });
    await hoverCard(container);
    await flushScrub();

    const layer = spriteLayer(container);
    expect(layer, 'video tile should mount a sprite layer on hover').toBeTruthy();
    expect(layer!.style.backgroundSize).toBe('1000% 1000%');
  });

  it('scrubs an animated GIF, which no extension list ever licensed (#832)', async () => {
    stubCueFetch(vtt(100, 10, 240, 196));
    stubSpriteSheetSize(2400, 1960);

    const { container } = render(AssetCard, { asset: asset({ file_extension: 'gif' }) });
    await hoverCard(container);
    await flushScrub();

    expect(
      spriteLayer(container),
      'a GIF with a sheet in storage must scrub — the gate is the cue file, not the extension',
    ).toBeTruthy();
  });

  it('cycles ONLY the cells the VTT declares, not the whole grid (#835)', async () => {
    // THE BUG. A ~5s clip fills 25 of 100 cells (the backend floors the
    // cell interval at 0.2s) and ffmpeg's `tile` filter pads the other
    // 75 with black. The old code cycled all 100, so three quarters of
    // the hover preview was blank. The sheet is unchanged — 2400x1340,
    // a full 10x10 grid — and only the cue count says otherwise.
    //
    // Only the interval is faked: the card's cue fetch and sheet
    // measurement are promise-based, and faking microtasks would stall
    // the very state this test needs to arrive.
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    stubCueFetch(vtt(25, 10, 240, 134, 0.2));
    stubSpriteSheetSize(2400, 1340);

    const { container } = render(AssetCard, { asset: asset({ file_extension: 'mp4' }) });
    await hoverCard(container);
    await flushScrub();

    const positions = new Set<string>();
    for (let i = 0; i < 25; i++) {
      positions.add(spriteLayer(container)!.style.backgroundPosition);
      vi.advanceTimersByTime(120);
      await tick();
    }
    // 25 cues over a 10-wide grid = rows 0..2, so cell 24 is at column 4
    // of row 2 and NOTHING may address row 3 or beyond.
    expect(positions.size, 'every declared cue should be visited exactly once').toBe(25);
    for (const p of positions) {
      const yPct = parseFloat(p.split(' ')[1]);
      // Row r sits at r * cellH / (sheetH - cellH) = r * 134 / 1206.
      const row = Math.round(((yPct / 100) * (1340 - 134)) / 134);
      expect(row, `cue at ${p} addresses row ${row}, past the last populated row`).toBeLessThan(3);
    }
    // Having visited all 25 it must wrap to the first, not walk into
    // the padding.
    expect(spriteLayer(container)!.style.backgroundPosition).toBe('0% 0%');
  });

  it('drops the trailing zero-length cue old sheets carry (#835, no re-render)', async () => {
    // The pre-#835 writer ran the full grid and broke on
    // `start >= duration` AFTER emitting that cue, so every short clip's
    // stored VTT ends with a `05.000 --> 05.000` cue pointing at the
    // first padding cell. Every sheet already on an install has this.
    // Dropping it client-side is what makes the fix deployable with no
    // re-render at all.
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    const good = vtt(25, 10, 240, 134, 0.2);
    const degenerate = '00:00:05.000 --> 00:00:05.000\nsprites.jpg#xywh=1200,268,240,134\n\n';
    stubCueFetch(good + degenerate);
    stubSpriteSheetSize(2400, 1340);

    const { container } = render(AssetCard, { asset: asset({ file_extension: 'mp4' }) });
    await hoverCard(container);
    await flushScrub();

    const positions = new Set<string>();
    for (let i = 0; i < 26; i++) {
      positions.add(spriteLayer(container)!.style.backgroundPosition);
      vi.advanceTimersByTime(120);
      await tick();
    }
    expect(positions.size, 'the empty-window cue must not be cycled').toBe(25);
  });

  it('tears the sprite layer down when the pointer leaves', async () => {
    stubCueFetch(vtt(100, 10, 240, 134));
    stubSpriteSheetSize(2400, 1340);

    const { container } = render(AssetCard, { asset: asset({ file_extension: 'mp4' }) });
    await hoverCard(container);
    await flushScrub();
    expect(spriteLayer(container)).toBeTruthy();

    await unhoverCard(container);
    expect(
      spriteLayer(container),
      'leaving the tile must stop the scrub, not leave it running off-screen',
    ).toBeNull();
  });

  it('requests nothing for an asset the server says has no sheet (#471 zero-404s)', async () => {
    // THE GATE IS scrub_available, NOT THE EXTENSION. This is an mp4 —
    // the old code would have cheerfully requested a sheet and 404'd —
    // whose expensive preview.video job has not drained yet, so it has
    // a card (from the cheap poster job, #818) and no scrub.
    stubCueFetch(vtt(100, 10, 240, 134));
    stubSpriteSheetSize(2400, 1340);

    const { container } = render(AssetCard, {
      asset: asset({ file_extension: 'mp4', scrub_available: false }),
    });
    await hoverCard(container);
    await flushScrub();

    expect(spriteLayer(container)).toBeNull();
    expect(fetch, 'no sheet means no request at all').not.toHaveBeenCalled();
  });

  it('leaves still images alone', async () => {
    stubCueFetch(null);
    stubSpriteSheetSize(2400, 1340);

    const { container } = render(AssetCard, {
      asset: asset({ file_extension: 'png', scrub_available: false }),
    });
    await hoverCard(container);
    await flushScrub();
    expect(spriteLayer(container)).toBeNull();
  });

  it('sizes the scrub box to the CELL the cue declares (#761)', async () => {
    // Sprite cells stopped being a fixed 16:9 in #761 — the sheet is
    // fitted to the source, so a portrait clip's cells are portrait. The
    // card used to paint them into a hardcoded `aspect-video` box, which
    // reintroduced the exact squash the backend fix removed, one layer
    // up. Since #835 the ratio comes off the cue rect directly rather
    // than being inferred from the sheet's shape — those agree only
    // while the grid is square.
    //
    // THE RATIO IS THE ASSERTION, in every mode. Which axis it binds is
    // the mode's business (#834, below); what must never happen is the
    // box claiming a ratio the cue did not declare.
    stubCueFetch(vtt(100, 10, 90, 160));
    stubSpriteSheetSize(900, 1600);

    const { container } = render(AssetCard, { asset: asset({ file_extension: 'mp4' }) });
    await hoverCard(container);
    await flushScrub();

    const layer = spriteLayer(container);
    expect(layer, 'video tile should mount a sprite layer on hover').toBeTruthy();
    // 90/160 — portrait, not 16:9.
    expect(parseFloat(layer!.style.aspectRatio)).toBeCloseTo(90 / 160, 4);
  });

  // ── Scrub fit matches the still's fit (#834) ──────────────────────
  //
  // THE DEFECT, measured on the browse grid before the fix: a 1920x818
  // video's STILL is the 320x320 `col` rung painted `object-cover` into
  // a 367x367 tile — an exact fill with no band. The band was the SCRUB:
  // it letterboxed the 2.35:1 cue cell to `w-full` inside that square
  // tile over `bg-black/95`, so 109px of opaque black sat above and
  // below a 161px strip — 57% of the tile — and hovering swapped one
  // framing for a completely different one.
  //
  // The issue read that black as a wrongly-shaped still. It was not.
  // Both layers were internally consistent; they simply disagreed, and
  // in grid the still is the one that is right (a contact sheet fills,
  // #561/#588). So the scrub follows `fill`, exactly as the <img> does.
  describe('scrub fit follows the tile mode (#834)', () => {
    it('COVERS the square grid tile — a landscape cell binds height', async () => {
      // Grid is `fill`. A 16:9 cell bound to `w-full` in a square tile
      // is the band; bound to `h-full` it overflows the width, the
      // frame's overflow-hidden clips it, and what shows is the same
      // centred middle square `col` already shows.
      stubCueFetch(vtt(100, 10, 160, 90));
      stubSpriteSheetSize(1600, 900);

      const { container } = render(AssetCard, {
        asset: asset({ file_extension: 'mp4' }),
        mode: 'grid',
      });
      await hoverCard(container);
      await flushScrub();

      const layer = spriteLayer(container);
      expect(parseFloat(layer!.style.aspectRatio)).toBeCloseTo(16 / 9, 4);
      expect(
        layer!.className,
        'a landscape cell must bind HEIGHT to cover a square tile — w-full is the black band',
      ).toContain('h-full');
      expect(layer!.className).not.toContain('w-full');
    });

    it('COVERS the square grid tile — a portrait cell binds width', async () => {
      stubCueFetch(vtt(100, 10, 90, 160));
      stubSpriteSheetSize(900, 1600);

      const { container } = render(AssetCard, {
        asset: asset({ file_extension: 'mp4' }),
        mode: 'grid',
      });
      await hoverCard(container);
      await flushScrub();

      const layer = spriteLayer(container);
      expect(layer!.className).toContain('w-full');
      expect(layer!.className).not.toContain('h-full');
    });

    it('still CONTAINS outside grid, so a rotated clip is never cropped (#761)', async () => {
      // Masonry is not a contact sheet — it sizes each tile to its own
      // artwork and shows the whole work. The pre-#834 contain branch is
      // unchanged there: landscape binds width, portrait binds height.
      stubCueFetch(vtt(100, 10, 160, 90));
      stubSpriteSheetSize(1600, 900);

      const { container } = render(AssetCard, {
        asset: asset({ file_extension: 'mp4' }),
        mode: 'masonry',
      });
      await hoverCard(container);
      await flushScrub();

      const layer = spriteLayer(container);
      expect(parseFloat(layer!.style.aspectRatio)).toBeCloseTo(16 / 9, 4);
      expect(layer!.className).toContain('w-full');
      expect(layer!.className).not.toContain('h-full');
    });

    it('letterboxes onto the MATTE, not black, where a box still shows', async () => {
      // The still letterboxes onto `bg-thumb-matte` in every contain
      // mode. `bg-black/95` was a second, different backdrop for the
      // same job, which is what made the two states look like different
      // components rather than two frames of one.
      stubCueFetch(vtt(100, 10, 90, 160));
      stubSpriteSheetSize(900, 1600);

      const { container } = render(AssetCard, {
        asset: asset({ file_extension: 'mp4' }),
        mode: 'masonry',
      });
      await hoverCard(container);
      await flushScrub();

      const box = spriteLayer(container)!.parentElement!;
      expect(box.className).toContain('bg-thumb-matte');
      expect(box.className).not.toContain('bg-black');
    });

    it('draws no backdrop at all in grid — nothing of it can show', async () => {
      stubCueFetch(vtt(100, 10, 160, 90));
      stubSpriteSheetSize(1600, 900);

      const { container } = render(AssetCard, {
        asset: asset({ file_extension: 'mp4' }),
        mode: 'grid',
      });
      await hoverCard(container);
      await flushScrub();

      const box = spriteLayer(container)!.parentElement!;
      expect(box.className).not.toContain('bg-black');
      expect(box.className).not.toContain('bg-thumb-matte');
    });
  });

  // ── prefers-reduced-motion (#837) ─────────────────────────────────
  describe('prefers-reduced-motion (#837)', () => {
    /** jsdom has no matchMedia at all, so the component's optional call
     *  yields undefined and no-motion is the default. Give it a real one
     *  that answers the query the way the test names. */
    function stubReducedMotion(reduce: boolean) {
      vi.stubGlobal('matchMedia', (query: string) => ({
        matches: reduce && query.includes('prefers-reduced-motion: reduce'),
        media: query,
        addEventListener: () => {},
        removeEventListener: () => {},
      }));
    }

    it('mounts no scrub at all — the poster still IS the static frame', async () => {
      // Freezing the scrub on cue 0 was the obvious reading and is the
      // wrong one: cue 0 is the clip's opening frame and films open on
      // black, so it renders a black tile. The poster underneath is
      // already a chosen representative frame (#818/#829), so the
      // correct "single representative frame" is the one the card is
      // showing before the pointer arrives.
      stubReducedMotion(true);
      stubCueFetch(vtt(100, 10, 240, 134));
      stubSpriteSheetSize(2400, 1340);

      const { container } = render(AssetCard, { asset: asset({ file_extension: 'mp4' }) });
      await hoverCard(container);
      await flushScrub();

      expect(spriteLayer(container)).toBeNull();
      expect(
        container.querySelector('img'),
        'suppressing the scrub must leave the poster visible — a hover that blanks the tile is not the fix',
      ).toBeTruthy();
    });

    it('downloads neither the cue file nor the sheet when motion is reduced', async () => {
      // The preference should cost less, not the same. Both halves are
      // gated, so a reduced-motion visitor never pays for a scrub they
      // will not see.
      stubReducedMotion(true);
      stubCueFetch(vtt(100, 10, 240, 134));
      stubSpriteSheetSize(2400, 1340);

      const { container } = render(AssetCard, { asset: asset({ file_extension: 'mp4' }) });
      await hoverCard(container);
      await flushScrub();

      expect(fetch, 'no scrub means no cue fetch').not.toHaveBeenCalled();
    });

    it('still cycles when motion is not reduced', async () => {
      // The control. Without it the test above passes just as well
      // against a card whose scrub is broken outright.
      vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
      stubReducedMotion(false);
      stubCueFetch(vtt(100, 10, 240, 134));
      stubSpriteSheetSize(2400, 1340);

      const { container } = render(AssetCard, { asset: asset({ file_extension: 'mp4' }) });
      await hoverCard(container);
      await flushScrub();

      const positions = new Set<string>();
      for (let i = 0; i < 20; i++) {
        positions.add(spriteLayer(container)!.style.backgroundPosition);
        vi.advanceTimersByTime(120);
        await tick();
      }
      expect(positions.size).toBe(20);
    });
  });

  it('paints nothing while the cue file is still in flight', async () => {
    // The first frame of a scrub must not be a guess at the sheet's
    // scale. Until both the cues and the sheet measurement land there is
    // nothing correct to draw, and drawing anyway reads as a zoom jump.
    stubCueFetch(vtt(100, 10, 240, 134));
    stubSpriteSheetSize(2400, 1340);

    const { container } = render(AssetCard, { asset: asset({ file_extension: 'mp4' }) });
    await hoverCard(container);
    expect(spriteLayer(container)).toBeNull();
  });

  it('degrades silently when the extension is unknown', async () => {
    // The null case is a REAL answer, not a caller that forgot — see
    // cardAsset.ts. The cue file, not the filename, is what says a
    // scrub is possible.
    stubCueFetch(null);
    stubSpriteSheetSize(2400, 1340);

    const { container } = render(AssetCard, {
      asset: asset({ file_extension: null, scrub_available: false }),
    });
    await hoverCard(container);
    await flushScrub();
    expect(spriteLayer(container)).toBeNull();
  });
});
