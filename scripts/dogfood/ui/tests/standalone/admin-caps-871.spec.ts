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
// #956 — the degraded panel. Distinct copy, because the whole point is
// that a user (and a test) can tell the two apart.
const CAPS_UNAVAILABLE = "We couldn't check what you're allowed to do";

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

// ── #956 — "could not determine your rights" ≠ "you have none" ───────
//
// The nightly failure that produced this issue rendered
// NO_PERMISSION at an administrator who demonstrably held system.admin
// one second earlier. The mechanism: the server's capability lookup
// failed, hydrateCapabilities omitted the key, and the client's
// fallback ("absent means holds nothing") was faithfully rendered as a
// permission refusal. Four triage passes went into narrowing that,
// because the panel is byte-identical to the one a genuinely powerless
// account sees — so neither the run log, nor the operator, nor a test
// could say which had happened.
//
// The stimulus here is the WIRE SHAPE, not a stubbed function. That is
// deliberate: /auth/me returning `capabilities_status: "unavailable"`
// is the entire contract between the two halves of the fix, and
// app/internal/auth/session_capabilities_test.go
// (TestHydrateCapabilities_FailedLookupReportsUnavailable) pins that a
// failing resolver really does produce this shape. Route interception
// is how this side gets to assume it without a broken database.
//
// Note the route is intercepted BEFORE goto, so the degraded body is
// what +layout.ts's boot fetch receives — i.e. this exercises the cold
// navigate, which is the only path that reaches the admin gate with a
// fresh session (test 8 of the nightly; see the pass-5 correction on
// #956 for why markReady is not involved).
async function withUnavailableCapabilities(page: Page): Promise<void> {
  await page.route('**/api/v1/auth/me', async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    // Exactly what the server sends when EffectiveCapabilitiesForUser
    // errors: the key omitted, the status saying why.
    delete body.capabilities;
    body.capabilities_status = 'unavailable';
    await route.fulfill({ response: res, json: body });
  });
}

test('a degraded capability lookup says so instead of denying permission', async ({ page }) => {
  await withUnavailableCapabilities(page);
  await page.goto('/admin/federation/peers');

  await expect(
    page.getByText(CAPS_UNAVAILABLE),
    'a failed capability lookup did not surface as its own state — this is the ' +
      '#956 defect, where a resolver blip is indistinguishable from having no rights',
  ).toBeVisible({ timeout: 15_000 });

  // The half that must NOT change. Failing closed on rights is correct
  // and stays correct; the fix is about the explanation, and a fix that
  // made the explanation honest by also showing the page would be a
  // privilege escalation.
  const text = (await page.locator('body').innerText()).trim();
  expect(
    text,
    'the degraded state accused the administrator of lacking permission — ' +
      'the two states must stay distinguishable',
  ).not.toContain(NO_PERMISSION);

  await page.screenshot({
    path: path.join(SHOT_DIR, '956-admin-caps-unavailable.png'),
    fullPage: true,
  });
});

test('a degraded capability lookup renders NO admin controls', async ({ page }) => {
  await withUnavailableCapabilities(page);
  await page.goto('/admin/federation/peers');
  await expect(page.getByText(CAPS_UNAVAILABLE)).toBeVisible({ timeout: 15_000 });

  // The sidebar renders only inside the gate's granted branch, so its
  // absence is proof the gate stayed shut. Asserted alongside the
  // page's own heading: "the shell is hidden" and "the content is
  // hidden" are two claims and this state owes both.
  await expect(
    adminShell(page),
    'the admin sidebar rendered on a session whose rights are unknown',
  ).toHaveCount(0);
  await expect(
    page.getByRole('heading', { name: 'Federation peers' }),
    'the gated page rendered on a session whose rights are unknown',
  ).toHaveCount(0);
});

// The other direction, and the one a fix could plausibly break: an
// account that genuinely holds nothing must still be TOLD it holds
// nothing. If this started showing the retry panel, the change would
// have replaced one wrong answer with another — and this one loops
// forever, because retrying resolves the same empty set every time.
test('an account that genuinely holds nothing still gets the permission panel', async ({ page }) => {
  await page.route('**/api/v1/auth/me', async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    // The lookup SUCCEEDED. It just came back empty.
    body.capabilities = [];
    body.capabilities_status = 'resolved';
    await route.fulfill({ response: res, json: body });
  });
  await page.goto('/admin/federation/peers');

  await expect(page.getByText(NO_PERMISSION)).toBeVisible({ timeout: 15_000 });
  const text = (await page.locator('body').innerText()).trim();
  expect(
    text,
    'a resolved-but-empty capability set was reported as an error the user could retry — ' +
      'it is not an error, and retrying it will never produce a different answer',
  ).not.toContain(CAPS_UNAVAILABLE);
  await expect(adminShell(page)).toHaveCount(0);
});
