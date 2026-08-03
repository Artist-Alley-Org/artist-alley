// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #871 — an administrator must never be told they lack permission.
//
// The defect this pins was not "the admin page fails to load". It
// loaded fine — eventually. The auth store flipped `ready` as soon as
// /auth/me resolved, but the capability set came from a SECOND request
// (/auth/me/capabilities), so for the window between the two the
// /admin gate evaluated "ready, and holds nothing" and rendered its red
// "You don't have permission to view this page." panel at a real
// admin. Then the second response landed and the page quietly appeared.
//
// WHY THIS TEST LOOKS THE WAY IT DOES. The obvious test — go to the
// page, read the text, assert the string is absent — passes on the
// broken build, because by the time a settled assertion runs the panel
// has already replaced itself. That is not a hypothetical: it is how
// this shipped, and phase-badge-801.spec.ts only caught it because its
// federation case happens to read the DOM early enough to sometimes
// lose the race. "Sometimes" is not a test.
//
// So the observable is not the end state, it is EVERY state. A
// requestAnimationFrame sampler installed before any page script runs
// records whether the panel was ever in the DOM on any frame from
// first paint through settle. rAF is the right granularity precisely
// because it is the user's: it fires once per frame, before paint, so
// anything it sees is something that got painted, and anything it
// misses was never on screen.

import { test, expect, type Locator, type Page } from '@playwright/test';
import path from 'node:path';

const SHOT_DIR = process.env.SHOT_DIR ?? '/tmp';
const NO_PERMISSION = "You don't have permission to view this page.";

interface FlashRecorder {
  __permHits?: number;
  __permSamples?: number;
  __permReset?: () => void;
}

// Install the per-frame sampler. Must run as an init script so it is
// live before SvelteKit's client bundle boots — the whole window under
// test is inside the app's own startup.
async function recordPermissionFlashes(page: Page): Promise<void> {
  await page.addInitScript((needle: string) => {
    const w = window as unknown as FlashRecorder;
    w.__permHits = 0;
    w.__permSamples = 0;
    w.__permReset = () => {
      w.__permHits = 0;
      w.__permSamples = 0;
    };
    const sample = () => {
      w.__permSamples = (w.__permSamples ?? 0) + 1;
      const body = document.body;
      if (body && (body.textContent ?? '').includes(needle)) {
        w.__permHits = (w.__permHits ?? 0) + 1;
      }
    };
    const tick = () => {
      sample();
      requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
  }, NO_PERMISSION);
}

async function flashCount(page: Page): Promise<{ hits: number; samples: number }> {
  return page.evaluate(() => {
    const w = window as unknown as FlashRecorder;
    return { hits: w.__permHits ?? 0, samples: w.__permSamples ?? 0 };
  });
}

// Cold navigate: a full document load straight at an admin URL, which
// is what a bookmark, a pasted link, or a browser reload does — and the
// only path that runs +layout.ts's hydrateFrom → markReady sequence
// with the gate watching.
async function coldNavigate(page: Page, url: string, settled: (p: Page) => Locator) {
  await recordPermissionFlashes(page);
  await page.goto(url);
  await expect(settled(page)).toBeVisible({ timeout: 15_000 });
}

// The admin sidebar renders only inside the gate's granted branch, so
// its presence is proof the gate said yes — on every /admin/* route,
// including ones with no distinctive heading of their own.
const adminShell = (p: Page) => p.getByRole('complementary', { name: 'Admin' });

test('a cold navigate to an admin page never flashes the permission panel', async ({ page }) => {
  await coldNavigate(page, '/admin/federation/peers', (p) =>
    p.getByRole('heading', { name: 'Federation peers' }));

  const { hits, samples } = await flashCount(page);
  // Guard the guard: a sampler that never ran would report zero hits
  // and look like a pass. A real page load is dozens of frames.
  expect(samples, 'the frame sampler never ran — this test proves nothing').toBeGreaterThan(3);
  expect(
    hits,
    `the admin gate rendered "${NO_PERMISSION}" on ${hits} of ${samples} sampled frames — ` +
      'capabilities are landing after `ready` again (#871)',
  ).toBe(0);

  await page.screenshot({
    path: path.join(SHOT_DIR, '871-admin-federation-peers-no-flash.png'),
    fullPage: true,
  });
});

// Same assertion on the admin root. The gate lives in the /admin
// layout, so every child route inherits it — but the root is the one an
// operator reaches from the nav menu, and it is worth one explicit case
// rather than trusting that "the layout" is a single code path forever.
test('the admin overview never flashes the permission panel either', async ({ page }) => {
  await coldNavigate(page, '/admin', adminShell);
  const { hits, samples } = await flashCount(page);
  expect(samples, 'the frame sampler never ran — this test proves nothing').toBeGreaterThan(3);
  expect(hits, `/admin flashed the permission panel on ${hits}/${samples} frames`).toBe(0);
});

// Proves the sampler can actually see what it is looking for. Without
// this, "hits === 0" above is indistinguishable from a sampler that
// looks at the wrong string, the wrong node, or nothing at all — and a
// silently blind detector is worse than no detector, because it
// certifies the defect as fixed.
test('the frame sampler detects the panel text when it is present', async ({ page }) => {
  await coldNavigate(page, '/admin/federation/peers', (p) =>
    p.getByRole('heading', { name: 'Federation peers' }));
  await page.evaluate(() => {
    (window as unknown as FlashRecorder).__permReset?.();
  });
  await page.evaluate((needle: string) => {
    const el = document.createElement('div');
    el.textContent = needle;
    document.body.appendChild(el);
  }, NO_PERMISSION);
  // Two frames: one for the mutation to be live at sample time, one of
  // slack for scheduling.
  await page.evaluate(
    () => new Promise<void>((r) => requestAnimationFrame(() => requestAnimationFrame(() => r()))),
  );
  const { hits } = await flashCount(page);
  expect(hits, 'the sampler failed to notice the panel text it exists to find').toBeGreaterThan(0);
});
