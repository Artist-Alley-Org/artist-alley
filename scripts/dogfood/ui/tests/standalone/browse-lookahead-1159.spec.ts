// browse-lookahead-1159.spec.ts
//
// The browse feed's next page must already be there when the reader
// arrives (#1159). Never wait at wheel speed.
//
// # Why this can only be measured in-page
//
// The failure is transient by construction: a gap opens at the tail of
// the wall and closes again as soon as the append lands. Sampling it
// over the Playwright wire — a round trip per observation — is the shape
// that made all three of #991's observations false negatives, and here
// it is worse, because the gaps this pins last two or three FRAMES. So
// the whole measurement is a rAF loop installed in the page, which
// records one row per frame and is read back once at the end.
//
// # What "unrendered feed area" means, precisely
//
// Two things, both recorded:
//
//   blank    — the wall's bottom edge is above the scrollport's bottom
//              edge while more pages exist. The reader is looking past
//              the end of loaded content at nothing.
//   skeleton — a loading placeholder tile intersects the scrollport.
//              Skeletons are the honest fallback for a genuinely slow
//              response and are not being removed; the bar is that a
//              reader at ordinary wheel speed never reaches one.
//
// # And the buffer, which is the mechanism rather than the symptom
//
// Zero blank frames is the OUTCOME, but it is also satisfiable by luck
// on a fast quiet runner — a feed with no lookahead at all can win the
// race if every append happens to land in time. So the spec also pins
// the unread buffer: how much loaded-but-unseen feed sits below the
// fold, sampled every frame. That is the thing the fix actually
// installs, it degrades gracefully under a loaded runner instead of
// flipping, and it is what fails loudly on the pre-#1159 code.
//
// Measured on the seeded stack, thumbnail mode at 1920x1080:
//
//   before (rootMargin 600px on the implicit viewport root, which
//           <main>'s own clip rect cancelled — see the route's note):
//     ~1.5k px/s -> 9.7% of frames had unrendered area; min buffer ~0
//     ~2.7k px/s -> 15.5%
//   after (2.5 scrollport-heights, rooted on the real scrollport):
//     up to ~4.2k px/s -> 0.00%; buffer never below ~1.4 screenfuls
//
// # The other half: no runaway prefetch
//
// A lookahead that fixes waiting by fetching the whole feed is not a
// fix. The request count is asserted against the distance actually
// travelled, so a prefetch loop — the #1103-era failure — reds this
// even though it would trivially satisfy every gap assertion above.

import { test, expect } from '../../helpers/test';

/** Wheel delta per dispatch, and the gap between dispatches. ~200px per
 *  ~16ms is ~2.5k px/s sustained: a fast continuous wheel scroll, above
 *  the ~1.5k px/s at which the old code already failed and inside the
 *  range a real reader can produce. */
const WHEEL_DELTA = 200;
const WHEEL_GAP_MS = 16;
/** Enough dispatches to walk well past ten 36-post pages of thumbnail
 *  tiles (~1.4k px each at 1920x1080). */
const WHEEL_TICKS = 90;

interface Sample {
  /** Scrollport offset when this frame was recorded. */
  y: number;
  /** px of blank below the wall while more pages exist. */
  blank: number;
  /** Skeleton placeholders intersecting the scrollport. */
  skeleton: number;
  /** px of loaded-but-unseen feed below the fold, or null once the feed
   *  is exhausted (there is nothing left to be ahead of). */
  buffer: number | null;
}

