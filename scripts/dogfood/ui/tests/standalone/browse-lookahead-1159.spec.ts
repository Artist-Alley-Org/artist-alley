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
// Measured on the seeded stack, thumbnail mode at 1920x1080 (the walk
// below now runs in a shorter window — see the next section — which
// makes the same bar strictly harder to clear, not easier):
//
//   before (rootMargin 600px on the implicit viewport root, which
//           <main>'s own clip rect cancelled — see the route's note):
//     ~1.5k px/s -> 9.7% of frames had unrendered area; min buffer ~0
//     ~2.7k px/s -> 15.5%
//   after (2.5 scrollport-heights, rooted on the real scrollport):
//     up to ~4.2k px/s -> 0.00%; buffer never below ~1.4 screenfuls
//
// # The walk has to fit INSIDE the seeded feed, and that decides the
//   scrollport and the page size
//
// Every assertion below is about a loader that is running. So the walk
// is only worth anything if the reader outruns what is already loaded —
// and the lookahead itself is what makes that hard to arrange, because
// the loader legitimately swallows a scrollport plus 2.5 more before the
// reader has moved a pixel. Anything the feed holds inside that first
// 3.5 screenfuls is fetched during LANDING, not during the walk.
//
// The CI seed is `aa seed --profile ci` — a set-cover subset, 82 posts,
// which is 3 pages of 36 and about 4.0k px of thumbnail wall (CI's own
// failure reported 2958px of travel, i.e. the whole feed). At 1920x1080
// the landing window is 3.5 x 1007 = 3.5k px, so, driving the dev stack
// with the feed capped to the same 82 posts:
//
//   1920x1080, limit 36   3 fetches, 3 before the walk, 0 during it
//   1920x1080, limit 12   7 fetches, 7 before the walk, 0 during it
//   1920x540,  limit 36   3 fetches, 2 before the walk, 1 during it
//   1920x540,  limit 12   7 fetches, 3 before the walk, 4 during it
//
// (Split as the probe saw it. `settle` below waits the landing chase
// out, which moves one more page from the walk column to the landing
// column — the bottom row is 4 and 3 as the spec counts it.)
//
// The whole feed was already in the DOM before the wheel turned, the
// sentinel was gone, and every gap assertion was trivially true of a
// static page: zero frames sampled a live loader. The absolute 6000px
// travel floor was the only thing that noticed, which is exactly what an
// anti-vacuity guard is for — but it reported the symptom (a short
// feed) rather than the property that was missing (a loader that ran).
//
// So the guard is now the property: at least MIN_WALK_FETCHES more pages
// requested AND landed WHILE the reader walks, plus a travel floor
// relative to the scrollport rather than an absolute pixel count. And
// the configuration is chosen to give a small seed room to satisfy it:
//
//   - a 540px-tall window, so the landing window is 3.5 x 467 = 1.6k px
//     and the rest of the feed is left for the walk to earn. The
//     lookahead is expressed in screenfuls, so this is the same
//     mechanism at a proportionally smaller size — and a HARSHER test,
//     since the buffer it buys is 1.2k px rather than 2.7k.
//   - `limit=12` rather than the route's 36, applied to the feed's own
//     query parameter on the way out, so the walk crosses several loader
//     rounds instead of one. Nothing about the response is faked; the
//     API's page size is the surface, and the request is otherwise
//     passed through untouched.
//
// Both are needed: the table above shows either one alone leaves the
// walk with fewer than two rounds on the CI seed. A page size much below
// 12 is worse, not better — the chase is sequential (one request in
// flight), so pages far shorter than the buffer cannot refill it at
// wheel speed: at 1920x700 with limit 8 the probe recorded a blank frame
// and a buffer of -17px, a fall-behind manufactured by the test rather
// than by the code under test. A taller window is worse in the other
// direction: at 1920x700 the landing chase eats so much of an 82-post
// feed that the walk is left with exactly two rounds and no margin.
//
// The one price of the short window is that it buys 1.2k px of buffer
// rather than 2.7k, so the same px/s reader consumes it faster. Driven
// under `AA_UI_CPU_THROTTLE=6` — a renderer stalled six-fold while the
// wheel keeps real time, far past any loaded runner — 200px/tick reds
// on skeleton frames 2 runs in 3 where the old 1080 walk survived 3 in
// 3. 130px/tick is 3 in 3 there and still a faster reader than the old
// walk in screenfuls, so that is the speed, and the headroom is spent
// where a loaded CI runner would spend it.
//
// # The other half: no runaway prefetch
//
// A lookahead that fixes waiting by fetching the whole feed is not a
// fix. The request count is asserted against the distance actually
// travelled, so a prefetch loop — the #1103-era failure — reds this
// even though it would trivially satisfy every gap assertion above.
//
// # ⛔ THE ROTATING FAILURE IS RESPONSE LATENCY (#1248, measured 2026-08-23)
//
// #1248 lists this spec among specs that fail and pass in rotation, with
// "129 of 214 sampled frames blank, worst 33.14px". That number was
// reproduced exactly, and it is NOT #1159 coming back.
//
// It cannot be reproduced by loading the BROWSER. Twenty-plus executions
// on the coding stack with no failure: 3 full two-worker suite runs, 5
// solo runs, 6 with two copies of itself running concurrently, and 3
// each at 3x/6x CPU throttle (which produced one skeleton frame, never a
// blank one). It reproduces on the first try by adding round-trip
// latency (AA_UI_API_LATENCY_MS, helpers/test.ts):
//
//   added latency   blank frames        worst blank
//     0 ms          0                   —          (passes)
//    50 ms          0                   —          (passes 3/3)
//   100 ms          6-15 of ~265        33.45px    (fails 3/3)
//   150 ms          67-69 of ~268       33.33px    (fails 3/3)
//   200 ms          92-94 of ~247       33.27px    (fails 3/3)
//   400 ms          132-135 of ~228     33.14px    (fails 3/3)
//
// ⭐ READ THE TWO COLUMNS AGAINST EACH OTHER. The frame COUNT scales
// with latency; the worst blank does NOT — it is pinned at ~33px at
// every latency, including the one that reproduces the reported figure
// to the last decimal. A loader falling further and further behind would
// move the second column. A reader parked at the bottom of the scroll
// range WAITING would not, and that is what this is: `scrollTop` cannot
// exceed `scrollHeight - clientHeight`, so once the reader reaches the
// end of loaded feed the measured gap stops growing and is bounded by
// whatever sits below the wall inside the scrollport.
//
// That 33px is the page's own bottom chrome, not unrendered feed:
// `space-y-4` between the wall and the sentinel (16px) + the sentinel's
// own `h-px` (1px) + the container's `py-4` bottom padding (16px)
// (web/src/routes/+page.svelte:733, :1042).
//
// So the verdict is STARVATION, and the arithmetic says it must be. The
// chase is SEQUENTIAL — one request in flight, stated above — and each
// 12-post page is ~390px of wall. At the 1.6k px/s this walk produces, a
// round trip costing more than ~240ms consumes more feed than it
// delivers, so the buffer drains by a fixed amount per round no matter
// how many screenfuls it started with. Lookahead depth expressed in
// SCREENFULS does not convert into tolerance of LATENCY; only
// concurrency or a page size that scales with reading speed would. That
// is a product enhancement to file, not a regression this spec found.
//
// ⛔ Which is why nothing here is tuned. The threshold and the wheel
// constants are untouched: the bar is still zero blank frames, this
// still reds on the pre-#1159 route, and a stack whose API answers in
// tens of milliseconds — which every stack this runs on does when it is
// not thrashing — still passes it. Raising the threshold would buy a
// green run by giving up the assertion.

