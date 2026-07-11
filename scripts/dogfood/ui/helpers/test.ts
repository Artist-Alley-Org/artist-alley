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
// Signal: `<main>` or the navbar `<header role="banner">`
// appearing in the DOM is a reliable post-hydration marker —
// neither exists in the static-shell index.html until the
// route component mounts. We wait for either; whichever the
// route renders first wins. If neither appears within the
// timeout, the wait silently exits and the test's own
// assertion fails with a clearer message than a generic
// "hydration timeout".

import { test as base, expect, type Page } from '@playwright/test';

const HYDRATION_TIMEOUT_MS = 10_000;
const HYDRATION_SIGNAL = 'main, header[role="banner"]';

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

export const test = base.extend<{}>({
  page: async ({ page }, use) => {
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
