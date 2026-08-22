// ai-toggle-1251.spec.ts
//
// "Hide AI-made work" (#1251 slice 3, ADR 0094 fourth amendment) — the
// switch INSIDE #1166's asset-type filter menu that sends
// `?ai=not_pure`.
//
// # Where it lives is part of what is being tested
//
// It shipped for review as its own footer button and the owner sent it
// back twice: "that shouldn't be its own footer item", then "should be
// mixed in the asset type filter". So the footer's right cluster carries
// the same two controls it carried before this sprint, and the switch is
// the last row of the type menu, under a divider and a `Hide` heading.
//
// `it lives INSIDE the type filter menu, not beside it` is the guard,
// and it fails on the earlier placement in both directions: the switch
// must be absent from the page while the menu is closed, and the right
// cluster must hold exactly two buttons. Every other case reaches the
// switch by opening the menu, so the whole file fails if it moves back
// out.
//
// # One menu is not one persistence model
//
// The types go to the URL and the switch goes to localStorage, and that
// split is signed off rather than pending cleanup — see
// `the two axes narrow each other and persist in different places`,
// which pins both halves so a future "unification" has to delete an
// assertion that says why.
//
// # And one menu IS one commit
//
// The switch is drafted and committed by the panel's own Apply, like
// every checkbox above it. `dismissing the menu throws the hide draft
// away` and the mid-case assertion in the ruling test are what hold
// that: a control that committed live inside a panel with an Apply
// button would break that button's only promise for everything beside
// it.
//
// # The one assertion that actually proves the feature
//
// THE MIXED POST STAYS. "Turning it on removes the AI post" is one line
// and it is not where this goes wrong. The owner's ruling is a
// distinction:
//
//   > If someone filters out AI content it should still show a post that
//   > has mixed AI/non-AI content — only exclude posts with pure AI. AI
//   > could be used as part of an ideation phase and the final project
//   > might be pure human made.
//
// So the fixture is a PAIR that a wrong implementation cannot tell
// apart — one post whose every contributor declares `generated`, one
// with a `generated` member beside an undeclared one — and every case
// below reads BOTH. A filter keyed on `posts.ai_provenance` (the
// LABELLING column, and the one that existed first) hides both, and
// passes any test that only looks at the pure one.
//
// The Go suite asserts the same rule at the query layer
// (posts/ai_filter_test.go). This file asserts it through the CONTROL,
// because a correct predicate behind a toggle that sends the wrong value
// — or that sends nothing, or that the wall never re-fetches for — is
// indistinguishable from a working feature on a corpus where nothing is
// declared. Which is every corpus this project has by default: the
// toggle is INVISIBLE until something is declared, so a spec that
// planted no fixtures would go green against a control wired to nothing.
//
// # It is a FILTER, never a GATE (ADR 0094 §4)
//
// Nothing is withheld on this axis. `a filter, never a gate` below is
// the case that keeps the column cheap: a caller who does not ask still
// sees the pure post, signed in and anonymous alike. The moment
// something subtracts on it, every derived copy inherits #1066's
// obligation.
//
// # ⛔ No badges here
//
// This spec never asserts an AI label on a card. Labelling and filtering
// are different facts (ADR 0094 fifth amendment) and the per-asset label
// is #1243 — a badge assertion here would pin a surface that has not
// been designed.

import { test, expect, type Page } from '../../helpers/test';
import type { APIRequestContext } from '@playwright/test';
import { loginAsAdminViaAPI, LOGGED_OUT } from '../../helpers/auth';
import { tid } from '../../helpers/testids';

/** A token that appears in every fixture title and nowhere in the seed,
 *  so `/?q=<TOKEN>` renders a wall of exactly these three posts. Reading
 *  a three-card wall is what makes "the pure one went and the mixed one
 *  stayed" a statement about the filter rather than about scroll depth
 *  on a wall of 851. */
const TOKEN = `aitoggle${Date.now()}`;

let pureId = '';
let mixedId = '';
let plainId = '';
const assetIds: string[] = [];
let priorPublicMode: boolean | undefined;