import { test, expect } from '../../helpers/test';

/** Wheel delta per dispatch, and the gap between dispatches. ~130px per
 *  ~16ms is ~1.6k px/s sustained — and the unit that matters is
 *  screenfuls, because that is what the lookahead is written in: in the
 *  540px window below that is 3.5 screenfuls a second, half again as
 *  fast a reader as the 2.5/s the same walk produced at 1080. It is a
 *  speed a real reader can produce, and it is above the ~1.5k px/s at
 *  which the old code already failed — verified, not assumed: the
 *  pre-#1159 route reds this walk on the dev seed (23 of 267 frames
 *  blank) and on a feed capped to the CI seed's 82 posts (8 to 96 of
 *  ~210, run to run). */
const WHEEL_DELTA = 130;
const WHEEL_GAP_MS = 16;
/** Enough dispatches to walk ~12k px — several times the tallest wall
 *  any seeded feed puts in front of it. */
const WHEEL_TICKS = 90;

/** The window the walk runs in. Short on purpose — see the note above. */
const VIEWPORT = { width: 1920, height: 540 };

/** Feed page size for this walk, imposed on `GET /posts` via its own
 *  `limit` parameter. The route asks for 36. */
const FEED_PAGE = 12;

/** Must match `LOOKAHEAD_VIEWPORTS` in web/src/routes/+page.svelte, plus
 *  the scrollport itself: together they are how much feed the loader is
 *  entitled to hold ahead of the reader, and therefore how much of a
 *  seeded feed it consumes before the walk begins. */
