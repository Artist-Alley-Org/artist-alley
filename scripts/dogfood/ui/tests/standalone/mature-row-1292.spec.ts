// mature-row-1292.spec.ts
//
// The Mature row in the browse filter menu's CONTENT category (#1292),
// which is ADR 0090's LAYER 3: the view.
//
// # Three layers, and only the third one is here
//
//   1 INSTANCE  /admin/system/mature-content: does this install carry
//               adult work at all.
//   2 ACCOUNT   /account/preferences: this reader consents to be shown
//               it.
//   3 VIEW      this row: include it in THESE results, right now.
//
// # ⭐ LAYER 3 NARROWS AND NEVER CONSENTS, which decides every case below
//
// Layer 2 is the consent, so this row may only ever subtract from rows
// the three conjuncts have already allowed. Two consequences are
// asserted rather than described:
//
//   IT DEFAULTS TO INCLUDED. `the row rests TICKED and changes nothing`
//   is what says shipping this changed no wall for a reader who had
//   already opted in. A control that defaulted to narrowing would have
//   silently hidden content from every consenting reader on upgrade.
//
//   IT CANNOT WIDEN. `it never widens for a reader who has not
//   consented` drives the wall for an opted-OUT reader with the stored
//   flag set both ways and requires the mature post to be absent from
//   both. There is no spelling of this control that adds a row.
//
// # ⛔ BOTH CASCADE RUNGS ARE ABSENCE, NOT DISABLEMENT
//
// A disabled row would advertise a filter this reader can never use and
// name a class of content the operator may have switched off entirely.
// So the two `does not render` cases assert `toHaveCount(0)`, and each
// one also asserts the AI row is STILL THERE beside it: that is what
// makes them statements about the cascade rather than about a panel
// that failed to open.
//
// # The fixture is a PAIR, for the rule-of-two reason
//
// One mature post and one plain one, both by the fixture's own author
// and both matching a token nobody else's corpus carries. "The mature
// post went" alone passes on a predicate that empties the wall; the
// plain post surviving is what separates a filter from an outage.
//
// # ⚠️ CONTENDED STATE, and there are TWO kinds of it here
//
// The INSTANCE switch is one value for the whole box, taken through the
// cross-file lock in helpers/mature-content.ts for the same reason
// public_mode is (#1248). The ACCOUNT opt-in is per account, and this
// file writes the admin's, so it re-GETs the whole preferences document
// and re-sends every member with one swapped: `UserPreferencesRequest`
// treats an ABSENT member as a RESET, and a partial write here would
// wipe the browse-rail curation and the notification channels of the
// account every other spec in the suite runs as.

import { test, expect, type Page } from '../../helpers/test';
import type { APIRequestContext } from '@playwright/test';
import { loginAsAdminViaAPI, LOGGED_OUT } from '../../helpers/auth';
import { tid } from '../../helpers/testids';
import { matureContentHold } from '../../helpers/mature-content';

/** A token in every fixture title and nowhere in the seed, so
 *  `/?q=<TOKEN>` renders a wall of exactly these two posts. */
const TOKEN = `maturerow${Date.now()}`;

let matureId = '';
let plainId = '';
const assetIds: string[] = [];

const matureSwitch = matureContentHold('mature-row-1292');

/** The account's opt-in as it was before this file ran, so the restore
 *  puts back a value rather than a guess. */
let priorOptIn: boolean | undefined;

type Prefs = Record<string, unknown>;

/** Read the WHOLE preferences document. */
async function readPrefs(request: APIRequestContext): Promise<Prefs> {
  const r = await request.get('/api/v1/account/preferences');
  expect(r.status(), 'preferences must be readable').toBe(200);
  return (await r.json()) as Prefs;
}