test.describe('#1159 browse feed lookahead', () => {
  test('a fast continuous wheel scroll never reaches unrendered feed', async ({ page }) => {
    // The walk is ~10s of real scrolling plus login and first paint.
    test.setTimeout(120_000);

    await page.setViewportSize({ width: 1920, height: 1080 });
    await page.addInitScript(() => {
      try {
        localStorage.setItem('aa_browse_mode', 'thumbnail');
      } catch {
        /* storage-less context: the default mode still exercises the loader */
      }
    });

    const pageRequests: string[] = [];
    page.on('request', (r) => {
      if (r.url().includes('/api/v1/posts?')) pageRequests.push(r.url());
    });

    await page.goto('/');
    const wall = page.locator('[data-testid="browse-wall"]');
    await expect(wall).toBeVisible({ timeout: 20_000 });
    // The first page has to be on screen before a scroll means anything.
    await expect
      .poll(() => page.locator('[data-testid="browse-wall"] article, [data-testid="browse-wall"] > div > *').count(), {
        timeout: 20_000,
      })
      .toBeGreaterThan(0);

    const scrollable = await page.evaluate(() => {
      const main = document.querySelector('main');
      return !!main && main.scrollHeight > main.clientHeight + 200;
    });
    expect(
      scrollable,
      'the seeded feed does not overflow one screen, so this test would prove nothing',
    ).toBeTruthy();

    // ── the in-page sampler ──────────────────────────────────────────
    await page.evaluate(() => {
      const w = window as unknown as { __aaSamples?: unknown[]; __aaRun?: boolean };
      w.__aaSamples = [];
      w.__aaRun = true;
      const port = document.querySelector('main') as HTMLElement;
      const tick = () => {
        if (!w.__aaRun) return;
        const wallEl = document.querySelector('[data-testid="browse-wall"]');
        if (wallEl && port) {
          const pr = port.getBoundingClientRect();
          const wr = wallEl.getBoundingClientRect();
          // The sentinel renders only while more pages exist, so its
          // presence IS the loader's own "there is more" signal.
          const sentinel = wallEl.parentElement?.querySelector(
            ':scope > div[aria-hidden="true"].h-px',
          );
          let skeleton = 0;
          for (const el of wallEl.querySelectorAll('.animate-pulse')) {
            const b = el.getBoundingClientRect();
            if (b.bottom > pr.top && b.top < pr.bottom) skeleton++;
          }
          (w.__aaSamples as unknown[]).push({
            y: port.scrollTop,
            blank: sentinel ? Math.max(0, pr.bottom - wr.bottom) : 0,
            skeleton,
            buffer: sentinel
              ? Math.round(sentinel.getBoundingClientRect().top - pr.bottom)
              : null,
          });
        }
        requestAnimationFrame(tick);
      };
      requestAnimationFrame(tick);
    });

    // ── the scroll ───────────────────────────────────────────────────
    // Wheel over the scrollport, not `window.scrollTo`: this app scrolls
    // an inner <main>, and a programmatic jump would skip exactly the
    // continuous motion the bug needs (and would not produce the wheel
    // events the auto-hiding chrome listens to either).
    await page.mouse.move(960, 600);
    for (let i = 0; i < WHEEL_TICKS; i++) {
      await page.mouse.wheel(0, WHEEL_DELTA);
      await page.waitForTimeout(WHEEL_GAP_MS);
    }

    const samples = (await page.evaluate(() => {
      const w = window as unknown as { __aaRun?: boolean; __aaSamples?: Sample[] };
      w.__aaRun = false;
      return w.__aaSamples ?? [];
    })) as Sample[];

    expect(samples.length, 'the sampler recorded no frames').toBeGreaterThan(60);

    const travelled = samples[samples.length - 1].y - samples[0].y;
    expect(
      travelled,
      'the wheel scroll did not move the feed, so this test would prove nothing',
    ).toBeGreaterThan(6000);

    // ── the bar ──────────────────────────────────────────────────────
    const blankFrames = samples.filter((s) => s.blank > 0);
    const skeletonFrames = samples.filter((s) => s.skeleton > 0);

    expect(
      blankFrames.length,
      `${blankFrames.length}/${samples.length} frames showed blank below the wall ` +
        `(worst ${Math.max(0, ...blankFrames.map((s) => s.blank))}px) — the loader fell behind the reader`,
    ).toBe(0);

    expect(
      skeletonFrames.length,
      `${skeletonFrames.length}/${samples.length} frames had a loading skeleton on screen — ` +
        'the reader reached the tail before the next page rendered',
    ).toBe(0);

    // The mechanism, not the symptom: while there is more feed to fetch,
    // there must always be at least a screenful of it already loaded
    // below the fold. The route buys 2.5 screenfuls; one is the floor
    // that separates "kept ahead" from "won the race".
    const live = samples.filter((s) => s.buffer !== null) as (Sample & { buffer: number })[];
    if (live.length > 20) {
      const minBuffer = Math.min(...live.map((s) => s.buffer));
      const port = await page.evaluate(() => document.querySelector('main')!.clientHeight);
      expect(
        minBuffer,
        `the unread buffer fell to ${minBuffer}px (scrollport is ${port}px) — the feed is ` +
          'refilling at the fold instead of ahead of it, which is the pre-#1159 behaviour',
      ).toBeGreaterThan(port);
    }

    // ── and no runaway prefetch ──────────────────────────────────────
    // One page is ~1.4k px of thumbnail tiles at this viewport. The
    // reader travelled `travelled` px, so a loader that stays one page
    // ahead makes about that many requests plus the landing burst. Three
    // pages of slack over the arithmetic; a prefetch loop overshoots by
    // orders of magnitude, not by three.
    const pagesRead = Math.ceil(travelled / 1400) + 1;
    expect(
      pageRequests.length,
      `${pageRequests.length} /posts requests for ~${pagesRead} pages of travel — runaway prefetch`,
    ).toBeLessThanOrEqual(pagesRead + 3);
  });
});