const LOOKAHEAD_PORTS = 2.5;
const REACH_PORTS = 1 + LOOKAHEAD_PORTS;

/** The anti-vacuity floor: the walk has to make the loader RUN. Two
 *  rounds, not one, so that a single tail page cannot satisfy it. */
const MIN_WALK_FETCHES = 2;

const isFeedRequest = (url: string) => url.includes('/api/v1/posts?');

/** The landing chase is a SEQUENTIAL burst, so "no new request for this
 *  long" is its completion signal. Generous against a loaded runner —
 *  the wire is tens of milliseconds on this stack — because crediting a
 *  landing fetch to the walk is precisely the vacuity being closed. */
const SETTLE_QUIET_MS = 750;
const SETTLE_CAP_MS = 15_000;

/** Quiet means NOTHING IS IN FLIGHT, not just "nothing new was asked
 *  for" (#1169-era CI failure).
 *
 *  This used to watch the request count alone, and a request count that
 *  stops rising has two causes: the chase finished, or the chase is
 *  BLOCKED on a response that has not come back. The second one is what
 *  a loaded runner produces — the CI job deliberately does not wait for
 *  `preview.3d`, and the ~31 outstanding renders land squarely on the
 *  first seconds of the suite, which is when this spec runs — and the
 *  old settle() returned after its 750ms of "quiet" with the very FIRST
 *  page still on the wire.
 *
 *  What the page looks like at that moment is the reason this went
 *  unnoticed: the wall renders skeleton placeholders, which are real
 *  children of a real wall, so every check downstream was satisfied by a
 *  feed that had not arrived. Reproduced deterministically by delaying
 *  `GET /posts` by 1.5s in front of a CI-shaped stack: the wall is
 *  199px of skeletons, `main.scrollHeight` is 648 against a 467px port,
 *  and the run dies on the anti-vacuity guard reporting a short seed.
 *
 *  So the condition is BOTH: every request has an answer, and no new one
 *  for SETTLE_QUIET_MS. A request still on the wire resets the clock the
 *  same way a new request does. */
async function settle(asked: () => number, answered: () => number): Promise<void> {
  let seen = asked();
  let quietSince = Date.now();
  const deadline = Date.now() + SETTLE_CAP_MS;
  while (Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 100));
    const inFlight = asked() > answered();
    if (asked() !== seen || inFlight) {
      seen = asked();
      quietSince = Date.now();
    } else if (Date.now() - quietSince >= SETTLE_QUIET_MS) return;
  }
}

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

interface Geometry {
  /** Tiles currently in the wall — the marquee's own item attribute. */
  tiles: number;
  /** Rendered height of the wall, for px-per-tile. */
  wallPx: number;
  /** The scrollport's visible height. */
  port: number;
  /** Is the reader parked on the last pixel of the loaded feed? */
  atEnd: boolean;
}

/** Read in the page, so it costs one round trip rather than four. */
const readGeometry = (): Geometry => {
  const port = document.querySelector('main');
  const wall = document.querySelector('[data-testid="browse-wall"]');
  return {
    tiles: wall ? wall.querySelectorAll('[data-select-id]').length : 0,
    wallPx: wall ? wall.getBoundingClientRect().height : 0,
    port: port ? port.clientHeight : 0,
    atEnd: !!port && port.scrollTop + port.clientHeight >= port.scrollHeight - 2,
  };
};

