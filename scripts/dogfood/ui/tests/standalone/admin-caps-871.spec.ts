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
// So the observable is not the end state, it is EVERY state: a recorder
// installed before any page script runs, which reports whether the panel
// was ever in the DOM between navigation start and settle.
//
// ── #1054: why that recorder is no longer requestAnimationFrame alone ─
//
// It used to be a bare rAF loop, on the reasoning that rAF is the user's
// own granularity — it fires before paint, so anything it sees got
// painted. True, and irrelevant, because rAF only fires when the
// compositor is producing frames, and in headless Chromium it does not
// start doing that until several hundred milliseconds after load.
// Measured on the dev stack: at the moment the settle assertion
// resolved, `document.readyState` was "complete", a control setInterval
// had ticked 15 times, and rAF had fired ZERO times; a second later it
// had fired 41. So the sampling window this test cares about — first
// byte through settle — was the exact window rAF never covered.
//
// That produced a test whose result tracked machine speed rather than
// the app: run in a full suite it took ~1.1s to settle, the compositor
// woke up in time, `samples` cleared its guard and the test "passed" on
// frames sampled entirely AFTER the window. Run on its own it settled in
// ~460ms, `samples` was 0, and the guard fired — which is the guard
// doing its job. Both outcomes proved nothing about the gate.
//
// So the primary instrument is now a MutationObserver, which is driven
// by the DOM rather than by the frame source and therefore cannot be
// starved: every insertion into the tree is an observation, from the
// first script-created node onward. The rAF loop is KEPT beside it and
// feeds the same counters, because when the compositor IS running it
// samples steady state that a mutation-quiet page would not report.
//
// How much stricter this actually is, measured rather than assumed: the
// observer callback runs at the microtask checkpoint AFTER a batch of
// mutations, and it reads the tree as it stands at that moment. A node
// appended and removed inside one task is therefore invisible to it too
// — verified while writing the probe below, which failed until its node
// was left in place across a task boundary. So the recorder still only
// reports states that survived to the end of a task, i.e. states the
// browser had a rendering opportunity for. It is not "any transient DOM
// shape"; it is "every state that lasted long enough to be drawn",
// which is what the rAF loop was trying and failing to observe.

import { test, expect } from '../../helpers/test';
import type { Locator, Page } from '@playwright/test';

const NO_PERMISSION = "You don't have permission to view this page.";
// #956 — the degraded panel. Distinct copy, because the whole point is
// that a user (and a test) can tell the two apart.
const CAPS_UNAVAILABLE = "We couldn't check what you're allowed to do";

interface FlashRecorder {
  __permHits?: number;
  __permSamples?: number;
  /** Observations contributed by the MutationObserver. */
  __permMutations?: number;
  /** Observations contributed by the rAF loop. Zero on a fast load — see
   *  the #1054 note above; that is the finding, not a fault. */
  __permFrames?: number;
  /** Observations contributed by the fixed-interval poll. */
  __permTicks?: number;
  __permReset?: () => void;
}

