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
// The check is a BICONDITIONAL over the rendered wall, not a spot
// check: for every card that draws a kind badge, `badge === K` must
// hold exactly when the post is in what `?kind=K` returns. One
// direction alone is satisfiable by a filter that returns too little.
//
// It reads the badges the page actually drew rather than re-deriving
// them here. A second copy of `kindForAsset` in this file would agree
// with the bug it was written to catch, and importing the module by
// source path only works against the Vite dev server — CI serves the
// BUILT app, where `/src/...` does not exist.
//
// Multi-asset posts are the deliberate limit: their card states the
// SET (count plus Shapes) and no kind at all (#1111), so there is no
// badge for a filter to disagree with. What is asserted for them is
// what is assertable — the kinds partition the feed, so each lands in
// exactly one bucket.
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

/** Every card on the wall that DREW a kind badge, as {postId, label}.
 *
 *  The post id comes from the card's own permalink rather than a
 *  dedicated attribute: walk up from the badge until an ancestor holds
 *  exactly one distinct `/posts/<id>` link, which is that card and
 *  nothing wider. Cards with a restricted cover draw no kind badge at
 *  all and are simply absent — which is the right answer, since they
 *  must be selected by no kind either. */
async function badgedCards(page: Page): Promise<Array<{ id: string; label: string }>> {
  return page.evaluate(() => {
    const out: Array<{ id: string; label: string }> = [];
    for (const badge of Array.from(document.querySelectorAll('[data-testid="card-kind"]'))) {
      let el: HTMLElement | null = badge.parentElement;
      for (let depth = 0; el && depth < 10; depth++, el = el.parentElement) {
        const hrefs = new Set(
          Array.from(el.querySelectorAll('a[href^="/posts/"]')).map(
            (a) => a.getAttribute('href') ?? '',
          ),
        );
        if (hrefs.size !== 1) continue;
        const id = [...hrefs][0].split('/posts/')[1].split(/[?#]/)[0];
        out.push({ id, label: badge.getAttribute('aria-label') ?? '' });
        break;
      }
    }
    return out;
  });
}

/** Every post id `?kind=<kind>` returns, paged to exhaustion. */
async function idsForKind(page: Page, kind: string): Promise<string[]> {
  return page.evaluate(async (k: string) => {
    const ids: string[] = [];
    let cursor: string | null = null;
    for (;;) {
      let u = '/api/v1/posts?limit=200';
      if (k) u += '&kind=' + k;
      if (cursor) u += '&cursor=' + encodeURIComponent(cursor);
      const d = await (await fetch(u)).json();
      for (const p of d.items ?? []) ids.push(p.id);
      cursor = d.next_cursor ?? null;
      if (!cursor) break;
    }
    return ids;
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

  test('badge and filter agree in BOTH directions, and the kinds partition the feed', async ({
    page,
  }) => {
    await page.goto('/');
    await expect(page.locator(tid('browse-wall'))).toBeVisible();
    await expect(page.locator(tid('card-kind')).first()).toBeVisible();
    // Walk a few screens so the sample spans more than the first page.
    for (let i = 0; i < 4; i++) {
      await page.mouse.wheel(0, 2400);
      await page.waitForTimeout(400);
    }

    const cards = await badgedCards(page);
    expect(cards.length, 'no badged cards on the unfiltered wall').toBeGreaterThan(5);

    const sets: Record<string, Set<string>> = {};
    for (const kind of Object.keys(KIND_LABEL)) {
      sets[kind] = new Set(await idsForKind(page, kind));
    }

    // ⭐ The biconditional. For every card the page actually drew a kind
    // badge on: it is in `?kind=K` exactly when its badge says K.
    for (const kind of Object.keys(KIND_LABEL)) {
      const label = KIND_LABEL[kind];
      const shouldBe = cards.filter((c) => c.label === label).map((c) => c.id);
      const shouldNot = cards.filter((c) => c.label !== label).map((c) => c.id);
      expect(
        shouldBe.filter((id) => !sets[kind].has(id)),
        `cards badged "${label}" that ?kind=${kind} did NOT return`,
      ).toEqual([]);
      expect(
        shouldNot.filter((id) => sets[kind].has(id)),
        `?kind=${kind} returned cards whose badge is not "${label}"`,
      ).toEqual([]);
    }

    // The kinds are disjoint — a post's cover has ONE kind, so no post
    // may be reachable through two of them.
    const kinds = Object.keys(KIND_LABEL);
    for (let i = 0; i < kinds.length; i++) {
      for (let j = i + 1; j < kinds.length; j++) {
        const overlap = [...sets[kinds[i]]].filter((id) => sets[kinds[j]].has(id));
        expect(overlap, `${kinds[i]} and ${kinds[j]} both returned the same post`).toEqual([]);
      }
    }

    // A multi-kind request is exactly the union of its parts — which is
    // also what covers the multi-asset posts, whose card states no kind
    // for the check above to use.
    const union = new Set(await idsForKind(page, kinds.join(',')));
    const parts = new Set(kinds.flatMap((k) => [...sets[k]]));
    expect(union.size, 'the multi-kind request is not the union of its parts').toBe(parts.size);
    expect([...parts].filter((id) => !union.has(id))).toEqual([]);

    // And every one of them is on the unfiltered feed: the filter
    // narrows, it never introduces a row.
    const all = new Set(await idsForKind(page, ''));
    expect(
      [...union].filter((id) => !all.has(id)),
      'a kind-filtered page returned a post the unfiltered feed does not have',
    ).toEqual([]);
    // `<=` and not `<`: whether any post's cover falls OUTSIDE the five
    // kinds sampled here is a property of the instance's fixture, not of
    // the filter. The subset assertion above is the one that carries the
    // meaning.
    expect(union.size).toBeLessThanOrEqual(all.size);
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
