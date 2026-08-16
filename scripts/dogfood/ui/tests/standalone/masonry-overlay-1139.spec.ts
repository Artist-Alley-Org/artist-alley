// masonry-overlay-1139.spec.ts
//
// The masonry overlay may never render a CLIPPED identity row (#1139).
//
// # What the unit test cannot see
//
// `masonryOverlayTier` is pinned by its own unit test, and that test
// would pass forever while the overlay clipped — because the tier is
// only half the rule. The other half is the overlay's own anatomy: the
// kind badge's height, the identity block's, the ⋯ menu's 44px tap
// target hanging out of a block that reserves no space for it, and the
// `justify-between` column that pins the two ends apart. A padding
// change, a bigger avatar or a second metadata line moves the height a
// tier was calibrated against, and nothing in the tier function knows.
//
// So the assertion here is geometric and makes no reference to the
// thresholds at all: FOR EVERY MASONRY TILE THAT RENDERS AN OVERLAY,
// the identity block must sit inside the tile and must not collide with
// the kind badge above it. That statement stays true through any
// recalibration and fails the moment the anatomy outgrows its tier.
//
// # Why it walks the size stepper
//
// The bug lived at one narrow band of tile heights, and the default rung
// does not produce one. Rung 6 at 1920 is where the seeded wall puts a
// 925x110 span-2 tile; rung 5 at 2560 puts a 410x118 one. A single-rung
// test would have gone green on the broken code. #1025's lesson applies
// to the test as much as to the rule: a rung is a clamp, so the rung
// that exposes a given tile SIZE differs per viewport.
//
// # Overlays are forced visible
//
// The overlay is hover-and-focus revealed, and hovering 36 tiles one at
// a time is 36 round-trips against a 200ms transition — the shape that
// made #991's three observations false negatives. Opacity is forced with
// a stylesheet instead; it changes nothing about LAYOUT, which is the
// only thing being measured here.

import { test, expect } from '../../helpers/test';

/** Viewport × rung pairs. The two marked "the band" are where the
 *  seeded wall produces tiles in the compressed range — they are the
 *  cases that would have caught #1139, and the others are the control
 *  that the ordinary wall is unaffected. */
const CASES = [
  { vw: 1920, vh: 1080, rung: 4 },
  { vw: 1920, vh: 1080, rung: 5 },
  { vw: 1920, vh: 1080, rung: 6 }, // the band
  { vw: 2560, vh: 1440, rung: 5 }, // the band
  { vw: 2560, vh: 1440, rung: 6 }, // the band
];

interface TileReport {
  w: number;
  h: number;
  span: number;
  tier: string;
  /** How far the identity block runs past the tile's bottom edge. */
  overflow: number;
  /** How far it runs INTO the kind badge above it. */
  collision: number;
  hasAuthorRow: boolean;
}

test.describe('#1139 — the masonry overlay never clips its identity row', () => {
  for (const c of CASES) {
    test(`no clipped overlay at ${c.vw}px, rung ${c.rung}`, async ({ page }) => {
      await page.setViewportSize({ width: c.vw, height: c.vh });
      await page.addInitScript(
        ([rung]) => {
          localStorage.setItem('aa_browse_mode', 'masonry');
          localStorage.setItem('aa_browse_tile_min', rung);
        },
        [String(c.rung)],
      );
      await page.goto('/');
      await expect(page.locator('[data-tile-id]').first()).toBeVisible({ timeout: 20_000 });
      // Let the wall place, then reconcile against the settled images.
      await page.waitForTimeout(1500);
      await page.addStyleTag({ content: '.grid-overlay{opacity:1 !important;}' });
      await page.waitForTimeout(300);

      const report: TileReport[] = await page.evaluate(() => {
        const out = [];
        for (const tile of document.querySelectorAll<HTMLElement>('[data-tile-id]')) {
          const overlay = tile.querySelector<HTMLElement>('[data-testid="post-card-overlay"]');
          if (!overlay) continue; // minimal tier — no overlay to clip
          const identity = tile.querySelector<HTMLElement>('[data-testid="post-card-identity"]');
          if (!identity) continue;
          // The badge block is the overlay's second child; the first is
          // the bottom scrim, which is absolutely positioned and is not
          // part of the flow.
          const badge = overlay.firstElementChild?.nextElementSibling;
          const tb = tile.getBoundingClientRect();
          const ib = identity.getBoundingClientRect();
          const bb = badge?.getBoundingClientRect();
          out.push({
            w: Math.round(tb.width),
            h: Math.round(tb.height),
            span: Number(tile.dataset.tileSpan ?? 1),
            tier: identity.dataset.overlayTier ?? '(unset)',
            overflow: Math.round(ib.bottom - tb.bottom),
            collision: bb ? Math.round(bb.bottom - ib.top) : 0,
            hasAuthorRow: !!identity.querySelector('a[href^="/u/"], a[href*="/profile"]'),
          });
        }
        return out;
      });

      expect(report.length, 'no masonry tile rendered an overlay at all — the wall did not load, ' +
        'or every tile fell to the minimal tier, and either way this test asserted nothing')
        .toBeGreaterThan(0);

      const clipped = report.filter((r) => r.overflow > 1);
      expect(
        clipped,
        `identity row runs past the bottom of its tile — #1139. ${JSON.stringify(clipped)}`,
      ).toEqual([]);

      const colliding = report.filter((r) => r.collision > 1);
      expect(
        colliding,
        `identity row overlaps the kind badge above it — the overlay stack does not fit the tier ` +
          `it was given. ${JSON.stringify(colliding)}`,
      ).toEqual([]);

      // The compressed tier must actually be compressed. Without this a
      // future edit could route a short tile to `compressed` and still
      // draw the avatar row, which is the original bug with a new label.
      const fatCompressed = report.filter((r) => r.tier === 'compressed' && r.hasAuthorRow);
      expect(
        fatCompressed,
        `a compressed overlay still drew its identity row: ${JSON.stringify(fatCompressed)}`,
      ).toEqual([]);
    });
  }

  // 390px is its own assertion, not another row of the table above. A
  // masonry column there is 168px — under the width floor at EVERY rung
  // and every height — so the honest statement is "no tile offers an
  // overlay at all", and the table's "this asserted nothing" guard would
  // (correctly) fail on it. Mobile is a REDUCED app, not a shrunken one:
  // the right answer here is art plus the two controls.
  test('at 390px no masonry tile offers an overlay at any height', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.addInitScript(() => {
      localStorage.setItem('aa_browse_mode', 'masonry');
      localStorage.setItem('aa_browse_tile_min', '5');
    });
    await page.goto('/');
    await expect(page.locator('[data-tile-id]').first()).toBeVisible({ timeout: 20_000 });
    await page.waitForTimeout(1500);

    const counts = await page.evaluate(() => ({
      tiles: document.querySelectorAll('[data-tile-id]').length,
      overlays: document.querySelectorAll('[data-testid="post-card-overlay"]').length,
    }));
    expect(counts.tiles, 'the masonry wall did not render').toBeGreaterThan(0);
    expect(
      counts.overlays,
      'a 390px column is 168px wide — under the overlay width floor — so an overlay here is ' +
        'either a broken gate or a recalibration nobody drove on mobile',
    ).toBe(0);
  });
});
