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
// ⭐ #1190 WIDENED THE FILTER AND THE BICONDITIONAL SURVIVED — read
// this before assuming it should have been weakened. The filter now
// matches ANY readable member, so a post can be selected by a kind its
// COVER is not. But a card only draws a KIND badge when it stands for
// exactly one visible asset: a set draws the count plus Shapes and
// names no kind at all (#1111). So on every card this check can see,
// "the cover's kind" and "the membership's kinds" are the same set of
// one, and both directions still hold. Widening it to sets would not
// make the check stronger — it would make it meaningless, since there
// is no badge there to agree or disagree with anything.
//
// It reads the badges the page actually drew rather than re-deriving
// them here. A second copy of `kindForAsset` in this file would agree
// with the bug it was written to catch, and importing the module by
// source path only works against the Vite dev server — CI serves the
// BUILT app, where `/src/...` does not exist.
//
// WHAT #1190 DID RETIRE is the partition. The kinds used to be
// disjoint, because a post had one cover and a cover has one kind; a
// post holding a .glb and an .mp4 is now in `?kind=3d` AND in
// `?kind=video`, so "no post appears in two buckets" is false by
// design and the assertion that pinned it is gone. Its replacement is
// the OVERLAP assertion — the same fact stated in the direction that is
// now true — plus the union law, which never depended on disjointness.
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
// SETS public mode for the length of that one assertion, under the
// cross-file lock, and puts the prior value back. It used to skip when
// the switch was off, which on CI was always (#1344).

import { test, expect, type Page } from '../../helpers/test';
import { tid } from '../../helpers/testids';
import { publicModeHold } from '../../helpers/public-mode';

/** ⚠️ CONTENDED INSTANCE STATE: `system.public_mode`.
 *
 *  This file is the fifth writer of the install-wide anonymous-browsing
 *  switch (ai-toggle-1251, collection-public-tier-1195,
 *  collection-cover-editor-1207 and advanced-operators are the others).
 *  The hold takes a cross-file lock, reads the prior value INSIDE it and
 *  puts that value back on release, which is the contract #1248 needed
 *  and a per-spec read/set/restore cannot give. */