async function makeAsset(request: APIRequestContext, decl: string | null): Promise<string> {
  const up = await request.post('/api/v1/storage/objects', {
    // Unique bytes per asset. Byte-identical uploads by one owner are
    // COLLAPSED by the content-address unique index, and a collapsed
    // asset would silently become a member of two fixture posts —
    // declaring it would then move both, which is precisely the blast
    // radius this fixture is built to keep at one post each.
    data: Buffer.from(`${TOKEN} ${decl ?? 'undeclared'} ${Math.random()}`),
    headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'text/plain' },
  });
  expect(up.status()).toBe(201);
  const { hash } = (await up.json()) as { hash: string };

  const r = await request.post('/api/v1/assets', {
    data: {
      title: `${TOKEN} asset ${decl ?? 'undeclared'}`,
      asset_type: 2,
      file_hash: hash,
      file_extension: 'txt',
      // ⚠️ OMITTED, not sent as null, for an undeclared asset. Null is
      // "leave alone" on every other property of this schema and there
      // is no value meaning "nobody was asked" — the absence IS the
      // value (ADR 0094, #1167).
      ...(decl ? { ai_provenance: decl } : {}),
    },
  });
  expect(r.status(), `create asset → ${r.status()} ${await r.text()}`).toBe(201);
  const asset = (await r.json()) as { id: string; ai_provenance?: string | null };
  expect(asset.ai_provenance ?? null, 'the declaration must round-trip').toBe(decl);
  assetIds.push(asset.id);
  return asset.id;
}

async function makePost(request: APIRequestContext, label: string, members: string[]) {
  const r = await request.post('/api/v1/posts', {
    data: {
      title: `${TOKEN} ${label}`,
      visibility: 'public',
      members: members.map((id, i) => ({ asset_id: id, sort_order: i })),
    },
  });
  expect(r.status(), `create post → ${r.status()} ${await r.text()}`).toBe(201);
  return ((await r.json()) as { id: string }).id;
}

/** The post ids `GET /posts` returns for one `?ai=` value, restricted to
 *  the fixture's own wall by `?q=`. */
async function feedIds(page: Page, ai: string): Promise<string[]> {
  return page.evaluate(
    async ([token, aiValue]: string[]) => {
      let u = `/api/v1/posts?limit=200&q=${encodeURIComponent(token)}`;
      if (aiValue) u += `&ai=${encodeURIComponent(aiValue)}`;
      const d = await (await fetch(u)).json();
      return (d.items ?? []).map((p: { id: string }) => p.id);
    },
    [TOKEN, ai],
  );
}

/** The post ids the WALL is currently rendering, off each card's own
 *  permalink. Reading the DOM rather than re-fetching is the point: the
 *  bug this catches is a control whose value never reaches a request, or
 *  a request whose result never reaches the wall. */
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

/** The hide switch — INSIDE the type-filter panel, which is the whole
 *  point of the placement and the reason every case has to open the menu
 *  first. Scoped to the panel rather than located globally so a stray
 *  copy of this id anywhere else on the page cannot satisfy it. */
function aiToggle(page: Page) {
  return page.locator(`${tid('kind-filter-panel')} ${tid('ai-filter-toggle')}`);
}

/** Reveal the auto-hiding footer bar. */
async function revealBar(page: Page) {
  const vp = page.viewportSize() ?? { width: 1280, height: 720 };
  await page.mouse.move(vp.width / 2, vp.height - 8);
  await expect(page.locator(tid('kind-filter-toggle'))).toBeVisible();
}

/** Reveal the bar, then open the one menu that holds both filters. */
async function openPanel(page: Page) {
  await revealBar(page);
  await page.locator(tid('kind-filter-toggle')).click();
  await expect(page.locator(tid('kind-filter-panel'))).toBeVisible();
}

/** Open the menu, set the switch, Apply.
 *
 *  ⚠️ APPLY IS WHAT COMMITS, for this switch exactly as for the type
 *  checkboxes — one panel, one commit. A test that flipped the switch
 *  and then asserted on the wall without applying would be asserting
 *  that the panel BROKE its own contract. */
