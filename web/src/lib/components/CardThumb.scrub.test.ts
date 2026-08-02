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
    stubCueFetch(vtt(100, 10, 90, 160));
    stubSpriteSheetSize(900, 1600);

    const { container } = render(AssetCard, { asset: asset({ file_extension: 'mp4' }) });
    await hoverCard(container);
    await flushScrub();

    const layer = spriteLayer(container);
    expect(layer, 'video tile should mount a sprite layer on hover').toBeTruthy();
    // 90/160 — portrait, not 16:9.
    expect(parseFloat(layer!.style.aspectRatio)).toBeCloseTo(90 / 160, 4);
    // A portrait cell is bound by the tile's HEIGHT and pillarboxed;
    // width-bound would overflow the square slot.
    expect(layer!.className).toContain('h-full');
    expect(layer!.className).not.toContain('w-full');
  });

  it('keeps the landscape scrub box width-bound at 16:9 (#761 no-regression)', async () => {
    stubCueFetch(vtt(100, 10, 160, 90));
    stubSpriteSheetSize(1600, 900);

    const { container } = render(AssetCard, { asset: asset({ file_extension: 'mp4' }) });
    await hoverCard(container);
    await flushScrub();

    const layer = spriteLayer(container);
    expect(parseFloat(layer!.style.aspectRatio)).toBeCloseTo(16 / 9, 4);
    expect(layer!.className).toContain('w-full');
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
