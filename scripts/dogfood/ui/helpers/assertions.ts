import { Page, expect } from '@playwright/test';

/**
 * Strong assertion that a page loaded successfully: no error
 * banner, no 404 placeholder, no unhandled Svelte exception text,
 * and the SvelteKit app shell rendered (the `main` element exists
 * with content).
 *
 * The smoke suite uses this on every route so any page that
 * starts throwing a render error gets caught in the next dogfood
 * run.
 *
 * ── Why `data-testid`, not `locator('main')` ──────────────────────
 *
 * `main` is not a unique handle on this app. /admin/integrations/api
 * embeds Scalar's API reference, a Vue app which renders a `<main
 * class="references-rendered">` of its own inside our shell's one, so
 * that route legitimately has more than one `main` in the DOM and every
 * strict-mode assertion on the bare tag throws:
 *
 *   strict mode violation: locator('main') resolved to 2 elements
 *
 * It passed for a long time by winning a RACE — Scalar's bundle is a
 * dynamic import, and the assertion usually evaluated before the Vue
 * app mounted (~435ms on a fast run). Any change that made that page
 * settle a few hundred ms slower flipped it, which is what happened on
 * the sprint-27 branch: same DOM, same Scalar version (1.64.1, pinned in
 * the lockfile), 1.1s instead of 435ms, and suddenly two matches.
 *
 * We do not author Scalar's DOM and cannot fix its landmark. What we can
 * do is stop asking an ambiguous question: the shell's own `main`
 * carries `data-testid="app-shell-main"` (see routes/+layout.svelte), so
 * "did the app shell render" is asked of the app shell and a third-party
 * embed's landmarks are none of this helper's business.
 *
 * Deliberately NOT `locator('main').first()`, which would have made the
 * symptom go away while leaving the assertion pointed at whichever
 * `main` happens to come first in document order.
 */
export async function expectPageRendersCleanly(page: Page) {
  // The app shell renders <main> as soon as the route's +page
  // component mounts. If it isn't there, SvelteKit failed to
  // hydrate.
  const main = page.getByTestId('app-shell-main');
  await expect(main).toBeVisible();

  // No "404" or "Not Found" headline at the top of main. Match
  // exact-ish to avoid false positives like a page that
  // legitimately talks about HTTP 404 responses (the admin
  // system log might).
  await expect(main).not.toContainText(/^404$/);
  await expect(main).not.toContainText(/^Not Found$/);

  // No "Something went wrong" / "Error" fallback boundary.
  await expect(main).not.toContainText(/^Something went wrong$/);

  // No SvelteKit dev error overlay (only present in dev when an
  // uncaught exception fires during render).
  await expect(page.locator('vite-error-overlay')).toHaveCount(0);
}

/**
 * Asserts the page URL matches the expected path. Works against
 * full URLs since toHaveURL inspects href, not pathname.
 */
export async function expectPath(page: Page, path: string) {
  // Strip trailing slash for comparison except for root.
  const trimmed = path === '/' ? path : path.replace(/\/$/, '');
  await expect(page).toHaveURL(new RegExp(`${escapeRegex(trimmed)}/?(?:\\?.*)?$`));
}

function escapeRegex(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}
