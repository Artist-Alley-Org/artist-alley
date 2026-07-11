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
 */
export async function expectPageRendersCleanly(page: Page) {
  // The app shell renders <main> as soon as the route's +page
  // component mounts. If it isn't there, SvelteKit failed to
  // hydrate.
  await expect(page.locator('main')).toBeVisible();

  // No "404" or "Not Found" headline at the top of main. Match
  // exact-ish to avoid false positives like a page that
  // legitimately talks about HTTP 404 responses (the admin
  // system log might).
  const main = page.locator('main');
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