async function setHide(page: Page, on: boolean) {
  await openPanel(page);
  const sw = aiToggle(page);
  if ((await sw.isChecked()) !== on) await sw.click();
  await expect(sw).toBeChecked({ checked: on });
  await page.locator(tid('kind-filter-apply')).click();
  await expect(page.locator(tid('kind-filter-panel'))).toBeHidden();
}

async function gotoFixtureWall(page: Page) {
  await page.goto(`/?q=${TOKEN}`);
  await expect(page.locator(tid('browse-wall'))).toBeVisible();
  await expect
    .poll(async () => (await wallIds(page)).length, {
      message: 'the fixture wall never rendered its three posts',
    })
    .toBeGreaterThan(0);
}

test.describe('#1251 browse footer — hide AI-made work', () => {
  test.describe.configure({ mode: 'serial' });

  test.beforeAll(async ({ request }) => {
    await loginAsAdminViaAPI(request);

    const mode = await request.get('/api/v1/admin/system/public-mode');
    expect(mode.status(), 'public-mode state must be readable as admin').toBe(200);
    priorPublicMode = ((await mode.json()) as { enabled: boolean }).enabled;

    const gen1 = await makeAsset(request, 'generated');
    const gen2 = await makeAsset(request, 'generated');
    const undeclared1 = await makeAsset(request, null);
    const undeclared2 = await makeAsset(request, null);

    // PURE: every contributor declares `generated`.
    pureId = await makePost(request, 'pure', [gen1]);
    // MIXED: one declared, one nobody was asked about. `ai_provenance`
    // reads `generated` here TOO — which is exactly why a filter on that
    // column cannot be told apart from a correct one by looking at the
    // pure post alone.
    mixedId = await makePost(request, 'mixed', [gen2, undeclared1]);
    // The shape of almost the whole real corpus.
    plainId = await makePost(request, 'plain', [undeclared2]);

    // ⭐ THE FIXTURE PROVES ITS OWN PREMISE before any case runs. Both
    // derived facts come from database triggers, and if they did not
    // fire the pure post sits at `ai_pure = false` — where "the pure
    // post is hidden" FAILS and every other assertion here still passes.
    // That reads as a filter bug and is not one.
    for (const [label, id, prov] of [
      ['pure', pureId, 'generated'],
      ['mixed', mixedId, 'generated'],
      ['plain', plainId, null],
    ] as const) {
      const r = await request.get(`/api/v1/posts/${id}`);
      expect(r.status()).toBe(200);
      const got = (await r.json()) as { ai_provenance?: string | null };
      expect(got.ai_provenance ?? null, `${label} post's LABEL`).toBe(prov);
    }
  });

  test.afterAll(async ({ request }) => {
    if (priorPublicMode !== undefined) {
      await request
        .patch('/api/v1/admin/system/public-mode', { data: { enabled: priorPublicMode } })
        .catch(() => undefined);
    }
    // Posts first, then their assets. A post delete leaves its members
    // standing, and leftovers pile up at the top of every newest-first
    // grid where the NEXT spec reads (#1198).
    for (const id of [pureId, mixedId, plainId]) {
      if (id) await request.delete(`/api/v1/posts/${id}`).catch(() => undefined);
    }
    for (const id of assetIds) {
      await request.delete(`/api/v1/assets/${id}`).catch(() => undefined);
    }
  });

  // -------------------------------------------------------------------
  // The wire, before the control
  // -------------------------------------------------------------------

  test('the wire partitions: not_pure is the wall minus the pure post', async ({ page }) => {
    await gotoFixtureWall(page);

    const all = await feedIds(page, '');
    const notPure = await feedIds(page, 'not_pure');
    const pure = await feedIds(page, 'pure');

    // The subtraction, written out: 3 = 1 + 2.
    expect(all.sort()).toEqual([pureId, mixedId, plainId].sort());
    expect(pure).toEqual([pureId]);
    expect(notPure.sort()).toEqual([mixedId, plainId].sort());
    expect(pure.length + notPure.length).toBe(all.length);
  });

  test('an unknown value is refused rather than answered with an empty wall', async ({
    page,
  }) => {
    await gotoFixtureWall(page);

    // ⚠️ THE ONE FILTER ON THIS OPERATION THAT ANSWERS A TYPO WITH AN
    // ERROR. `?kind=nonsense` and `?visibility=nonsense` both return an
    // empty page — they are positive selections, where "only X" for an X
    // nobody has is legibly answered by no rows. This is an EXCLUSION
    // over a closed two-value vocabulary, so a tolerated `?ai=generated`
    // would render a predicate matching nothing and hand a reader who
    // asked to hide AI work a blank wall, silently. Same answer /search
    // gives `filter=ai:junk`.
    for (const bad of ['junk', 'generated', 'true']) {
      const status = await page.evaluate(
        async ([token, v]: string[]) =>
          (await fetch(`/api/v1/posts?q=${encodeURIComponent(token)}&ai=${encodeURIComponent(v)}`))
            .status,
        [TOKEN, bad],
      );
      expect(status, `?ai=${bad} must be refused, not answered with an empty page`).toBe(400);
    }
  });

  // -------------------------------------------------------------------
  // ⭐ The control, and the owner's ruling on a rendered wall
  // -------------------------------------------------------------------

  // ⭐ THE PLACEMENT IS THE REGRESSION GUARD.
  //
  // The control shipped for review as its own footer button and the
  // owner sent it back twice — "that shouldn't be its own footer item",
  // then "should be mixed in the asset type filter". This case fails on
  // that earlier placement in both directions, which is what makes it a
  // guard rather than a description: the switch must NOT be reachable
  // with the menu closed, and the right cluster must hold the same two
  // controls it held before this sprint.
  test('it lives INSIDE the type filter menu, not beside it', async ({ page }) => {
    await gotoFixtureWall(page);
    await revealBar(page);

    // Closed menu: the switch is nowhere on the page.
    await expect(page.locator(tid('ai-filter-toggle'))).toHaveCount(0);

    // And the footer's right cluster is the type button + the sort
    // toggle, exactly as before #1251. Counted off the bar itself so a
    // third control anywhere in it fails here rather than in a
    // screenshot review.
    const clusterButtons = await page.evaluate(() => {
      const bar = document.querySelector('[data-testid="view-controls"]');
      if (!bar) return -1;
      const cluster = bar.querySelector('.ml-auto');
      return cluster ? cluster.querySelectorAll('button').length : -1;
    });
    expect(
      clusterButtons,
      'the right cluster must carry the type-filter button and the sort toggle and ' +
        'nothing else — the AI control belongs in the menu, not beside it',
    ).toBe(2);

    // Open it and the switch is there, under the type checkboxes.
    await page.locator(tid('kind-filter-toggle')).click();
    await expect(page.locator(tid('kind-filter-panel'))).toBeVisible();
    await expect(aiToggle(page)).toBeVisible();
    await expect(aiToggle(page)).toHaveAttribute('role', 'switch');
  });

  test('flipping it removes the PURE post and leaves the MIXED one', async ({ page }) => {
    await gotoFixtureWall(page);
    await revealBar(page);

    // Resting state: off, nothing hidden, and the closed button says so.
    await expect(page.locator(tid('kind-filter-toggle')))
      .not.toHaveAttribute('data-active', 'true');
    await expect(page.locator(tid('ai-filter-active'))).toHaveCount(0);
    expect((await wallIds(page)).sort()).toEqual([pureId, mixedId, plainId].sort());

    // ⚠️ DRAFTED, NOT COMMITTED. Ticking the switch must change
    // NOTHING until Apply — the panel's one promise is that nothing you
    // have touched has happened yet, and a control that committed live
    // would break it for every checkbox beside it.
    await openPanel(page);
    await aiToggle(page).click();
    await expect(aiToggle(page)).toBeChecked();
    expect(
      (await wallIds(page)).sort(),
      'the switch committed without Apply — that breaks the panel\'s contract',
    ).toEqual([pureId, mixedId, plainId].sort());

    await page.locator(tid('kind-filter-apply')).click();
    await expect(page.locator(tid('kind-filter-panel'))).toBeHidden();

    // THE BUTTON CARRIES THE STATE — a filtered wall that looks like an
    // unfiltered one is how people conclude the site is broken, and on
    // this axis most walls DO look identical either way.
    await expect(page.locator(tid('kind-filter-toggle')))
      .toHaveAttribute('data-active', 'true');
    await expect(page.locator(tid('ai-filter-active'))).toHaveCount(1);

    await expect
      .poll(async () => (await wallIds(page)).sort().join(','), {
        message: 'the wall never re-fetched after Apply',
      })
      .toBe([mixedId, plainId].sort().join(','));

    // ⭐ Stated as its own assertion because it is the ruling, not a
    // side effect of the counts above.
    const shown = await wallIds(page);
    expect(shown, 'the MIXED post must survive — excluding it would punish the honest declaration')
      .toContain(mixedId);
    expect(shown, 'the PURE post is the only thing this hides').not.toContain(pureId);

    // And back off restores it, in place.
    await setHide(page, false);
    await expect(page.locator(tid('ai-filter-active'))).toHaveCount(0);
    await expect
      .poll(async () => (await wallIds(page)).sort().join(','))
      .toBe([pureId, mixedId, plainId].sort().join(','));
  });

  test('dismissing the menu throws the hide draft away, like every other draft in it', async ({
    page,
  }) => {
    await gotoFixtureWall(page);
    await openPanel(page);
    await aiToggle(page).click();
    await expect(aiToggle(page)).toBeChecked();

    // Escape is the panel's own light dismiss.
    await page.keyboard.press('Escape');
    await expect(page.locator(tid('kind-filter-panel'))).toBeHidden();

    expect(
      (await wallIds(page)).sort(),
      'a dismissed draft must not commit — the Cancel-less panel\'s existing contract',
    ).toEqual([pureId, mixedId, plainId].sort());

    // Re-opening shows the APPLIED state, not the abandoned draft.
    await openPanel(page);
    await expect(aiToggle(page)).not.toBeChecked();
    await page.keyboard.press('Escape');
  });

  test('the two axes narrow each other and persist in different places', async ({ page }) => {
    // ⚠️ ONE MENU IS NOT ONE PERSISTENCE MODEL, and this is the case
    // that pins it. Types go to the URL because a type-filtered wall is
    // shareable; the hide switch goes to localStorage because it
    // describes the reader. Sharing a popover changed neither, and a
    // future "unification" fails here.
    await gotoFixtureWall(page);

    await setHide(page, true);
    expect(page.url(), 'the hide switch must NOT be written to the URL').not.toContain('ai=');

    await openPanel(page);
    // ⚠️ "All types" is a PLAIN CHECKBOX, so reaching one type is
    // "ensure it is checked, then ONE click to clear the board" — two
    // clicks from the resting all-checked state land back on
    // all-checked, and the next tick then UNCHECKS a type instead of
    // selecting it. That is kind-filter-1166's `pickOnly` helper's whole
    // reason for existing, and this case walked into it first time
    // round: the URL came back with the twelve kinds that are not `doc`.
    const all = page.locator(tid('kind-filter-all'));
    if (!(await all.isChecked())) await all.click();
    await all.click();
    await page.locator(`${tid('kind-filter-option')}[data-kind="doc"]`).click();
    await page.locator(tid('kind-filter-apply')).click();

    // The type axis DID reach the URL.
    await expect(page).toHaveURL(/kind=doc/);
    // ...and still no `ai=` in it.
    expect(page.url()).not.toContain('ai=');

    // Both applied: the fixtures are all `doc`, so the type filter keeps
    // them and the hide switch still removes exactly the pure one.
    await expect
      .poll(async () => (await wallIds(page)).sort().join(','), {
        message: 'the two axes did not AND',
      })
      .toBe([mixedId, plainId].sort().join(','));

    await setHide(page, false);
  });

  test('it survives a reload, and keeps filtering', async ({ page }) => {
    await gotoFixtureWall(page);
    await setHide(page, true);
    await expect.poll(async () => (await wallIds(page)).includes(pureId)).toBe(false);

    await gotoFixtureWall(page);
    await revealBar(page);

    // THREE halves, because each can fail without the others. A switch
    // that comes back on but sends nothing is worse than one that
    // resets: it states a filter is applied and it is not.
    await expect(page.locator(tid('ai-filter-active')), 'the closed button lost the state')
      .toHaveCount(1);
    await expect
      .poll(async () => (await wallIds(page)).sort().join(','), {
        message: 'the switch came back ON but the wall is unfiltered',
      })
      .toBe([mixedId, plainId].sort().join(','));
    await openPanel(page);
    await expect(aiToggle(page), 'the menu lost the state').toBeChecked();
    await page.keyboard.press('Escape');

    // Leave the device clean for the next case — this is stored state,
    // and a spec that changes what the next spec sees is not isolated.
    await setHide(page, false);
  });

  test('it is a filter, never a gate — nothing is withheld from a caller who did not ask', async ({
    page,
    browser,
    request,
  }) => {
    // Signed in, no parameter: the pure post is there, in full. This
    // half needs no instance config at all, so it runs first and
    // outside the window below.
    await gotoFixtureWall(page);
    expect(await feedIds(page, '')).toContain(pureId);

    // ⚠️ `public_mode` IS SHARED INSTANCE CONFIG, and this spec is now a
    // second writer of it — `collection-public-tier-1195` is the other,
    // and #1248 names exactly that contention as the thing to rule in or
    // out before calling a rotating failure a timing flake.
    //
    // So the window is as narrow as the assertion allows: set it, assert
    // anonymously, put it straight back in a `finally` — rather than
    // holding it for the file the way a beforeAll/afterAll pair would.
    // The prior value is captured once in beforeAll and restored again in
    // afterAll as a backstop, because a crash inside this block would
    // otherwise leave the instance switched.
    const r = await request.patch('/api/v1/admin/system/public-mode', { data: { enabled: true } });
    expect(r.status(), 'public mode must be settable for the anonymous half').toBe(200);
    try {
      // Anonymous, no parameter: same answer. ADR 0094 §4 — the work
      // stays public, findable and countable for everyone who did not
      // ask to hide it.
      const anon = await browser.newContext({ storageState: LOGGED_OUT });
      try {
        const anonPage = await anon.newPage();
        await anonPage.goto(`/?q=${TOKEN}`);
        await expect(anonPage.locator(tid('browse-wall'))).toBeVisible();
        await expect.poll(async () => (await wallIds(anonPage)).length).toBeGreaterThan(0);
        expect(
          await wallIds(anonPage),
          'a signed-out visitor must see the pure-AI post unless they ask not to',
        ).toContain(pureId);

        // And the control is offered to them too — it is a filter, not a
        // per-account feature, so the menu carries it for a signed-out
        // reader exactly as it does for a member.
        await setHide(anonPage, true);
        await expect
          .poll(async () => (await wallIds(anonPage)).sort().join(','), {
            message: 'the switch did not filter for an anonymous reader',
          })
          .toBe([mixedId, plainId].sort().join(','));
      } finally {
        await anon.close();
      }
    } finally {
      if (priorPublicMode !== undefined) {
        await request
          .patch('/api/v1/admin/system/public-mode', { data: { enabled: priorPublicMode } })
          .catch(() => undefined);
      }
    }
  });

  // -------------------------------------------------------------------
  // Mobile
  // -------------------------------------------------------------------

  test('the switch is reachable and works at 390px', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await gotoFixtureWall(page);
    await openPanel(page);

    // ⭐ IT MUST BE REACHABLE, NOT MERELY PRESENT. The panel is a
    // scroll box holding thirteen type rows before this switch, and it
    // is capped at 60vh — which on an 844px phone is ~506px, less than
    // the rows need. So the row that matters is the one furthest down,
    // and "renders in the DOM" says nothing about whether a thumb can
    // get to it. Scrolling it into view and clicking it is the check.
    await aiToggle(page).scrollIntoViewIfNeeded();
    await expect(aiToggle(page)).toBeVisible();
    // Named, because at this width the trigger button is a bare glyph.
    await expect(page.locator(tid('kind-filter-toggle'))).toHaveAttribute('aria-label', /.+/);

    await aiToggle(page).click();
    await expect(aiToggle(page)).toBeChecked();
    await page.locator(tid('kind-filter-apply')).click();

    await expect
      .poll(async () => (await wallIds(page)).sort().join(','))
      .toBe([mixedId, plainId].sort().join(','));

    await setHide(page, false);
  });
});
