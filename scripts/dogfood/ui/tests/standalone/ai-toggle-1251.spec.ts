// ai-toggle-1251.spec.ts
//
// The browse footer's "Hide AI-made work" toggle (#1251 slice 3, ADR
// 0094 fourth amendment) — the switch beside #1166's type filter that
// sends `?ai=not_pure`.
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

/** Reveal the auto-hiding footer bar and read the toggle. */
function aiToggle(page: Page) {
  return page.locator(tid('ai-filter-toggle'));
}
async function revealBar(page: Page) {
  const vp = page.viewportSize() ?? { width: 1280, height: 720 };
  await page.mouse.move(vp.width / 2, vp.height - 8);
  await expect(aiToggle(page)).toBeVisible();
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

  test('flipping it removes the PURE post and leaves the MIXED one', async ({ page }) => {
    await gotoFixtureWall(page);
    await revealBar(page);

    // Resting state: off, nothing hidden, no request parameter.
    await expect(aiToggle(page)).toHaveAttribute('aria-pressed', 'false');
    await expect(aiToggle(page)).not.toHaveAttribute('data-active', 'true');
    expect((await wallIds(page)).sort()).toEqual([pureId, mixedId, plainId].sort());

    await aiToggle(page).click();
    await expect(aiToggle(page)).toHaveAttribute('aria-pressed', 'true');
    // THE BUTTON CARRIES THE STATE — a filtered wall that looks like an
    // unfiltered one is how people conclude the site is broken, and on
    // this axis most walls DO look identical either way.
    await expect(aiToggle(page)).toHaveAttribute('data-active', 'true');

    await expect
      .poll(async () => (await wallIds(page)).sort().join(','), {
        message: 'the wall never re-fetched after the toggle flipped',
      })
      .toBe([mixedId, plainId].sort().join(','));

    // ⭐ Stated as its own assertion because it is the ruling, not a
    // side effect of the counts above.
    const shown = await wallIds(page);
    expect(shown, 'the MIXED post must survive — excluding it would punish the honest declaration')
      .toContain(mixedId);
    expect(shown, 'the PURE post is the only thing this hides').not.toContain(pureId);

    // And back off restores it, in place.
    await aiToggle(page).click();
    await expect(aiToggle(page)).toHaveAttribute('aria-pressed', 'false');
    await expect
      .poll(async () => (await wallIds(page)).sort().join(','))
      .toBe([pureId, mixedId, plainId].sort().join(','));
  });

  test('it survives a reload, and keeps filtering', async ({ page }) => {
    await gotoFixtureWall(page);
    await revealBar(page);
    await aiToggle(page).click();
    await expect(aiToggle(page)).toHaveAttribute('aria-pressed', 'true');
    await expect.poll(async () => (await wallIds(page)).includes(pureId)).toBe(false);

    await gotoFixtureWall(page);
    await revealBar(page);

    // Both halves. A toggle that comes back pressed but sends nothing is
    // worse than one that resets: it states a filter is applied and it
    // is not.
    await expect(aiToggle(page)).toHaveAttribute('aria-pressed', 'true');
    await expect
      .poll(async () => (await wallIds(page)).sort().join(','), {
        message: 'the toggle came back ON but the wall is unfiltered',
      })
      .toBe([mixedId, plainId].sort().join(','));

    // Leave the device clean for the next case — this is stored state,
    // and a spec that changes what the next spec sees is not isolated.
    await aiToggle(page).click();
    await expect(aiToggle(page)).toHaveAttribute('aria-pressed', 'false');
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
        // per-account feature.
        await revealBar(anonPage);
        await aiToggle(anonPage).click();
        await expect
          .poll(async () => (await wallIds(anonPage)).sort().join(','), {
            message: 'the toggle did not filter for an anonymous reader',
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

  test('the toggle is reachable and works at 390px', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await gotoFixtureWall(page);
    await revealBar(page);

    // The label collapses to the glyph below `sm`, so the control has to
    // stay NAMED — an anonymous icon is not a policy-shaped choice
    // anybody can find.
    await expect(aiToggle(page)).toHaveAttribute('aria-label', /.+/);

    await aiToggle(page).click();
    await expect
      .poll(async () => (await wallIds(page)).sort().join(','))
      .toBe([mixedId, plainId].sort().join(','));

    await aiToggle(page).click();
    await expect(aiToggle(page)).toHaveAttribute('aria-pressed', 'false');
  });
});