test.describe('#1159 browse feed lookahead', () => {
  test('a fast continuous wheel scroll never reaches unrendered feed', async ({
    page,
    request,
  }) => {
    // The walk is ~10s of real scrolling plus login and first paint.
    test.setTimeout(120_000);

    await page.setViewportSize(VIEWPORT);
    await page.addInitScript(() => {
      try {
        localStorage.setItem('aa_browse_mode', 'thumbnail');
      } catch {
        /* storage-less context: the default mode still exercises the loader */
      }
    });

    // Shrink the feed's page size on the way out. A URL predicate rather
    // than a glob because `?` is not a wildcard here and the per-post
    // routes must not be touched.
    await page.route(
      (url) => url.pathname === '/api/v1/posts',
      async (route) => {
        const url = new URL(route.request().url());
        url.searchParams.set('limit', String(FEED_PAGE));
        await route.continue({ url: url.toString() });
      },
    );

    // Asked for, and landed. Both are counted because the anti-vacuity
    // floor below needs both halves: requests alone would be satisfied
    // by a loader that asked and never rendered, responses alone by a
    // landing chase that finished late (which `settle` also rules out).
    const feedRequests: string[] = [];
    const feedResponses: string[] = [];
    page.on('request', (r) => {
      if (isFeedRequest(r.url())) feedRequests.push(r.url());
    });
    page.on('response', (r) => {
      if (isFeedRequest(r.url()) && r.ok()) feedResponses.push(r.url());
    });

    await page.goto('/');
    const wall = page.locator('[data-testid="browse-wall"]');
    await expect(wall).toBeVisible({ timeout: 20_000 });
    // The first page has to be on screen before a scroll means anything —
    // and TILES are what "on screen" means, not children.
    //
    // This used to count `article, > div > *`, which a wall full of
    // SKELETON placeholders satisfies: the loading state is real
    // children of a real wall. On a runner slow enough to still be
    // fetching the first page, the poll passed on skeletons and every
    // measurement downstream was taken of a feed that had not landed.
    // `[data-select-id]` is the tile's own attribute — the same one
    // readGeometry() counts, so the gate and the measurement now agree
    // about what a rendered feed is.
    await expect
      .poll(() => page.locator('[data-testid="browse-wall"] [data-select-id]').count(), {
        timeout: 20_000,
      })
      .toBeGreaterThan(0);

    // Let the landing chase finish before anything is counted. The
    // loader is entitled to 3.5 screenfuls before the reader moves, and
    // it fills them in a sequential burst; a fetch still in flight when
    // the wheel turns would otherwise be credited to the walk and the
    // anti-vacuity floor below would be measuring first paint.
    await settle(
      () => feedRequests.length,
      () => feedResponses.length,
    );

    const landing = await page.evaluate(() => {
      const main = document.querySelector('main');
      const wall = document.querySelector('[data-testid="browse-wall"]');
      return {
        scrollHeight: main?.scrollHeight ?? 0,
        clientHeight: main?.clientHeight ?? 0,
        wallPx: wall ? Math.round(wall.getBoundingClientRect().height) : 0,
        tiles: wall ? wall.querySelectorAll('[data-select-id]').length : 0,
        skeletons: wall ? wall.querySelectorAll('.animate-pulse').length : 0,
      };
    });
    // The anti-vacuity guard, and it now SAYS WHICH vacuity. A short
    // page has two causes that need opposite responses — a seed too
    // small to walk (re-seed / re-tune the configuration) and a feed
    // that has not landed yet (the runner is slow and the wait above is
    // wrong) — and the message used to name only the first, which cost
    // a whole investigation the day skeletons got past it.
    expect(
      landing.scrollHeight > landing.clientHeight + 200,
      `the loaded feed does not overflow one screen, so this test would prove nothing: ` +
        `${JSON.stringify({ ...landing, feedRequests: feedRequests.length, feedResponses: feedResponses.length })}. ` +
        `tiles: 0 with skeletons > 0 means the first page had not arrived — a wait bug, not a seed one.`,
    ).toBeTruthy();

    // ── the corpus precondition, MEASURED rather than assumed (#1241) ─
    //
    // Every assertion after this one is about a loader that RUNS during
    // the walk, and whether it can run at all is a property of the
    // seeded feed, not of the code under test. When the feed was too
    // short the spec used to go red on `walkFetches` — reporting a
    // product regression for a dataset problem, which is the failure
    // that reached the owner's inbox on 08-20 and not on 08-19 at the
    // same commit.
    //
    // ⚠️ THE PRECONDITION IS CHECKED WITH AN INSTRUMENT THE BUG CANNOT
    // TOUCH. "How many posts exist" is asked of the API directly; the
    // lookahead cannot change that number. So a small corpus SKIPS, and
    // a corpus that is provably big enough while the loader still failed
    // to fetch stays a FAILURE — which is the regression this spec is
    // for. Skipping on `walkFetches` itself would have made the test
    // permanently green and stopped it watching anything.
    //
    // The landing chase has finished by here, so its page count is known
    // rather than estimated: whatever it consumed is unavailable to the
    // walk, and the walk needs MIN_WALK_FETCHES pages on top of it.
    const landingPages = feedRequests.length;
    const needPosts = (landingPages + MIN_WALK_FETCHES) * FEED_PAGE;
    const probe = await request.get(`/api/v1/posts?limit=${needPosts}`);
    const availablePosts = probe.ok()
      ? (((await probe.json()) as { items?: unknown[] }).items ?? []).length
      : -1;
    test.skip(
      availablePosts >= 0 && availablePosts < needPosts,
      `the seeded feed cannot support this walk: ${availablePosts} post(s) are visible, ` +
        `and the landing chase already spent ${landingPages} page(s) of ${FEED_PAGE}, ` +
        `leaving fewer than the ${MIN_WALK_FETCHES} the walk needs to make the loader run. ` +
        `Seed more posts. This is a DATASET shortfall, not a lookahead regression — the ` +
        `two are told apart by asking the API how much feed exists, which no lookahead ` +
        `behaviour can influence.`,
    );

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
    const atWalkStart = {
      requests: feedRequests.length,
      responses: feedResponses.length,
      geometry: (await page.evaluate(readGeometry)) as Geometry,
    };

    await page.mouse.move(VIEWPORT.width / 2, VIEWPORT.height / 2);
    for (let i = 0; i < WHEEL_TICKS; i++) {
      await page.mouse.wheel(0, WHEEL_DELTA);
      await page.waitForTimeout(WHEEL_GAP_MS);
    }

    const samples = (await page.evaluate(() => {
      const w = window as unknown as { __aaRun?: boolean; __aaSamples?: Sample[] };
      w.__aaRun = false;
      return w.__aaSamples ?? [];
    })) as Sample[];
    const geometry = (await page.evaluate(readGeometry)) as Geometry;

    expect(samples.length, 'the sampler recorded no frames').toBeGreaterThan(60);

    // ── the walk has to have exercised the loader ────────────────────
    // Not "did the page move far enough" — a feed can travel a long way
    // on content that was already loaded, and on the CI seed at the
    // original 1920x1080 it travelled its whole length without the
    // loader running once. The property is that the reader outran the
    // buffer often enough for the loader to answer, and that what it
    // asked for came back.
    const travelled = samples[samples.length - 1].y - samples[0].y;
    const walkFetches = feedRequests.length - atWalkStart.requests;
    const walkCommits = feedResponses.length - atWalkStart.responses;
    // The corpus was PROVED sufficient by the precondition above, which
    // asked the API how much feed exists — so reaching here with too few
    // fetches is the LOADER, not the dataset. The message used to blame
    // the seed, which is what sent a dataset-shaped explanation to the
    // owner for a run that had already ruled the dataset out (#1241).
    const tooSmall =
      `the loader did not run during the walk, and the corpus is not the reason: ` +
      `${availablePosts} post(s) were confirmed available before the wheel turned, ` +
      `against the ${needPosts} this walk needs. The wall held ` +
      `${atWalkStart.geometry.tiles} posts when the wheel turned and ` +
      `${geometry.tiles} at the end, asking for ${walkFetches} more page(s) ` +
      `and landing ${walkCommits} — under the ${MIN_WALK_FETCHES} required. ` +
      `A sufficient corpus with a loader that stopped fetching is the ` +
      `regression this spec is for.`;
    expect(walkFetches, tooSmall).toBeGreaterThanOrEqual(MIN_WALK_FETCHES);
    expect(walkCommits, tooSmall).toBeGreaterThanOrEqual(MIN_WALK_FETCHES);

    // And the reader has to have covered ground while doing it —
    // measured in screenfuls, because that is the unit the lookahead is
    // written in, and satisfied either way a dataset can satisfy it:
    // by running out of feed, or by not running out for 2.5 screenfuls.
    const travelFloor = LOOKAHEAD_PORTS * geometry.port;
    expect(
      geometry.atEnd || travelled > travelFloor,
      `the wheel scroll moved the feed ${travelled}px and stopped short of its end, ` +
        `under the ${Math.round(travelFloor)}px (${LOOKAHEAD_PORTS} scrollports) this ` +
        'would need to prove anything',
    ).toBeTruthy();

    // ── the bar ──────────────────────────────────────────────────────
    const blankFrames = samples.filter((s) => s.blank > 0);
    const skeletonFrames = samples.filter((s) => s.skeleton > 0);

    // ⚠️ THE px FIGURE IS NOT A DEPTH, and reading it as one sent a whole
    // investigation the wrong way (see the latency table in the header).
    // It saturates at ~33px — the chrome below the wall — because the
    // reader cannot scroll past the end of the scroll range. What scales
    // with how badly the loader is losing is the FRAME COUNT, so the
    // message leads with the time those frames represent.
    const blankPct = Math.round((blankFrames.length / Math.max(1, samples.length)) * 100);
    expect(
      blankFrames.length,
      `${blankFrames.length}/${samples.length} frames (${blankPct}% of the walk) showed the ` +
        `reader at the end of loaded feed with more pages still to come — the loader fell ` +
        `behind the reader. Worst gap ${Math.max(0, ...blankFrames.map((s) => s.blank))}px, ` +
        `which SATURATES at the ~33px of chrome below the wall and is therefore not a ` +
        `measure of how far behind it fell; the percentage is. If the API on this stack is ` +
        `answering slowly, that is the cause — this walk cannot absorb a round trip over ` +
        `~240ms and the header says why.`,
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
    //
    // Every frame that sampled a live sentinel counts. This used to run
    // only when there were more than 20 of them, which on a feed the
    // size of the CI seed is a silent skip of the one assertion that
    // fails on the pre-#1159 code; the walk-fetch floor above is what
    // now guarantees there are frames to average, so the skip can go.
    const live = samples.filter((s) => s.buffer !== null) as (Sample & { buffer: number })[];
    expect(
      live.length,
      'no frame sampled a live loader, so the buffer was never measured',
    ).toBeGreaterThan(0);
    const minBuffer = Math.min(...live.map((s) => s.buffer));
    expect(
      minBuffer,
      `the unread buffer fell to ${minBuffer}px (scrollport is ${geometry.port}px) — the feed is ` +
        'refilling at the fold instead of ahead of it, which is the pre-#1159 behaviour',
    ).toBeGreaterThan(geometry.port);

    // ── and no runaway prefetch ──────────────────────────────────────
    // Measured, not assumed: px-per-tile comes off the wall this run
    // actually rendered, so the arithmetic holds at whatever tile size,
    // mode and seed the stack is running. A loader that stays one
    // lookahead ahead has fetched the ground the reader covered plus the
    // 3.5 screenfuls it is entitled to hold, and no more.
    //
    // The arithmetic is a LOWER bound — the loader tops up to a whole
    // page past the buffer's edge, and the landing chase overshoots by
    // the same rounding — so a quarter over it plus two pages absorbs
    // that. A prefetch loop overshoots by orders of magnitude, not by a
    // quarter: on the dev seed it would be the whole feed.
    const pagePx = (geometry.wallPx / Math.max(1, geometry.tiles)) * FEED_PAGE;
    const reachPx = travelled + REACH_PORTS * geometry.port;
    const ceiling = Math.ceil(Math.ceil(reachPx / pagePx) * 1.25) + 2;
    expect(
      feedRequests.length,
      `${feedRequests.length} /posts requests to cover ${Math.round(reachPx)}px of feed at ` +
        `~${Math.round(pagePx)}px per page — runaway prefetch`,
    ).toBeLessThanOrEqual(ceiling);
  });
});