/**
 * Set the account's mature opt-in.
 *
 * ⛔ RE-GET AND MERGE, ALWAYS. `UserPreferencesRequest` treats an
 * absent member as a RESET and an absent key inside a present member as
 * a reset of that key, so sending `{mature_content:{show}}` on its own
 * would wipe this account's browse-rail curation and notification
 * channels. `account/preferences/+page.svelte` documents the same
 * hazard and takes the same round trip; a spec that took the shortcut
 * would corrupt the identity every other spec in the suite signs in as,
 * and the damage would surface in an unrelated file.
 *
 * It asserts the PERSISTED value rather than the response echo (#946).
 */
async function setOptIn(request: APIRequestContext, show: boolean): Promise<void> {
  const base = await readPrefs(request);
  const body: Prefs = { ...base, mature_content: { show } };
  // Read-only members the request schema does not carry.
  delete body.known_channels;
  delete body.known_event_types;
  const w = await request.patch('/api/v1/account/preferences', { data: body });
  expect(w.status(), `opt-in must be settable to ${show}: ${await w.text()}`).toBe(200);

  const fresh = await readPrefs(request);
  expect(
    (fresh.mature_content as { show?: boolean } | undefined)?.show ?? false,
    'the opt-in must be READ BACK, not taken from the write response',
  ).toBe(show);
  expect(fresh.browse_rail, 'the merge must not have reset an unrelated member').toEqual(
    base.browse_rail,
  );
}

async function makeAsset(request: APIRequestContext, mature: boolean): Promise<string> {
  const up = await request.post('/api/v1/storage/objects', {
    // Unique bytes per asset: byte-identical uploads by one owner are
    // COLLAPSED by the content-address unique index, and a collapsed
    // asset would become a member of both fixture posts, which is
    // exactly the blast radius this pair exists to keep at one each.
    data: Buffer.from(`${TOKEN} ${mature} ${Math.random()}`),
    headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'text/plain' },
  });
  expect(up.status()).toBe(201);
  const { hash } = (await up.json()) as { hash: string };

  const r = await request.post('/api/v1/assets', {
    data: {
      title: `${TOKEN} asset ${mature ? 'mature' : 'plain'}`,
      asset_type: 2,
      file_hash: hash,
      file_extension: 'txt',
      mature,
    },
  });
  expect(r.status(), `create asset → ${r.status()} ${await r.text()}`).toBe(201);
  const asset = (await r.json()) as { id: string; mature?: boolean };
  expect(asset.mature ?? false, 'the flag must round-trip').toBe(mature);
  assetIds.push(asset.id);
  return asset.id;
}

async function makePost(request: APIRequestContext, label: string, member: string) {
  const r = await request.post('/api/v1/posts', {
    data: {
      title: `${TOKEN} ${label}`,
      visibility: 'public',
      members: [{ asset_id: member, sort_order: 0 }],
    },
  });
  expect(r.status(), `create post → ${r.status()} ${await r.text()}`).toBe(201);
  return ((await r.json()) as { id: string }).id;
}

/** The post ids the WALL is rendering, off each card's own permalink.
 *  Reading the DOM rather than re-fetching is the point: the bug this
 *  catches is a control whose value never reaches a request, or a
 *  request whose result never reaches the wall. */