// Install the recorder. Must run as an init script so it is live before
// SvelteKit's client bundle boots — the whole window under test is
// inside the app's own startup.
async function recordPermissionFlashes(page: Page): Promise<void> {
  await page.addInitScript((needle: string) => {
    const w = window as unknown as FlashRecorder;
    w.__permHits = 0;
    w.__permSamples = 0;
    w.__permMutations = 0;
    w.__permFrames = 0;
    w.__permTicks = 0;
    w.__permReset = () => {
      w.__permHits = 0;
      w.__permSamples = 0;
      w.__permMutations = 0;
      w.__permFrames = 0;
      w.__permTicks = 0;
    };
    const sample = () => {
      w.__permSamples = (w.__permSamples ?? 0) + 1;
      const body = document.body;
      if (body && (body.textContent ?? '').includes(needle)) {
        w.__permHits = (w.__permHits ?? 0) + 1;
      }
    };

    // Instrument 1 — the DOM's own history. Driven by mutations, so it
    // is live from the first node the app appends and cannot be starved
    // by a sleeping compositor (#1054).
    const observe = () => {
      new MutationObserver(() => {
        w.__permMutations = (w.__permMutations ?? 0) + 1;
        sample();
      }).observe(document.documentElement, {
        subtree: true,
        childList: true,
        characterData: true,
      });
      // The state BEFORE any mutation counts too: a document that
      // arrived with the panel already in it would otherwise only be
      // noticed by whatever happened to change next.
      sample();
    };
    if (document.documentElement) {
      observe();
    } else {
      // An init script can run before <html> exists. `readystatechange`
      // fires with readyState="interactive" at the latest, by which
      // point documentElement is there.
      document.addEventListener('readystatechange', function once() {
        if (!document.documentElement) return;
        document.removeEventListener('readystatechange', once);
        observe();
      });
    }

    // Instrument 2 — a fixed-interval poll. This is what makes the
    // guard below deterministic: a timer is not compositor-bound, so it
    // ticks ~20 times during a 350ms admin boot on any machine, where
    // rAF measured 0 on half the loads and 17-19 on the rest (#1054).
    // On its own a 16ms poll could step over a shorter-lived panel,
    // which is exactly what instrument 1 is there for.
    setInterval(() => {
      w.__permTicks = (w.__permTicks ?? 0) + 1;
      sample();
    }, 16);

    // Instrument 3 — the frames that actually got painted, once the
    // compositor starts producing them. Kept because it is the only one
    // of the three that speaks to what was drawn rather than to what
    // existed; it just cannot be relied on to have run.
    const tick = () => {
      w.__permFrames = (w.__permFrames ?? 0) + 1;
      sample();
      requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
  }, NO_PERMISSION);
}

interface FlashReading {
  hits: number;
  samples: number;
  mutations: number;
  frames: number;
  ticks: number;
}

async function flashCount(page: Page): Promise<FlashReading> {
  return page.evaluate(() => {
    const w = window as unknown as FlashRecorder;
    return {
      hits: w.__permHits ?? 0,
      samples: w.__permSamples ?? 0,
      mutations: w.__permMutations ?? 0,
      frames: w.__permFrames ?? 0,
      ticks: w.__permTicks ?? 0,
    };
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

test('a cold navigate to an admin page never flashes the permission panel', async ({ page }, testInfo) => {
  await coldNavigate(page, '/admin/federation/peers', (p) =>
    p.getByRole('heading', { name: 'Federation peers' }));

  const { hits, samples, mutations, frames, ticks } = await flashCount(page);
  // Guard the guard: a recorder that never ran would report zero hits
  // and look like a pass. The threshold is unchanged from #871; what
  // changed is that it is now met by instruments that always run rather
  // than by a frame source that starts when it feels like it (#1054).
  expect(
    samples,
    `the recorder never ran — this test proves nothing ` +
      `(mutations=${mutations}, ticks=${ticks}, frames=${frames})`,
  ).toBeGreaterThan(3);
  // …and the DOM instrument specifically, because it is the only one
  // that cannot step over a panel shorter-lived than its poll interval.
  // Without this a future edit could drop it and the count above would
  // still clear on timer ticks alone.
  expect(
    mutations,
    `the DOM instrument did not observe a single state change during an ` +
      `admin boot — it is not installed (ticks=${ticks}, frames=${frames})`,
  ).toBeGreaterThan(0);
  expect(
    hits,
    `the admin gate rendered "${NO_PERMISSION}" on ${hits} of ${samples} observations — ` +
      'capabilities are landing after `ready` again (#871)',
  ).toBe(0);

  await page.screenshot({
    path: testInfo.outputPath('871-admin-federation-peers-no-flash.png'),
    fullPage: true,
  });
});

// Same assertion on the admin root. The gate lives in the /admin
// layout, so every child route inherits it — but the root is the one an
// operator reaches from the nav menu, and it is worth one explicit case
// rather than trusting that "the layout" is a single code path forever.
test('the admin overview never flashes the permission panel either', async ({ page }) => {
  await coldNavigate(page, '/admin', adminShell);
  const { hits, samples, mutations, frames, ticks } = await flashCount(page);
  expect(
    samples,
    `the recorder never ran — this test proves nothing ` +
      `(mutations=${mutations}, ticks=${ticks}, frames=${frames})`,
  ).toBeGreaterThan(3);
  expect(
    mutations,
    `the DOM instrument did not observe a single state change during an ` +
      `admin boot — it is not installed (ticks=${ticks}, frames=${frames})`,
  ).toBeGreaterThan(0);
  expect(hits, `/admin flashed the permission panel on ${hits}/${samples} observations`).toBe(0);
});

// Proves the sampler can actually see what it is looking for. Without
// this, "hits === 0" above is indistinguishable from a sampler that
// looks at the wrong string, the wrong node, or nothing at all — and a
// silently blind detector is worse than no detector, because it
// certifies the defect as fixed.
test('the recorder detects the panel text when it is present', async ({ page }) => {
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
  // The MutationObserver callback is a microtask-boundary away from the
  // append, so one turn of the event loop is enough — and, unlike the
  // two-rAF wait this used to do, it does not depend on the compositor
  // having woken up (#1054).
  await page.evaluate(() => new Promise<void>((r) => setTimeout(r, 0)));
  const { hits, mutations } = await flashCount(page);
  expect(
    hits,
    `the recorder failed to notice the panel text it exists to find ` +
      `(mutations observed: ${mutations})`,
  ).toBeGreaterThan(0);
});

// #1054's own regression guard. The bug that made this file worth
// reopening was an instrument that only ran once the page had already
// settled, so the window it claimed to watch went unobserved and the
// test passed anyway. This asserts the recorder is live DURING that
// window: a node appended before the settle assertion is a node the
// recorder must already have seen, on a machine where the frame source
// has not started yet.
test('the recorder is live before the page settles, not only after', async ({ page }) => {
  await recordPermissionFlashes(page);
  // Inject the needle as soon as the document exists — the earliest
  // point the app itself could render the panel — and let the settle
  // assertion run afterwards, exactly as the real tests do.
  await page.addInitScript((needle: string) => {
    document.addEventListener('readystatechange', function once() {
      if (!document.body) return;
      document.removeEventListener('readystatechange', once);
      const el = document.createElement('div');
      el.setAttribute('data-1054-probe', '');
      el.textContent = needle;
      document.body.appendChild(el);
      // Removed a task later, not inline: the real defect held the panel
      // across an in-flight capability fetch, and a node that never
      // survives a task boundary is one the browser never had a chance
      // to paint — the recorder is not supposed to report those.
      setTimeout(() => el.remove(), 0);
    });
  }, NO_PERMISSION);

  await page.goto('/admin/federation/peers');
  await expect(page.getByRole('heading', { name: 'Federation peers' })).toBeVisible({
    timeout: 15_000,
  });

  const { hits, samples, frames, mutations, ticks } = await flashCount(page);
  expect(
    mutations,
    'the MutationObserver instrument did not run at all — with only the poll ' +
      'and the rAF loop left, a panel shorter-lived than 16ms goes unseen ' +
      `(ticks=${ticks}, frames=${frames}, samples=${samples})`,
  ).toBeGreaterThan(0);
  expect(
    hits,
    'the recorder did not see a node that existed only during page startup — ' +
      'it is sampling the window AFTER settle again, which is what made ' +
      `the old rAF sampler vacuous (frames=${frames}, mutations=${mutations}, samples=${samples})`,
  ).toBeGreaterThan(0);
  // The probe node is gone by the time the page settles, so a test that
  // read the end state would see nothing. That is the whole point.
  await expect(page.locator('[data-1054-probe]')).toHaveCount(0);
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

test('a degraded capability lookup says so instead of denying permission', async ({ page }, testInfo) => {
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
    path: testInfo.outputPath('956-admin-caps-unavailable.png'),
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
