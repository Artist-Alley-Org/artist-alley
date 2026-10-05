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

import { test, expect, type APIRequestContext } from '../../helpers/test';
import { tid } from '../../helpers/testids';

// Three 1x1 PNGs that differ only in the pixel's colour.
//
// The band is derived from each member's `file_extension` COLUMN, not
// from the bytes, so one image would have done — except that storage is
// content-addressed: posting the same bytes twice returns the SAME
// asset id, the two members collapse into one, and a post built to be
// multi-asset arrives single-asset and is skipped by the very loop this
// file exists to run. Distinct bytes are what make them distinct rows.
const PNG_1PX = [
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC',
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGNg+M8AAAICAQB7CYF4AAAAAElFTkSuQmCC',
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGNgYPgPAAEDAQAIicLsAAAAAElFTkSuQmCC',
].map((b64) => Buffer.from(b64, 'base64'));

const STAMP = Date.now();
const UNIFORM_TITLE = `1190 uniform ${STAMP}`;
const MIXED_TITLE = `1190 mixed ${STAMP}`;

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
  // Serial: both cases read one provisioned pair.
  test.describe.configure({ mode: 'serial' });

  // ── Why this file provisions its own posts ────────────────────────
  //
  // It used to assert against whatever the seeded corpus happened to
  // put on the wall's first page, and both cases were conditional on
  // finding a multi-asset post there — one with a vacuity guard, one
  // with a `test.skip`. Measured on the coding stack: the corpus holds
  // 117 multi-asset posts in its first 200, and exactly ONE of them is
  // in the first 36. The feed is newest-first and the seed's recent
  // posts are single-asset, so a couple of transient posts created by
  // any concurrently-running spec are enough to push that one row out
  // of the window.
  //
  // Which is what happened: two consecutive full-suite runs on an
  // unchanged tree disagreed here — run 1 exercised both arms, run 2
  // failed the vacuity guard and skipped the mixed case. A test whose
  // result depends on what else was running is not measuring the
  // branch, and "it proved nothing" is the honest half of that; the
  // dishonest half is that nobody can tell the two runs apart from the
  // summary line.
  //
  // So the two shapes the rule distinguishes are now GUARANTEED to be
  // on the wall, provisioned here and removed in afterAll. The corpus
  // walk below is unchanged and still checks agreement across every
  // other card it finds — the fixtures raise its floor, they do not
  // replace it.
  let fx: { uniformId: string; mixedId: string; assetIds: string[] } | undefined;

  test.beforeAll(async ({ request }: { request: APIRequestContext }) => {
    const json = async (r: { json(): Promise<unknown> }) =>
      (await r.json()) as Record<string, unknown>;

    const mkAsset = async (ext: string, n: number): Promise<string> => {
      const up = await request.post('/api/v1/storage/objects', {
        data: PNG_1PX[n - 1],
        headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'image/png' },
      });
      expect(up.status(), `uploading fixture bytes ${n}`).toBe(201);
      const fileHash = String((await json(up)).hash);
      const r = await request.post('/api/v1/assets', {
        data: {
          title: `1190 fixture ${ext} ${n} ${STAMP}`,
          asset_type: 1,
          file_extension: ext,
          file_hash: fileHash,
          original_filename: `1190-${n}.${ext}`,
        },
      });
      expect(r.status(), `creating the .${ext} fixture asset`).toBeLessThan(300);
      return String((await json(r)).id);
    };
    const pngA = await mkAsset('png', 1);
    const pngB = await mkAsset('png', 2);
    const jpg = await mkAsset('jpg', 3);

    const mkPost = async (title: string, members: string[]): Promise<string> => {
      const r = await request.post('/api/v1/posts', {
        data: {
          title,
          description: '1190 fixture',
          members: members.map((asset_id) => ({ asset_id })),
        },
      });
      expect(r.status(), `creating post "${title}"`).toBeLessThan(300);
      return String((await json(r)).id);
    };
    // Two members each, because the band is a MULTI-asset affordance —
    // a single-asset post draws its own extension by a different path.
    const uniformId = await mkPost(UNIFORM_TITLE, [pngA, pngB]);
    const mixedId = await mkPost(MIXED_TITLE, [pngB, jpg]);

    fx = { uniformId, mixedId, assetIds: [pngA, pngB, jpg] };
  });

  test.afterAll(async ({ request }: { request: APIRequestContext }) => {
    if (!fx) return;
    await request.delete(`/api/v1/posts/${fx.uniformId}`).catch(() => undefined);
    await request.delete(`/api/v1/posts/${fx.mixedId}`).catch(() => undefined);
    for (const id of fx.assetIds) {
      await request.delete(`/api/v1/assets/${id}`).catch(() => undefined);
    }
  });

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

    // Vacuity guard. The corpus assertions above are all conditional on
    // what the wall drew, so a run that saw neither shape proved
    // nothing. Both shapes are provisioned in beforeAll, so reaching
    // this line with a zero now means the fixtures themselves never
    // made it onto the wall — which is a real failure, not an unlucky
    // corpus.
    expect(
      drawn[fx!.uniformId],
      'the provisioned uniform post is not on the wall — the fixture that makes ' +
        'this test deterministic did not land (#1190)',
    ).toBeDefined();
    expect(
      drawn[fx!.mixedId],
      'the provisioned mixed post is not on the wall',
    ).toBeDefined();
    expect(uniformSeen, 'the uniform arm was never exercised').toBeGreaterThan(0);
    expect(mixedSeen, 'the mixed arm was never exercised').toBeGreaterThan(0);
  });

  test('the mixed band is marked as a word, not as an extension', async ({ page }) => {
    await page.addInitScript(() => {
      localStorage.setItem('aa_browse_mode', 'thumbnail');
    });
    await page.goto('/');
    await expect(page.locator(tid('thumb-band-top')).first()).toBeVisible();

    const mixed = page.locator(`${tid('thumb-band-extension')}[data-mixed="true"]`);
    // No `test.skip` on an empty corpus any more (#1190): the mixed post
    // is provisioned in beforeAll, so an empty locator here is a
    // failure. A skip that fires on half the runs reports the same
    // green summary as a run that checked something.
    await expect(
      mixed.first(),
      'no mixed-format band on the wall, though one is provisioned',
    ).toBeVisible({ timeout: 15_000 });
    // It reads as an extension by position and by casing, so the
    // accessible name has to say what it means — a screen reader
    // otherwise hears a filename fragment.
    await expect(mixed.first()).toHaveAttribute('aria-label', /mixed/i);
    await expect(mixed.first()).toHaveText('mixed');
  });
});