async function wallIds(page: Page): Promise<string[]> {
  return page.evaluate(() =>
    [
      ...new Set(
        Array.from(document.querySelectorAll('[data-testid="browse-wall"] a[href^="/posts/"]')).map(
          (a) => (a.getAttribute('href') ?? '').split('/posts/')[1].split(/[?#]/)[0],
        ),
      ),
    ].filter(Boolean),
  );
}

function matureRow(page: Page) {
  return page.locator(`${tid('kind-filter-panel')} ${tid('mature-filter-toggle')}`);
}

function aiRow(page: Page) {
  return page.locator(`${tid('kind-filter-panel')} ${tid('ai-filter-toggle')}`);
}

/** Reveal the auto-hiding footer bar. */
async function revealBar(page: Page) {
  const vp = page.viewportSize() ?? { width: 1280, height: 720 };
  await page.mouse.move(vp.width / 2, vp.height - 8);
  await expect(page.locator(tid('kind-filter-toggle'))).toBeVisible();
}

async function openPanel(page: Page) {
  await revealBar(page);
  await page.locator(tid('kind-filter-toggle')).click();
  await expect(page.locator(tid('kind-filter-panel'))).toBeVisible();
}

/** Open the menu, set the Mature row, Apply.
 *
 *  ⚠️ THE ARGUMENT IS "EXCLUDE" AND THE TICK IS ITS INVERSE, because
 *  every tick in this menu means SHOW since #1292. Apply is what
 *  commits, for this row exactly as for the type checkboxes. */
async function setExcludeMature(page: Page, exclude: boolean) {
  await openPanel(page);
  const row = matureRow(page);
  if ((await row.isChecked()) === exclude) await row.click();
  await expect(row).toBeChecked({ checked: !exclude });
  await page.locator(tid('kind-filter-apply')).click();
  await expect(page.locator(tid('kind-filter-panel'))).toBeHidden();
}

async function gotoFixtureWall(page: Page) {
  await page.goto(`/?q=${TOKEN}`);
  await expect(page.locator(tid('browse-wall'))).toBeVisible();
  await expect
    .poll(async () => (await wallIds(page)).length, {
      message: 'the fixture wall never rendered its posts',
    })
    .toBeGreaterThan(0);
}

test.describe('#1292 browse filter menu: the Mature row', () => {
  test.describe.configure({ mode: 'serial' });
  // The instance switch is taken for the whole file, and one case waits
  // on the lock behind whoever else wants it.
  test.setTimeout(600_000);

  test.beforeAll(async ({ request }) => {
    await loginAsAdminViaAPI(request);

    priorOptIn =
      ((await readPrefs(request)).mature_content as { show?: boolean } | undefined)?.show ?? false;

    // The instance has to allow mature content before an asset can
    // carry the flag: publication is REFUSED, not silently ignored,
    // while the switch is off.
    await matureSwitch.acquire(request);
    await matureSwitch.set(request, true);

    matureId = await makePost(request, 'mature', await makeAsset(request, true));
    plainId = await makePost(request, 'plain', await makeAsset(request, false));

    // ⭐ THE FIXTURE PROVES ITS OWN PREMISE, AND IT HAS TO DO IT ON THE
    // WIRE. `posts.mature` is DERIVED by a trigger from the members
    // (ADR 0090 §4), and if it did not fire the mature post sits at the
    // column default, where "the filter removed it" FAILS and every
    // other case here still passes. That reads as a filter bug and is
    // not one.
    //
    // ⚠️ THE DERIVED POST FLAG IS NOT ON THE WIRE. `Post` carries no
    // `mature` member, so this cannot read the column back the way the
    // AI fixture reads `ai_provenance`. The direct assertion lives in
    // the Go suite, which reads `posts.mature` straight out of the
    // database (posts/mature_view_filter_test.go, newMVFCorpus); here
    // the premise is stated BEHAVIOURALLY, one API call, before any UI
    // is driven. It separates the two causes the way a premise check
    // has to: a trigger that never fired returns BOTH posts and says
    // so, rather than surfacing four cases later as an inexplicable UI
    // failure.
    const filtered = await request.get(
      `/api/v1/posts?limit=200&q=${encodeURIComponent(TOKEN)}&mature=not_mature`,
    );
    expect(filtered.status()).toBe(200);
    const ids = ((await filtered.json()) as { items: { id: string }[] }).items.map((i) => i.id);
    expect(
      ids,
      'the mature fixture is still returned under ?mature=not_mature, which means the ' +
        'post_assets trigger did not derive posts.mature: every case below would be ' +
        'asserting about the wrong corpus',
    ).not.toContain(matureId);
    expect(ids, 'the plain fixture must be there').toContain(plainId);
  });

  test.afterAll(async ({ request }) => {
    if (priorOptIn !== undefined) await setOptIn(request, priorOptIn).catch(() => undefined);
    for (const id of [matureId, plainId]) {
      if (id) await request.delete(`/api/v1/posts/${id}`).catch(() => undefined);
    }
    for (const id of assetIds) {
      await request.delete(`/api/v1/assets/${id}`).catch(() => undefined);
    }
    // Backstop behind the per-test release: idempotent, so it writes
    // nothing when the switch was already given back.
    await matureSwitch.release(request);
  });

  // -------------------------------------------------------------------
  // The cascade, both rungs, as ABSENCE
  // -------------------------------------------------------------------

  test('⛔ no row for a SIGNED-OUT reader, and the AI row is unaffected', async ({ browser }) => {
    // An anonymous viewer can never opt in, because there is nowhere to
    // store the answer (ADR 0090 §2), so there is no consent for a view
    // filter to narrow. The AI row beside it is a filter that never
    // gates (ADR 0094 §4), so everybody gets that one.
    const ctx = await browser.newContext({ storageState: LOGGED_OUT });
    try {
      const page = await ctx.newPage();
      await page.goto('/');
      await expect(page.locator(tid('browse-wall'))).toBeVisible();
      await openPanel(page);
      await expect(matureRow(page), 'absent, never disabled').toHaveCount(0);
      await expect(aiRow(page), 'the AI row has no cascade').toBeVisible();
    } finally {
      await ctx.close();
    }
  });

  test('⛔ no row for a reader who has NOT opted in', async ({ page, request }) => {
    // Rung 2. A control meaning "leave mature out of these results" is
    // meaningless to a reader who was never going to be shown any: it
    // could only ever do nothing, and a tickable box that does nothing
    // is the failure this cascade exists to make unreachable.
    await setOptIn(request, false);
    await gotoFixtureWall(page);
    await openPanel(page);
    await expect(matureRow(page)).toHaveCount(0);
    await expect(aiRow(page)).toBeVisible();
  });

  test('⛔ no row when the INSTANCE forbids mature content', async ({ page, request }) => {
    // Rung 1, and it outranks the account: the operator's answer is
    // about the install. Opted IN for this case, so the ONLY thing that
    // can remove the row is the instance switch.
    await setOptIn(request, true);
    await matureSwitch.set(request, false);
    try {
      await gotoFixtureWall(page);
      await openPanel(page);
      await expect(matureRow(page), 'the row must vanish with the feature').toHaveCount(0);
      await expect(aiRow(page)).toBeVisible();
    } finally {
      await matureSwitch.set(request, true);
    }
  });

  // -------------------------------------------------------------------
  // ⭐ The control
  // -------------------------------------------------------------------

  test('⭐ the row rests TICKED and changes nothing for a reader who opted in', async ({
    page,
    request,
  }) => {
    // The upgrade case. Layer 3 defaults to INCLUDED, so a reader who
    // had already consented sees exactly the wall they saw before this
    // shipped, and the closed button says nothing is narrowing it.
    await setOptIn(request, true);
    await gotoFixtureWall(page);

    expect((await wallIds(page)).sort()).toEqual([matureId, plainId].sort());

    await revealBar(page);
    await expect(page.locator(tid('kind-filter-toggle'))).not.toHaveAttribute('data-active', 'true');
    await expect(page.locator(tid('mature-filter-active'))).toHaveCount(0);

    await openPanel(page);
    await expect(matureRow(page), 'included is the default').toBeChecked();
  });

  test('⭐ unticking it removes the mature post and leaves the plain one', async ({
    page,
    request,
  }) => {
    await setOptIn(request, true);
    await gotoFixtureWall(page);

    // ⚠️ DRAFTED, NOT COMMITTED. Unticking must change nothing until
    // Apply, which is the panel's one promise for every control in it.
    await openPanel(page);
    await matureRow(page).click();
    await expect(matureRow(page)).not.toBeChecked();
    expect(
      (await wallIds(page)).sort(),
      "the row committed without Apply, which breaks the panel's contract",
    ).toEqual([matureId, plainId].sort());

    await page.locator(tid('kind-filter-apply')).click();
    await expect(page.locator(tid('kind-filter-panel'))).toBeHidden();

    await expect
      .poll(async () => (await wallIds(page)).sort().join(','), {
        message: 'the wall never re-fetched after Apply',
      })
      .toBe([plainId].join(','));

    // ⭐ Stated separately because it is the difference between a filter
    // and an outage.
    expect(await wallIds(page), 'the plain post must SURVIVE').toContain(plainId);

    // THE BUTTON CARRIES THE STATE. On most walls a mature-free wall IS
    // the wall you would have got, so the button is the only place this
    // state can show.
    await expect(page.locator(tid('kind-filter-toggle'))).toHaveAttribute('data-active', 'true');
    await expect(page.locator(tid('mature-filter-active'))).toHaveCount(1);

    // And it does not reach the URL: a link that narrowed somebody
    // else's wall would impose a preference under cover of sharing.
    expect(page.url(), 'the view filter must NOT be written to the URL').not.toContain('mature=');

    // Back on, in place.
    await setExcludeMature(page, false);
    await expect
      .poll(async () => (await wallIds(page)).sort().join(','))
      .toBe([matureId, plainId].sort().join(','));
  });

  test('it survives a reload, and keeps filtering', async ({ page, request }) => {
    await setOptIn(request, true);
    await gotoFixtureWall(page);
    await setExcludeMature(page, true);
    await expect.poll(async () => (await wallIds(page)).includes(matureId)).toBe(false);

    await gotoFixtureWall(page);
    await revealBar(page);

    // THREE halves, because each can fail without the others. A row
    // that comes back narrowed but sends nothing is worse than one that
    // resets: it states a filter is applied and it is not.
    await expect(page.locator(tid('mature-filter-active')), 'the button lost the state')
      .toHaveCount(1);
    await expect
      .poll(async () => (await wallIds(page)).sort().join(','), {
        message: 'the row came back narrowed but the wall is unfiltered',
      })
      .toBe([plainId].join(','));
    await openPanel(page);
    await expect(matureRow(page), 'the menu lost the state').not.toBeChecked();
    await page.keyboard.press('Escape');

    // Leave the device clean: this is stored state, and a spec that
    // changes what the next one sees is not isolated.
    await setExcludeMature(page, false);
  });

  // -------------------------------------------------------------------
  // ⛔ It never widens
  // -------------------------------------------------------------------

  test('⛔ a stored flag does NOTHING once the cascade withdraws the row', async ({
    page,
    request,
  }) => {
    // The device keeps the flag; the reader loses the consent. The
    // filter must then be as if it were never set, and this asserts it
    // in the only way that cannot go vacuous: the SAME wall, with the
    // stored flag both ways, and no `mature=` on any request.
    //
    // ⚠️ THE READER HERE IS AN ADMIN, and that is why this case asserts
    // an EQUALITY rather than "the mature post is gone". `system.admin`
    // is EXEMPT from the mature gate (ADR 0090 §2, so a moderator can
    // see what the instance switch hid), so an opted-out admin is still
    // shown mature rows. The first draft of this case asserted the post
    // was absent and failed against the shipped exemption, which is the
    // single-arm test passing on a premise nobody checked.
    //
    // ⛔ WHICH LEAVES A REAL GAP, recorded rather than pinned: an admin
    // who has not opted in is shown mature work and is offered no row
    // to filter it, because the cascade asks about CONSENT and the gate
    // exempts them from needing any. Widening the row to them is a
    // product decision about a case ADR 0090's amendment does not name.
    // This case therefore asserts only what IS decided: the view filter
    // cannot act without the row, in either direction.
    await setOptIn(request, true);
    await gotoFixtureWall(page);
    await setExcludeMature(page, true);

    await setOptIn(request, false);
    const walls: string[] = [];
    for (const stored of ['1', null]) {
      const sent: string[] = [];
      page.on('request', (r) => {
        if (r.url().includes('/api/v1/posts?')) sent.push(r.url());
      });
      await page.evaluate((v) => {
        if (v === null) localStorage.removeItem('aa_browse_hide_mature');
        else localStorage.setItem('aa_browse_hide_mature', v);
      }, stored);
      await gotoFixtureWall(page);

      expect(
        sent.filter((u) => u.includes('mature=')),
        `stored flag ${JSON.stringify(stored)}: the request must not carry a filter the ` +
          'menu is no longer offering a control for',
      ).toEqual([]);

      await openPanel(page);
      await expect(matureRow(page), 'and there is no row to act through').toHaveCount(0);
      await page.keyboard.press('Escape');

      walls.push((await wallIds(page)).sort().join(','));
      page.removeAllListeners('request');
    }

    expect(
      walls[0],
      'the stored flag changed the wall for a reader the cascade no longer offers the row to',
    ).toBe(walls[1]);

    await page.evaluate(() => localStorage.removeItem('aa_browse_hide_mature'));
  });

  // -------------------------------------------------------------------
  // Both rows compose
  // -------------------------------------------------------------------

  test('the two content rows narrow each other', async ({ page, request }) => {
    await setOptIn(request, true);
    await gotoFixtureWall(page);

    await openPanel(page);
    await aiRow(page).click();
    await matureRow(page).click();
    await page.locator(tid('kind-filter-apply')).click();

    // Neither fixture is purely AI, so the AI conjunct keeps both and
    // the mature one removes exactly the mature post. The point is that
    // the two ANDed rather than one replacing the other.
    await expect
      .poll(async () => (await wallIds(page)).sort().join(','), {
        message: 'the two content axes did not AND',
      })
      .toBe([plainId].join(','));

    await expect(page.locator(tid('ai-filter-active'))).toHaveCount(1);
    await expect(page.locator(tid('mature-filter-active'))).toHaveCount(1);
    expect(page.url()).not.toContain('mature=');
    expect(page.url()).not.toContain('ai=');

    await setExcludeMature(page, false);
    await openPanel(page);
    await aiRow(page).click();
    await page.locator(tid('kind-filter-apply')).click();
    await expect
      .poll(async () => (await wallIds(page)).sort().join(','))
      .toBe([matureId, plainId].sort().join(','));
  });

  // -------------------------------------------------------------------
  // Mobile
  // -------------------------------------------------------------------

  test('the row is reachable and works at 390px', async ({ page, request }) => {
    await setOptIn(request, true);
    await page.setViewportSize({ width: 390, height: 844 });
    await gotoFixtureWall(page);
    await openPanel(page);

    // ⭐ REACHABLE, NOT MERELY PRESENT. The panel is a scroll box with
    // eleven type rows and the AI row above this one, capped at 60vh,
    // which on an 844px phone is ~506px: less than the rows need. This
    // is now the LAST row, so "renders in the DOM" says nothing about
    // whether a thumb can get to it.
    await matureRow(page).scrollIntoViewIfNeeded();
    await expect(matureRow(page)).toBeVisible();
    const hit = await matureRow(page).evaluate(
      (el) => el.closest('label')!.getBoundingClientRect().height,
    );
    expect(hit, 'the row must keep the 44px touch target the type rows have')
      .toBeGreaterThanOrEqual(44);

    await matureRow(page).click();
    await page.locator(tid('kind-filter-apply')).click();
    await expect
      .poll(async () => (await wallIds(page)).sort().join(','))
      .toBe([plainId].join(','));

    await setExcludeMature(page, false);
  });
});
