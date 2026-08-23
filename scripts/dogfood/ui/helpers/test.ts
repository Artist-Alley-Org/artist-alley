// helpers/test.ts
//
// Extended Playwright `test` fixture that wraps `page.goto()` to
// wait for SvelteKit's client router to mount before returning.
//
// Why: the embedded prod build uses adapter-static with SPA
// fallback. Every non-prerendered route serves the root
// `index.html` first; the SvelteKit client router then takes
// over and mounts the actual page (sets `<title>`, renders
// `<main>`, etc.). Tests that assert title or DOM content
// immediately after `page.goto()` race against hydration —
// they pass against Vite dev (full SSR) and fail against the
// embedded build.
//
// Fix in one place rather than touching every test:
//   - Import `test` + `expect` from this file instead of from
//     `@playwright/test`.
//   - Every `page.goto(...)` now waits for hydration before
//     returning, with a 10s timeout. Tests assert against a
//     real, mounted DOM.
//
// It is also where the FIXTURE LEDGER hangs (#1247): every row a spec
// creates through the API — directly or by driving a form — is recorded
// against the spec that created it, and every delete against the spec
// that removed it, so the run can say WHICH spec left a row rather than
// only that a table drifted. That is why importing `test` from here is
// not optional: a spec that imports it from `@playwright/test` is
// invisible to the accounting, which is precisely the spec the leak
// tends to come from.
//
// Signal: `<main>` or the navbar `<header role="banner">`
// appearing in the DOM is a reliable post-hydration marker —
// neither exists in the static-shell index.html until the
// route component mounts. We wait for either; whichever the
// route renders first wins. If neither appears within the
// timeout, the wait silently exits and the test's own
// assertion fails with a clearer message than a generic
// "hydration timeout".

// AA_UI_CPU_THROTTLE additionally slows the renderer down by the given
// factor via CDP, which is how a timing-sensitive failure that only
// shows up on a loaded runner is made reproducible on a quiet
// workstation (#1024 used 3× and 6×; #1061 was diagnosed with it):
//
//   AA_UI_CPU_THROTTLE=6 npx playwright test --project standalone \
//     --grep "refines /search IN PLACE" --repeat-each=10
//
// Off by default, so CI timings are unchanged.

import { test as base, expect, type Page } from '@playwright/test';
import {
  identFor,
  ledgerPath,
  watchBrowser,
  watchContext,
  watchRequestContext,
} from './fixture-ledger';

const HYDRATION_TIMEOUT_MS = 10_000;
const HYDRATION_SIGNAL = 'main, header[role="banner"]';

const CPU_THROTTLE = Number(process.env.AA_UI_CPU_THROTTLE ?? '1');
const FRAME_STALL_MS = Number(process.env.AA_UI_FRAME_STALL_MS ?? '0');

async function applyCPUThrottle(page: Page): Promise<void> {
  if (!(CPU_THROTTLE > 1)) return;
  // Chromium-only, and deliberately silent elsewhere: the knob is a
  // diagnostic, not a gate, so a non-Chromium run should not fail on it.
  try {
    const cdp = await page.context().newCDPSession(page);
    await cdp.send('Emulation.setCPUThrottlingRate', { rate: CPU_THROTTLE });
  } catch {
    // no CDP available — run at full speed
  }
}

async function waitForHydration(page: Page): Promise<void> {
  await page
    .locator(HYDRATION_SIGNAL)
    .first()
    .waitFor({ state: 'attached', timeout: HYDRATION_TIMEOUT_MS })
    .catch(() => {
      // Swallow — if neither signal appears, the test's own
      // assertion will surface the real problem. We don't want
      // every hydration timeout to mask itself as a goto
      // failure.
    });
}

/**
 * Stretch every animation frame to `AA_UI_FRAME_STALL_MS` by burning the
 * main thread inside a rAF loop.
 *
 * A CPU throttle slows tasks and frames by the SAME factor, so it does
 * not change what falls inside one frame — which is why it alone never
 * reproduced #1061. A loaded runner is different: frames get long while
 * the CDP round-trips driving the test do not, so two `evaluate()` calls
 * that normally straddle a frame boundary land inside one. Anything the
 * browser coalesces per frame — scroll events above all — is then
 * delivered ONCE, carrying only the final value.
 *
 *   AA_UI_FRAME_STALL_MS=200 npx playwright test --project standalone \
 *     --grep "refines /search IN PLACE" --repeat-each=10
 *
 * Off by default. This is a diagnostic for timing-sensitive failures,
 * not something to run a suite under.
 */
async function applyFrameStall(page: Page): Promise<void> {
  if (!(FRAME_STALL_MS > 0)) return;
  await page.addInitScript((ms: number) => {
    const stall = () => {
      const t0 = performance.now();
      while (performance.now() - t0 < ms) {
        /* burn */
      }
      requestAnimationFrame(stall);
    };
    requestAnimationFrame(stall);
  }, FRAME_STALL_MS);
}

export const test = base.extend<{}>({
  // WHO CREATED THE ROW (#1247). All FOUR paths a spec can write through
  // are watched, because a spec leaks the same row whichever it used:
  // the `request` fixture, the browser's own traffic, `page.request`
  // (the CONTEXT's api context — neither of the first two), and any
  // context the spec opens for a second principal. Nothing here is
  // opt-in: the leak always comes from the spec that did not opt in.
  request: async ({ request }, use, testInfo) => {
    watchRequestContext(
      request as unknown as { fetch: Function },
      identFor(testInfo),
      ledgerPath(testInfo),
    );
    await use(request);
  },
  browser: [
    async ({ browser }, use, workerInfo) => {
      watchBrowser(
        browser,
        { spec: '(worker)', title: '(worker)', file: '' },
        ledgerPath(workerInfo),
      );
      await use(browser);
    },
    { scope: 'worker' },
  ],
  page: async ({ page }, use, testInfo) => {
    watchContext(page.context(), identFor(testInfo), ledgerPath(testInfo));
    await applyCPUThrottle(page);
    await applyFrameStall(page);
    const originalGoto = page.goto.bind(page);
    // Replace page.goto so every navigation auto-waits for
    // hydration. Signature mirrors Playwright's
    // (https://playwright.dev/docs/api/class-page#page-goto).
    page.goto = (async (url: string, options?: Parameters<typeof page.goto>[1]) => {
      const resp = await originalGoto(url, options);
      await waitForHydration(page);
      return resp;
    }) as typeof page.goto;
    await use(page);
  },
});

export { expect };