const publicMode = publicModeHold('kind-filter-1166');

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
    // Single-asset cards only — a set draws the count badge and states
    // no kind, and since #1190 the wall legitimately holds sets whose
    // cover is not a video. `badgeLabels` reads `card-kind` exactly, so
    // what it collects is the population this rule applies to.
    const labels = await badgeLabels(page);
    expect(labels.length, 'no cards on the filtered wall').toBeGreaterThan(0);
    expect(
      labels.filter((l) => l !== KIND_LABEL.video),
      'a single-asset card on the wall disagrees with the applied filter',
    ).toEqual([]);
  });

  test('badge and filter agree in BOTH directions, and the kinds OVERLAP', async ({
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
    // badge on — which is every SINGLE-asset card and no other — it is
    // in `?kind=K` exactly when its badge says K. See the header note on
    // why #1190's any-member widening leaves this intact.
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

    const kinds = Object.keys(KIND_LABEL);

    // ⭐ #1190: A POST WITH TWO KINDS IN IT IS RETURNED BY BOTH.
    //
    // The one check here that the cover-only implementation cannot pass,
    // and the reason the disjointness assertions it replaced are gone.
    //
    // The hard part is naming such a post WITHOUT a kind table. This
    // file may not carry one — a second copy of `kindForAsset` agrees
    // with the bug it was written to catch — so the map is LEARNED from
    // what the app itself rendered: every SINGLE-asset card states its
    // kind in a badge, and the payload states that same asset's
    // extension and asset_type, so the wall is a (extension, asset_type)
    // → kind oracle for exactly the formats it is showing. A post whose
    // readable members resolve, through THAT map, to two different kinds
    // must be in both buckets. Nothing here decides what a format means;
    // the page already did.
    //
    // The key is the PAIR and not the extension, because the extension
    // alone is not the input to the derivation it is learning: a sprite
    // atlas is a PNG and only the ref separates it from a texture. Keyed
    // on the extension alone, one atlas on the wall would teach this
    // that "png means sprite sheet" and the assertion below would then
    // demand the wrong bucket. Any pair whose cards DISAGREE is dropped
    // rather than resolved — an oracle that has seen a contradiction
    // about a format has nothing to say about it.
    const corpus = await page.evaluate(async () => {
      const d = await (await fetch('/api/v1/posts?limit=200')).json();
      return (d.items ?? []).map(
        (p: {
          id: string;
          members?: Array<{
            restricted?: boolean;
            asset?: { file_extension?: string; asset_type?: number | null };
          }>;
        }) => ({
          id: p.id,
          formats: [
            ...new Set(
              (p.members ?? [])
                .filter((m) => !m.restricted && m.asset)
                .map(
                  (m) =>
                    (m.asset?.file_extension ?? '').replace(/^\./, '').toLowerCase() +
                    '|' +
                    String(m.asset?.asset_type ?? ''),
                ),
            ),
          ],
        }),
      ) as Array<{ id: string; formats: string[] }>;
    });
    const byId = new Map(corpus.map((p) => [p.id, p]));
    const learned = new Map<string, string | null>(); // null = contradicted
    for (const c of cards) {
      // Single-asset cards only — `cards` is exactly those — so the one
      // readable format and the badge describe the same asset.
      const formats = byId.get(c.id)?.formats ?? [];
      if (formats.length !== 1) continue;
      const seen = learned.get(formats[0]);
      if (seen === undefined) learned.set(formats[0], c.label);
      else if (seen !== c.label) learned.set(formats[0], null);
    }
    const labelToKind = new Map(kinds.map((k) => [KIND_LABEL[k], k]));
    const twoKindPosts: Array<{ id: string; a: string; b: string }> = [];
    for (const p of corpus) {
      const named = [
        ...new Set(
          p.formats
            .map((f) => learned.get(f))
            .filter((l): l is string => !!l && labelToKind.has(l)),
        ),
      ];
      if (named.length >= 2) {
        twoKindPosts.push({
          id: p.id,
          a: labelToKind.get(named[0])!,
          b: labelToKind.get(named[1])!,
        });
      }
    }
    // Genuinely unobservable on a corpus whose posts are all one kind —
    // there both implementations agree and there is nothing to see.
    test.skip(
      twoKindPosts.length === 0,
      'no post on this instance holds two kinds the wall could name',
    );
    for (const { id, a, b } of twoKindPosts.slice(0, 10)) {
      expect(
        [sets[a].has(id), sets[b].has(id)],
        `post ${id} holds a ${a} and a ${b}; both filters must return it (#1190)`,
      ).toEqual([true, true]);
    }

    // A multi-kind request is exactly the union of its parts — which is
    // also what covers the multi-asset posts, whose card states no kind
    // for the check above to use. Unaffected by the overlap: a union is
    // a set, so a post in two of the parts is in it once.
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

  // ⭐ #1190's DIAGNOSIS half. The owner picked E-book, got nothing, and
  // read "No posts yet — once posts are uploaded they'll appear here" as
  // the filter being broken. It was not: the feed pill was on Following,
  // and `?kind=ebook&feed=following` was legitimately empty while
  // `?kind=ebook` returned rows on the same session. An honestly empty
  // page that describes itself as an EMPTY INSTANCE is indistinguishable
  // from a broken one, so the sentence now names what emptied it.
  //
  // Both narrowings get their own case because they compose: the type
  // filter alone, and the type filter inside the Following scope.
  test('an empty filtered wall names the type filter', async ({ page }) => {
    // A kind this instance genuinely has none of. `sequence` is the
    // guaranteed one — a real kind in the vocabulary that no single
    // asset can ever resolve to — so this case never depends on the
    // corpus, and the page reads its selection off the URL exactly as
    // the dropdown writes it.
    await page.goto('/?kind=sequence');
    const title = page.locator(tid('browse-empty-title'));
    await expect(title).toBeVisible();
    // The TYPE is named, in the same words the checkbox list uses.
    await expect(title).toContainText('Image sequence');
    // And the old sentence — the one about the instance — is gone.
    await expect(title).not.toContainText('No posts yet');
  });

  test('an empty Following + type wall names BOTH', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator(tid('browse-wall'))).toBeVisible();
    // Drive the real feed pill, not a store poke: which scope is active
    // is exactly what the owner could not see, so the test has to get
    // there the way they did.
    await page.getByRole('tab', { name: 'Following' }).click();
    await page.goto('/?kind=sequence');

    const title = page.locator(tid('browse-empty-title'));
    await expect(title).toBeVisible();
    await expect(title).toContainText('Image sequence');
    await expect(title).toContainText('follow');
    // The hint names the way out of both, which is the part the owner
    // had no way to work out from the page.
    await expect(page.locator(tid('browse-empty-hint'))).toContainText('Latest');
  });

  test('composes with the rail chips', async ({ page, request }) => {
    // ⛔ THIS GUARD USED TO BE A POINT-IN-TIME DOM READ, AND IT IS THE
    // ONE SKIP #1348 ACTUALLY CAUGHT:
    //
    //     await page.goto('/');
    //     const chip = page.locator(tid('teams-rail-chip')).first();
    //     test.skip((await chip.count()) === 0, 'no team chip on this instance');
    //
    // `count()` does not wait. The rail renders behind its own GET
    // /teams and behind `browseRail.loaded`, and the whole section is
    // absent until that resolves (BrowseRail.svelte: `{#if auth.user &&
    // browseRail.loaded && hasChips}`), so "not loaded yet" and "no
    // teams on this instance" are the SAME DOM. On a quiet box the fetch
    // wins the race and the case runs; on a contended one it loses and
    // the case deletes itself with a message about the corpus.
    //
    // Measured, not reasoned: run 33198346487 skipped this on its
    // contended attempt and ran it on the quiet re-run of the same
    // commit, against a database seeded identically both times.
    //
    // So the corpus question is asked of an instrument load cannot move,
    // and the DOM question is WAITED for. A slow box now makes this case
    // slow instead of making it vanish. GET /teams is the SAME call the
    // rail makes (browseRail.load) from the same admin session, so the
    // answer is about the corpus rather than about a different question
    // that happens to correlate with it.
    const teams = await request.get('/api/v1/teams?limit=1');
    const teamCount = teams.ok()
      ? (((await teams.json()) as { items?: unknown[] }).items ?? []).length
      : 0;
    test.skip(
      teamCount === 0,
      'no team exists on this instance, so the rail has no chip to compose with',
    );

    await page.goto('/');
    const chip = page.locator(tid('teams-rail-chip')).first();
    // The rail is behind that fetch, so this is the wait the old
    // `count()` was missing. A team exists, therefore a chip is coming.
    await expect(
      chip,
      'GET /teams returned a team but no chip was drawn. The rail hides teams the ' +
        'account has hidden (browseRail.hidden), so either this account has curated ' +
        'them all away or the rail stopped rendering.',
    ).toBeVisible();
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
    request,
  }) => {
    // ⛔ THIS CASE USED TO SKIP ITSELF HERE, AND ON CI IT ALWAYS DID
    // (#1344). The guard was:
    //
    //     const probe = await page.request.get('/api/v1/posts?limit=1');
    //     test.skip(probe.status() === 401, 'public mode is off on this install');
    //
    // `public_mode`'s ZERO VALUE IS DISABLED and the key is deliberately
    // absent on a fresh install (sysconfig/publicmode.go: "the absence
    // of the key IS the default"). CI seeds a fresh database on every
    // run and nothing in ui-pr.yml turns the switch on, so the probe got
    // its 401 and the case removed itself. Confirmed on the CI log for
    // run 33198346487: it is one of the two names under `2 skipped`.
    //
    // Both readings of that were bad. Either the anonymous arm of the
    // kind filter had never been checked on CI, or it ran inside another
    // spec's public-mode window against state it did not set, which is
    // the #1248 lost update and worse, because that green is not
    // repeatable.
    //
    // So the precondition is SET rather than waited for, exactly as
    // ai-toggle-1251 and mature-row-1292 do it: borrow the switch under
    // the cross-file lock, assert, give it straight back.
    //
    // ⚠️ LOCK ORDER. This file takes `system.public_mode` and nothing
    // else. mature-row-1292 takes `system.mature_content` first and
    // nests public mode inside it; a file taking them the other way
    // round would deadlock against that one, so if this case ever needs
    // the mature switch too it must take mature content FIRST.
    //
    // Waiting for the switch can outlast the default 30s budget, since
    // collection-cover-editor-1207 holds it across its whole file.
    test.setTimeout(600_000);
    await publicMode.acquire(request);

    // A fresh context with no stored session — the standalone project
    // reuses an authenticated one, which is exactly the identity this
    // case must not have. `baseURL` is threaded through explicitly:
    // `browser.newContext()` is the raw fixture and does NOT inherit
    // the project's `use` options, so a relative goto here would leave
    // the case silently unrun.
    const ctx = await browser.newContext({ storageState: undefined, baseURL });
    const page = await ctx.newPage();
    try {
      await publicMode.set(request, true);

      // ⭐ THE PRECONDITION IS ASSERTED, NOT ASSUMED. #1344 records the
      // near miss that makes this line load-bearing: with public mode
      // off, an anonymous visitor gets a sign-in card, and every
      // "narrowing" assertion below would hold vacuously against it.
      // The wall being on screen is what says the reader is looking at
      // a feed at all.
      const probe = await page.request.get('/api/v1/posts?limit=1');
      expect(
        probe.status(),
        'public mode was set on, so the anonymous feed must answer rather than 401',
      ).toBe(200);
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
      // The switch goes back before the lock does, and `release` does
      // both. Idempotent, so the afterAll backstop below writes nothing
      // once this has run.
      await publicMode.release(request);
    }
  });

  // A backstop for a crash inside the window above. Without it a failed
  // assertion would leave the instance public for every spec after this
  // one, which is the second half of the damage #1248 describes.
  test.afterAll(async ({ request }) => {
    await publicMode.release(request);
  });
});
