// #1167 / ADR 0094 — the maker's AI declaration.
//
// # The thing this file is really guarding
//
// ⛔ UNDECLARED IS NOT `none`. The obvious implementation was a boolean
// with `NOT NULL DEFAULT false`, and it is wrong in a way that only
// appears after there is data: every asset predating the feature would
// assert "the maker declares no generative AI" on the maker's behalf —
// a fabricated disclosure, on the one topic where a false disclaimer is
// the worst available error.
//
// Most of the cases below fail loudly against that implementation:
//
//   - an asset created without the field comes back with NO value, not
//     `none`;
//   - the create surface must OMIT the key rather than send a zero
//     value the way it sends `mature: false`;
//   - a post with one undeclared member is UNDECLARED, not `none`;
//   - a declared work can be returned to undeclared.
//
// # And the derivation is asymmetric
//
// A positive claim propagates on ANY (one `generated` member makes the
// post `generated`); the negative claim requires ALL. A "strongest
// value over a total order" implementation passes the first and fails
// the second, which is why both are here.
//
// The Go suite asserts the DATABASE derivation directly
// (assets/ai_provenance_derivation_test.go). This file asserts the same
// rule through the HTTP surface a client actually sees, because a
// correct trigger behind a projection that drops the column is the
// #1116 defect — the handler echoes its own wrong value consistently
// and a body assertion agrees with the bug.

import { test, expect } from '../../helpers/test';
import type { APIRequestContext } from '@playwright/test';
import { loginAsAdminViaAPI } from '../../helpers/auth';
import { tid } from '../../helpers/testids';

async function makeAsset(
  request: APIRequestContext,
  body: Record<string, unknown> = {},
): Promise<{ id: string; ai_provenance?: string | null }> {
  const up = await request.post('/api/v1/storage/objects', {
    data: Buffer.from(`1167 fixture ${Date.now()}-${Math.random()}`),
    headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'text/plain' },
  });
  expect(up.status()).toBe(201);
  const { hash } = (await up.json()) as { hash: string };

  const r = await request.post('/api/v1/assets', {
    data: {
      title: `1167 probe ${Date.now()}`,
      asset_type: 2,
      file_hash: hash,
      file_extension: 'txt',
      ...body,
    },
  });
  expect(r.status(), `create asset → ${r.status()} ${await r.text()}`).toBe(201);
  return (await r.json()) as { id: string; ai_provenance?: string | null };
}

async function readAsset(request: APIRequestContext, id: string) {
  const r = await request.get(`/api/v1/assets/${id}`);
  expect(r.status()).toBe(200);
  return (await r.json()) as { ai_provenance?: string | null };
}

/**
 * Remove a post AND the assets it was built from.
 *
 * ⛔ Deleting only the post is what #1198 is about. A post delete leaves
 * its member assets standing, the browse and uploads grids are
 * newest-first, and leftovers pile up at the TOP of page one where they
 * push the seeded corpus out of the window OTHER specs read —
 * `marquee-select-1177` and `ui-13` go red for reasons that have nothing
 * to do with them. A spec that changes what the next spec sees is not
 * isolated, however harmless its own leftovers look.
 */
async function cleanUp(request: APIRequestContext, postId: string, assetIds: string[]) {
  await request.delete(`/api/v1/posts/${postId}`).catch(() => undefined);
  for (const id of assetIds) {
    await request.delete(`/api/v1/assets/${id}`).catch(() => undefined);
  }
}

async function makePost(request: APIRequestContext, assetIds: string[]) {
  const r = await request.post('/api/v1/posts', {
    data: {
      title: `1167 post ${Date.now()}`,
      visibility: 'org-only',
      members: assetIds.map((id, i) => ({ asset_id: id, sort_order: i })),
    },
  });
  expect(r.status(), `create post → ${r.status()} ${await r.text()}`).toBe(201);
  return (await r.json()) as { id: string; ai_provenance?: string | null };
}

