// post-band-format-1190.spec.ts
//
// The thumbnail card's FORMAT band on a multi-asset post (#1190).
//
// The owner's ruling: "if a multi asset post contains all the same
// extension (glb, png, etc...) we can place the extension on the
// thumbnail. Not if it's mixed. Maybe we can put (mixed) for the
// extension instead?"
//
// # Why this is an end-to-end case and not only a unit test
//
// CardChrome.placement.test.ts pins the RULE against hand-built member
// arrays: uniform shows the extension, mixed shows the word, a
// restricted member never changes the answer. What it cannot pin is
// that the rule is being fed the real thing. The band is derived from
// `post.members[].asset.file_extension` on the feed payload, and that
// payload is assembled by a server pass (enrichPreview) that redacts
// per caller — so "the card computes uniformity correctly" and "the
// card is computing it over the members this reader actually has" are
// two facts, and only the second one needs a browser.
//
// So this walks a real wall and checks AGREEMENT: for every multi-asset
// card that drew a band, the word on it must be exactly what the
// payload's own readable members say it should be. The expectation is
// recomputed here from the API response rather than restated as a
// literal, which is what makes it independent of whatever the seeded
// corpus happens to hold — and the two branches are counted so a run
// where neither shape exists says so instead of passing vacuously.

import { test, expect } from '../../helpers/test';
import { tid } from '../../helpers/testids';

/** The one extension every readable member shares, `null` when they
 *  disagree (mixed) or when there is nothing readable to compare.
 *
 *  Deliberately a re-derivation from the WIRE, not an import of the
 *  card's own helper: an oracle that shares code with the thing it
 *  judges agrees with its bugs. It is three lines and no vocabulary —
 *  unlike `kindForAsset`, which is a table this file must never copy. */
function uniformExtOf(members: Array<{ restricted?: boolean; asset?: { file_extension?: string } }>) {
  const exts = members
    .filter((m) => !m.restricted && m.asset)
    .map((m) => (m.asset?.file_extension ?? '').replace(/^\./, '').toLowerCase());
  if (exts.length === 0) return { readable: 0, uniform: null as string | null };
  const set = new Set(exts);
  return { readable: exts.length, uniform: set.size === 1 ? exts[0] || null : null };
}

test.describe('#1190 the multi-asset format band', () => {
  test('says the shared extension, or the word — and agrees with the payload', async ({ page }) => {
    // Thumbnail is the density that HAS a band; grid and masonry draw
    // none at all, which the unit tests pin separately.
    await page.addInitScript(() => {
      localStorage.setItem('aa_browse_mode', 'thumbnail');
    });
    await page.goto('/');
    await expect(page.locator(tid('browse-wall'))).toBeVisible();
    await expect(page.locator(tid('thumb-band-top')).first()).toBeVisible();

    // The wall's own first page, read from the same endpoint the page
    // read it from, so the two describe the same rows.
    const payload = await page.evaluate(async () => {
      const d = await (await fetch('/api/v1/posts?limit=36')).json();
      return (d.items ?? []) as Array<{
        id: string;
        cover_asset_id?: string | null;
        members: Array<{
          asset_id: string;
          restricted?: boolean;
          asset?: { file_extension?: string };
        }>;
      }>;
    });
    expect(payload.length, 'no posts on this instance').toBeGreaterThan(0);

    // What each card actually drew, keyed by post id — the same
    // ancestor walk the kind-filter spec uses, since the band carries
    // no post id of its own.
    const drawn = await page.evaluate(() => {
      const out: Record<string, string> = {};
      for (const band of Array.from(document.querySelectorAll('[data-testid="thumb-band-top"]'))) {
        let el: HTMLElement | null = band.parentElement;
        for (let depth = 0; el && depth < 10; depth++, el = el.parentElement) {
          const hrefs = new Set(
            Array.from(el.querySelectorAll('a[href^="/posts/"]')).map(
              (a) => a.getAttribute('href') ?? '',
            ),
          );
          if (hrefs.size !== 1) continue;
          const id = [...hrefs][0].split('/posts/')[1].split(/[?#]/)[0];
          const ext = band.querySelector('[data-testid="thumb-band-extension"]');
          out[id] = ext ? (ext.textContent ?? '').trim() : '';
          break;
        }
      }
      return out;
    });

    let uniformSeen = 0;
    let mixedSeen = 0;
    for (const post of payload) {
      const shown = drawn[post.id];
      if (shown === undefined) continue; // not on the loaded wall
      if (post.members.length <= 1) continue; // the single-asset arm
      const { readable, uniform } = uniformExtOf(post.members);
      // A restricted COVER withholds the whole band — badge included —
      // and is covered by its own unit test; skip rather than assert an
      // empty string against it here. The cover is resolved the way the
      // card resolves it: the explicit one, else the first member.
      const coverId = post.cover_asset_id ?? post.members[0]?.asset_id;
      if (post.members.find((m) => m.asset_id === coverId)?.restricted) continue;
      if (readable === 0) {
        expect(shown, `post ${post.id}: no readable member, so no format to state`).toBe('');
        continue;
      }
      if (uniform) {
        uniformSeen++;
        expect(shown, `post ${post.id}: every readable member is .${uniform}`).toBe(uniform);
      } else {
        mixedSeen++;
        expect(shown, `post ${post.id}: readable members disagree, so the band says so`).toBe(
          'mixed',
        );
      }
    }

    // Vacuity guard. The assertions above are all conditional on the
    // corpus, so a run that saw neither shape proved nothing — and the
    // seeded instance has both (uniform packs and mixed drops).
    expect(
      uniformSeen + mixedSeen,
      'no multi-asset post on the first page — nothing was actually checked',
    ).toBeGreaterThan(0);
  });

  test('the mixed band is marked as a word, not as an extension', async ({ page }) => {
    await page.addInitScript(() => {
      localStorage.setItem('aa_browse_mode', 'thumbnail');
    });
    await page.goto('/');
    await expect(page.locator(tid('thumb-band-top')).first()).toBeVisible();

    const mixed = page.locator(`${tid('thumb-band-extension')}[data-mixed="true"]`);
    test.skip((await mixed.count()) === 0, 'no mixed-format post on the first page');
    // It reads as an extension by position and by casing, so the
    // accessible name has to say what it means — a screen reader
    // otherwise hears a filename fragment.
    await expect(mixed.first()).toHaveAttribute('aria-label', /mixed/i);
    await expect(mixed.first()).toHaveText('mixed');
  });
});
