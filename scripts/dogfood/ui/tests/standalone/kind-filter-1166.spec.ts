// kind-filter-1166.spec.ts
//
// The browse footer's asset-type filter (#1166): the icon button beside
// Newest/Oldest that opens a checkbox list and narrows the wall to one
// or more cover kinds.
//
// # The one assertion that actually proves the feature
//
// BADGE-VS-FILTER AGREEMENT. The card's kind badge is DERIVED in the
// browser — `kindForAsset(asset_type, file_extension)` — while the
// filter is decided in SQL, so the two are independent expressions of
// one fact and the only honest check is to make them meet on a rendered
// page. Ticking "Video" and then reading every badge on the wall is
// that check, and it is what a filter keyed on the wrong column
// (`assets.asset_type`, the tempting one) fails: ref 2 is "Document"
// and the badge splits it into E-book and Document, so such a filter
// returns two badges under one label.
//
// The DOM half only sees SINGLE-asset posts — a multi-asset post draws
// the count-plus-Shapes badge instead of a kind glyph (#1111) — so the
// spec also re-derives every returned item's cover kind through the
// app's own resolver, imported from the running bundle rather than
// copied here. Between them, every item on the filtered page is
// checked.
//
// # The rest is the ratified anatomy
//
// All-checked = no filter (and therefore no query parameter), an active
// state on the button, Apply as the commit, the selection in the URL so
// a direct load reproduces it, and light dismiss on Escape and on an
// outside click. Each is a rule that inverts silently: a control that
// commits the wrong set looks exactly like one that works.
//
// Anonymous is covered by its own case because that is where a filter
// can leak — see the visibility note on `?kind=` in openapi.yaml. It
// runs only when the install has public mode on.

import { test, expect, type Page } from '../../helpers/test';
import { tid } from '../../helpers/testids';

const KIND_LABEL: Record<string, string> = {
  image: 'Image',
  video: 'Video',
  '3d': '3D model',
  ebook: 'E-book',
  audio: 'Audio',
};

/** Reveal the auto-hiding footer bar, then open the filter panel. */
async function openPanel(page: Page) {
  const vp = page.viewportSize() ?? { width: 1280, height: 720 };
  await page.mouse.move(vp.width / 2, vp.height - 8);
  await page.locator(tid('kind-filter-toggle')).click();
  await expect(page.locator(tid('kind-filter-panel'))).toBeVisible();
}

/** Reduce the draft to exactly one kind, from ANY starting state.
 *  "All types" is a plain checkbox: from a partial selection its first
 *  click CHECKS it (every type) and a second clears the board. */
async function pickOnly(page: Page, kind: string) {
  const all = page.locator(tid('kind-filter-all'));
  if (!(await all.isChecked())) await all.click();
  await all.click();
  await page.locator(`${tid('kind-filter-option')}[data-kind="${kind}"]`).click();
  await page.locator(tid('kind-filter-apply')).click();
}

/** Every kind badge currently on the wall, by accessible name. */
async function badgeLabels(page: Page): Promise<string[]> {
  return page.locator(tid('card-kind')).evaluateAll((els) =>
    els.map((e) => e.getAttribute('aria-label') ?? ''),
  );
}

/**
 * Re-derive the cover kind of every item `?kind=<kind>` returns, using
 * the app's OWN resolver. This is the half that covers multi-asset
 * posts, whose badge states the set rather than a kind.
 */
async function coverKindsFor(page: Page, kind: string) {
  return page.evaluate(async (k: string) => {
    const mod = await import('/src/lib/components/viewers/controller.ts');
    const items: Array<Record<string, unknown>> = [];
    let cursor: string | null = null;
    for (;;) {
      let u = `/api/v1/posts?limit=200&kind=${k}`;
      if (cursor) u += '&cursor=' + encodeURIComponent(cursor);
      const d = await (await fetch(u)).json();
      items.push(...(d.items ?? []));
      cursor = d.next_cursor ?? null;
      if (!cursor) break;
    }
    const kinds: Record<string, number> = {};
    let multi = 0;
    for (const p of items as never[]) {
      const post = p as {
        cover_asset_id?: string | null;
        members?: Array<{
          asset_id: string;
          restricted?: boolean;
          asset?: { asset_type?: number | null; file_extension?: string | null };
        }>;
      };
      const members = post.members ?? [];
      if (members.length > 1) multi++;
      const id = post.cover_asset_id ?? (members.length ? members[0].asset_id : null);
      const m = members.find((x) => x.asset_id === id);
      const resolved =
        !m || m.restricted || !m.asset
          ? '(withheld)'
          : (mod as { kindForAsset: (a: unknown) => string }).kindForAsset({
              asset_type: m.asset.asset_type ?? null,
              file_extension: m.asset.file_extension ?? null,
            });
      kinds[resolved] = (kinds[resolved] ?? 0) + 1;
    }
    return { total: items.length, kinds, multi };
  }, kind);
}