test.describe('AI provenance (#1167)', () => {
  test.beforeAll(async ({ request }) => {
    await loginAsAdminViaAPI(request);
  });

  test('an asset nobody was asked about carries NO declaration', async ({ request }) => {
    const a = await makeAsset(request);
    try {
      const got = await readAsset(request, a.id);
      expect(
        got.ai_provenance ?? null,
        'a default of `none` would have the server disclaim AI on behalf of a maker ' +
          'who was never asked — the fabricated disclaimer ADR 0094 exists to prevent',
      ).toBeNull();
    } finally {
      await request.delete(`/api/v1/assets/${a.id}`).catch(() => undefined);
    }
  });

  test('all three declared values round-trip, and clearing returns to undeclared', async ({
    request,
  }) => {
    for (const value of ['none', 'assisted', 'generated'] as const) {
      const a = await makeAsset(request, { ai_provenance: value });
      try {
        expect(
          a.ai_provenance,
          'the CREATE response must carry it — a client rendering straight from this ' +
            'body would otherwise be told the declaration did not take',
        ).toBe(value);
        expect((await readAsset(request, a.id)).ai_provenance).toBe(value);

        // PATCH to another value.
        const patched = await request.patch(`/api/v1/assets/${a.id}`, {
          data: { ai_provenance: 'generated' },
        });
        expect(patched.status()).toBe(200);
        expect(
          ((await patched.json()) as { ai_provenance?: string }).ai_provenance,
          'the PATCH response must carry it too — #1116 was exactly this converter ' +
            'dropping `mature`, and a body assertion agreed with the bug',
        ).toBe('generated');
        expect((await readAsset(request, a.id)).ai_provenance).toBe('generated');

        // And back to undeclared — a declaration is a statement a person
        // makes, and a person may have made it by accident.
        const cleared = await request.patch(`/api/v1/assets/${a.id}`, {
          data: { clear_ai_provenance: true },
        });
        expect(cleared.status()).toBe(200);
        expect((await readAsset(request, a.id)).ai_provenance ?? null).toBeNull();
      } finally {
        await request.delete(`/api/v1/assets/${a.id}`).catch(() => undefined);
      }
    }
  });

  test('sending a value AND the clear flag is refused rather than resolved', async ({
    request,
  }) => {
    const a = await makeAsset(request, { ai_provenance: 'assisted' });
    try {
      const r = await request.patch(`/api/v1/assets/${a.id}`, {
        data: { ai_provenance: 'none', clear_ai_provenance: true },
      });
      expect(
        r.status(),
        'guessing which the caller meant is worse than asking again — and a silent ' +
          'winner here would change a disclosure without telling anyone',
      ).toBe(400);
      expect((await readAsset(request, a.id)).ai_provenance).toBe('assisted');
    } finally {
      await request.delete(`/api/v1/assets/${a.id}`).catch(() => undefined);
    }
  });

  // ⚠️ #1116 ON THE CREATE PATH — found by the case above.
  //
  // #1116 fixed the PATCH converter that dropped `mature`. The CREATE
  // converter had the same hole and nothing covered it, so `POST
  // /assets` with `mature: true` answered `mature: false` from #1115
  // until this sprint. Pinned here rather than left to the AI field's
  // own case, because it is a DIFFERENT column with the same defect and
  // it should fail loudly if the converter is ever regenerated by hand.
  test('the create response echoes the flags it actually stored', async ({ request }) => {
    const a = await makeAsset(request, { mature: true, ai_provenance: 'assisted' });
    try {
      expect(
        (a as { mature?: boolean }).mature,
        'the create response said the mature flag did not take, while the database has it',
      ).toBe(true);
      expect(a.ai_provenance).toBe('assisted');

      // And the persisted value agrees — asserting only the echo would
      // pass on a handler that echoes the REQUEST rather than the row.
      const stored = await readAsset(request, a.id);
      expect((stored as { mature?: boolean }).mature).toBe(true);
      expect(stored.ai_provenance).toBe('assisted');
    } finally {
      await request.delete(`/api/v1/assets/${a.id}`).catch(() => undefined);
    }
  });

  test('an invalid value is refused, not silently stored', async ({ request }) => {
    const up = await request.post('/api/v1/storage/objects', {
      data: Buffer.from(`1167 bad ${Date.now()}`),
      headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'text/plain' },
    });
    const { hash } = (await up.json()) as { hash: string };
    const r = await request.post('/api/v1/assets', {
      data: {
        title: 'bad declaration',
        asset_type: 2,
        file_hash: hash,
        file_extension: 'txt',
        ai_provenance: 'maybe',
      },
    });
    expect(r.status()).toBe(400);
  });

  test('the post value is DERIVED, and one undeclared member makes it undeclared', async ({
    request,
  }) => {
    const gen = await makeAsset(request, { ai_provenance: 'generated' });
    const none1 = await makeAsset(request, { ai_provenance: 'none' });
    const none2 = await makeAsset(request, { ai_provenance: 'none' });
    const undeclared = await makeAsset(request);
    const created: string[] = [];

    try {
      // ANY: one generated member carries the whole post.
      const p1 = await makePost(request, [none1.id, gen.id]);
      created.push(p1.id);
      expect(
        p1.ai_provenance,
        'one AI-generated member makes the bundle one that contains AI-generated work',
      ).toBe('generated');

      // ALL: unanimous none IS none.
      const p2 = await makePost(request, [none1.id, none2.id]);
      created.push(p2.id);
      expect(p2.ai_provenance).toBe('none');

      // ⛔ THE ARM A TOTAL-ORDER IMPLEMENTATION FAILS.
      const p3 = await makePost(request, [none1.id, undeclared.id]);
      created.push(p3.id);
      expect(
        p3.ai_provenance ?? null,
        'a post containing a member nobody was asked about cannot claim "no AI" — ' +
          'that would fabricate the undeclared maker\'s disclaimer at the post level. ' +
          'A "strongest value wins" derivation returns `none` here and is wrong.',
      ).toBeNull();

      // And it stays derived on re-read, not just in the create echo.
      const reread = await request.get(`/api/v1/posts/${p1.id}`);
      expect(((await reread.json()) as { ai_provenance?: string }).ai_provenance).toBe(
        'generated',
      );
    } finally {
      for (const id of created) {
        await request.delete(`/api/v1/posts/${id}`).catch(() => undefined);
      }
      for (const a of [gen, none1, none2, undeclared]) {
        await request.delete(`/api/v1/assets/${a.id}`).catch(() => undefined);
      }
    }
  });

  // ⛔ THE ORDER OF THE TWO ACTS MUST NOT MATTER.
  //
  // The upload starts the instant a file is added, so `POST /assets`
  // has normally already gone by the time anybody reaches these
  // controls. Both orders are asserted because they broke for different
  // reasons and either one silently loses the artist's answer:
  //
  //   - DECLARE FIRST: the control wrote through to `rows`, and with an
  //     empty queue there were none, so the choice evaporated.
  //   - DROP FIRST: the value was only ever read in the create body, so
  //     a label applied a second later was never sent. `mature` has
  //     behaved this way since #1115 and is asserted here alongside.
  test('a label set AFTER the file was added still reaches the asset', async ({ page }) => {
    const assetIds: string[] = [];
    await page.goto('/create');
    await page.locator(tid('create-file-input')).setInputFiles({
      name: 'late-label.txt',
      mimeType: 'text/plain',
      buffer: Buffer.from(`1167 late label ${Date.now()}-${Math.random()}`),
    });
    // Wait for the row to be READY — i.e. the asset already exists and
    // the create request is long gone. That is the whole point.
    await expect(page.locator(tid('create-publish'))).toBeEnabled({ timeout: 30_000 });

    await page.locator(tid('ai-provenance-create-assisted')).check();
    await page.locator(tid('create-mature')).check();
    await page.locator(tid('create-publish')).click();
    await page.waitForURL(/\/posts\/[0-9a-f-]{36}/, { timeout: 30_000 });
    const postId = page.url().split('/posts/')[1];

    try {
      const post = (await (await page.request.get(`/api/v1/posts/${postId}`)).json()) as {
        ai_provenance?: string;
        members?: { asset_id: string }[];
      };
      expect(post.ai_provenance, 'the late declaration must derive onto the post').toBe(
        'assisted',
      );
      const asset = (await (
        await page.request.get(`/api/v1/assets/${post.members?.[0]?.asset_id}`)
      ).json()) as { ai_provenance?: string; mature?: boolean };
      expect(asset.ai_provenance).toBe('assisted');
      expect(
        asset.mature,
        'the mature box has the same failure mode and the same fix — a ticked box ' +
          'that stores nothing is worse than no box',
      ).toBe(true);
      assetIds.push(...(post.members ?? []).map((m) => m.asset_id));
    } finally {
      await cleanUp(page.request, postId, assetIds);
    }
  });

  test('the create page control stores nothing until it is touched', async ({ page }) => {
    await page.goto('/create');
    const group = page.locator(tid('ai-provenance-create'));
    await expect(group).toBeVisible();

    // No radio pre-selected, and the page SAYS what that means — an
    // empty group with no explanation reads as a question the artist
    // forgot rather than one they are entitled not to answer.
    await expect(group).toHaveAttribute('data-value', 'undeclared');
    await expect(page.locator(tid('ai-provenance-create-none'))).not.toBeChecked();
    await expect(page.locator(tid('ai-provenance-create-undeclared'))).toBeVisible();

    // Picking one, then clearing, returns to undeclared rather than
    // falling back to `none`.
    await page.locator(tid('ai-provenance-create-assisted')).check();
    await expect(group).toHaveAttribute('data-value', 'assisted');
    await page.locator(tid('ai-provenance-create-clear')).click();
    await expect(group).toHaveAttribute('data-value', 'undeclared');
  });

  test('set at upload on the create page, it reaches the asset and the post', async ({ page }) => {
    await page.goto('/create');
    await page.locator(tid('create-file-input')).setInputFiles({
      name: 'ai-probe.txt',
      mimeType: 'text/plain',
      buffer: Buffer.from(`1167 create-page fixture ${Date.now()}-${Math.random()}`),
    });
    await expect(page.locator(tid('create-publish'))).toBeEnabled({ timeout: 30_000 });

    await page.locator(tid('ai-provenance-create-generated')).check();
    await page.locator(tid('create-publish')).click();
    await page.waitForURL(/\/posts\/[0-9a-f-]{36}/, { timeout: 30_000 });
    const postId = page.url().split('/posts/')[1];
    // Declared OUTSIDE the try, so the finally can still tear it down
    // when an assertion inside fails — a cleanup that only runs on the
    // happy path is the leak it was written to prevent.
    const assetIds: string[] = [];

    try {
      const r = await page.request.get(`/api/v1/posts/${postId}`);
      expect(r.status()).toBe(200);
      const post = (await r.json()) as {
        ai_provenance?: string;
        members?: { asset_id: string }[];
      };
      assetIds.push(...(post.members ?? []).map((m) => m.asset_id));
      expect(
        post.ai_provenance,
        'the declaration made on the page must reach the asset and derive onto the post',
      ).toBe('generated');

      const assetId = post.members?.[0]?.asset_id;
      expect(assetId).toBeTruthy();
      const asset = await page.request.get(`/api/v1/assets/${assetId}`);
      expect(((await asset.json()) as { ai_provenance?: string }).ai_provenance).toBe(
        'generated',
      );
    } finally {
      await cleanUp(page.request, postId, assetIds);
    }
  });
});