test.describe('#1166 browse footer — asset-type filter', () => {
  test('the control is on browse, all-checked, and offers the badge vocabulary', async ({
    page,
  }) => {
    await page.goto('/');
    await expect(page.locator(tid('view-controls'))).toBeVisible();
    await openPanel(page);

    const options = page.locator(tid('kind-filter-option'));
    expect(await options.count()).toBeGreaterThan(5);

    // ⭐ All-checked is the resting state, and it means NO filter — the
    // rule the whole control rests on. A resting state that already
    // carried a selection would put a `?kind=` on an unfiltered wall.
    expect(
      await options.evaluateAll((els) => els.every((e) => (e as HTMLInputElement).checked)),
    ).toBe(true);
    await expect(page.locator(tid('kind-filter-all'))).toBeChecked();
    expect(page.url()).not.toContain('kind=');
    await expect(page.locator(tid('kind-filter-toggle'))).not.toHaveAttribute('data-active', 'true');
  });

  test('filtering to one type leaves only that badge on the wall', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator(tid('browse-wall'))).toBeVisible();
    await openPanel(page);
    await pickOnly(page, 'video');

    await expect(page).toHaveURL(/kind=video/);
    await expect(page.locator(tid('kind-filter-toggle'))).toHaveAttribute('data-active', 'true');

    await expect(page.locator(tid('card-kind')).first()).toBeVisible();
    // Walk a page's worth so the append path is included, then read the
    // whole wall at once.
    for (let i = 0; i < 5; i++) {
      await page.mouse.wheel(0, 2400);
      await page.waitForTimeout(400);
    }
    const labels = await badgeLabels(page);
    expect(labels.length, 'no cards on the filtered wall').toBeGreaterThan(0);
    expect(
      labels.filter((l) => l !== KIND_LABEL.video),
      'a badge on the wall disagrees with the applied filter',
    ).toEqual([]);
  });

  test('every returned item — multi-asset ones too — has a cover of the asked kind', async ({
    page,
  }) => {
    await page.goto('/');
    await expect(page.locator(tid('browse-wall'))).toBeVisible();

    for (const kind of ['video', 'image', '3d']) {
      const r = await coverKindsFor(page, kind);
      test.skip(r.total === 0, `no ${kind} posts on this instance`);
      expect(
        Object.keys(r.kinds),
        `?kind=${kind} returned ${r.total} items (${r.multi} multi-asset) with cover kinds ` +
          JSON.stringify(r.kinds),
      ).toEqual([kind]);
    }
  });

  test('re-checking All clears the filter and restores the mixed wall', async ({ page }) => {
    await page.goto('/?kind=video');
    await expect(page.locator(tid('card-kind')).first()).toBeVisible();
    await openPanel(page);

    // From a filtered page the All box is unchecked; ticking it means
    // every type, which is the absence of a filter.
    await page.locator(tid('kind-filter-all')).click();
    await page.locator(tid('kind-filter-apply')).click();

    await expect(page).not.toHaveURL(/kind=/);
    await expect(page.locator(tid('kind-filter-toggle'))).not.toHaveAttribute(
      'data-active',
      'true',
    );
    await expect(page.locator(tid('card-kind')).first()).toBeVisible();
    await page.waitForTimeout(600);
    const labels = await badgeLabels(page);
    expect(
      new Set(labels).size > 1 || (await page.locator(tid('card-kind-multi')).count()) > 0,
      'clearing the filter left a single-kind wall',
    ).toBe(true);
  });

  test('a direct load of a filtered URL renders filtered, and multi-select works', async ({
    page,
  }) => {
    await page.goto('/?kind=image,3d');
    await expect(page.locator(tid('card-kind')).first()).toBeVisible();
    await page.waitForTimeout(600);

    const toggle = page.locator(tid('kind-filter-toggle'));
    await expect(toggle).toHaveAttribute('data-active', 'true');
    await expect(toggle).toContainText('2');

    const labels = await badgeLabels(page);
    expect(labels.length).toBeGreaterThan(0);
    expect(labels.filter((l) => l !== KIND_LABEL.image && l !== KIND_LABEL['3d'])).toEqual([]);

    // The panel re-opens showing what is applied, not a fresh all-check.
    await openPanel(page);
    const ticked = await page
      .locator(tid('kind-filter-option'))
      .evaluateAll((els) =>
        els
          .filter((e) => (e as HTMLInputElement).checked)
          .map((e) => (e as HTMLElement).dataset.kind),
      );
    expect(ticked).toEqual(['image', '3d']);
  });

  test('composes with the rail chips', async ({ page }) => {
    await page.goto('/');
    const chip = page.locator(tid('teams-rail-chip')).first();
    test.skip((await chip.count()) === 0, 'no team chip on this instance');
    await chip.click();
    await expect(page).toHaveURL(/team=/);

    await openPanel(page);
    await pickOnly(page, 'video');

    // Both conjuncts, both in the URL — the type filter is a different
    // axis from the rail's single-select chip and clears nothing.
    await expect(page).toHaveURL(/team=/);
    await expect(page).toHaveURL(/kind=video/);
    await page.waitForTimeout(800);
    const labels = await badgeLabels(page);
    expect(labels.filter((l) => l !== KIND_LABEL.video)).toEqual([]);
  });

  test('light dismiss: Escape and an outside click, never a lost draft', async ({ page }) => {
    await page.goto('/?kind=video');
    await openPanel(page);
    await page.keyboard.press('Escape');
    await expect(page.locator(tid('kind-filter-panel'))).toHaveCount(0);
    // Dismissing is not applying.
    await expect(page).toHaveURL(/kind=video/);

    await openPanel(page);
    // The page HEADING, not a viewport corner. The panel is anchored to
    // the bottom right and can be tall, so "somewhere far away" has to
    // be chosen deliberately — and the top-left corner is the brand
    // link, which dismisses the panel by NAVIGATING and takes `?kind=`
    // with it. The heading is inert, on screen, and outside the panel.
    const heading = page.locator(tid('browse-feed-heading')).first();
    await heading.click();
    await expect(page.locator(tid('kind-filter-panel'))).toHaveCount(0);
    await expect(page).toHaveURL(/kind=video/);

    // A draft abandoned by dismissal does not survive into the next
    // open: Apply is the only thing that commits, so an abandoned tick
    // must not be waiting the next time the panel comes up.
    await openPanel(page);
    await page.locator(`${tid('kind-filter-option')}[data-kind="image"]`).click();
    await heading.click();
    await expect(page).toHaveURL(/kind=video/);
    await openPanel(page);
    await expect(
      page.locator(`${tid('kind-filter-option')}[data-kind="image"]`),
    ).not.toBeChecked();
  });

  test('anonymous: the filter narrows within the public tier only', async ({
    browser,
    baseURL,
  }) => {
    // A fresh context with no stored session — the standalone project
    // reuses an authenticated one, which is exactly the identity this
    // case must not have. `baseURL` is threaded through explicitly:
    // `browser.newContext()` is the raw fixture and does NOT inherit
    // the project's `use` options, so a relative goto here would leave
    // the case silently unrun.
    const ctx = await browser.newContext({ storageState: undefined, baseURL });
    const page = await ctx.newPage();
    try {
      // Public mode is what decides whether this case is meaningful, and
      // the honest probe is the endpoint itself: with it OFF the feed
      // answers 401 before the handler sees the request.
      const probe = await page.request.get('/api/v1/posts?limit=1');
      test.skip(probe.status() === 401, 'public mode is off on this install');
      await page.goto('/');
      await expect(page.locator(tid('browse-wall'))).toBeVisible();

      const counts = await page.evaluate(async () => {
        const all = async (qs: string) => {
          let n = 0;
          let cursor: string | null = null;
          for (;;) {
            let u = '/api/v1/posts?limit=200' + qs;
            if (cursor) u += '&cursor=' + encodeURIComponent(cursor);
            const d = await (await fetch(u)).json();
            n += (d.items ?? []).length;
            cursor = d.next_cursor ?? null;
            if (!cursor) break;
          }
          return n;
        };
        return {
          unfiltered: await all(''),
          video: await all('&kind=video'),
          orgOnly: await all('&kind=video&visibility=org-only'),
          nonsense: await all('&kind=nonsense'),
        };
      });

      // Narrowing, never widening: a filtered anonymous page is a subset
      // of the unfiltered one, and naming a private tier adds nothing.
      expect(counts.video).toBeLessThanOrEqual(counts.unfiltered);
      expect(counts.orgOnly).toBe(0);
      // An unrecognised kind selects nothing rather than being ignored —
      // ignoring it would serve the whole feed under a label promising
      // one type.
      expect(counts.nonsense).toBe(0);

      if (counts.video > 0) {
        await page.goto('/?kind=video');
        await expect(page.locator(tid('card-kind')).first()).toBeVisible();
        await page.waitForTimeout(600);
        const labels = await badgeLabels(page);
        expect(labels.filter((l) => l !== KIND_LABEL.video)).toEqual([]);
      }
    } finally {
      await ctx.close();
    }
  });
});
