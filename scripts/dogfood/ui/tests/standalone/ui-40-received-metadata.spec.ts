// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1577: a received asset's latest received metadata, in a real browser.
//
// What this proves, and what it does not: the page's real rendering of a
// received asset detail answer, in Chromium, with that ONE request
// answered by page.route from synthetic, schema-shaped fixtures at
// synthetic UUIDs. Nothing is written: no principal, asset, grant, peer
// or config is created; the login is the framework's normal admin login.
// It is not a federation test: no envelope is signed, sent or admitted,
// and who may see a record is decided by the server (Go tests in
// internal/federation/receivedassets). The rendering details are pinned
// below the browser in web/src/routes/account/received/received-pages.test.ts.

import { test, expect } from '../../helpers/test';
import type { Page, Route } from '@playwright/test';
import { loginAsAdminViaUI } from '../../helpers/auth';
import { expectPageRendersCleanly } from '../../helpers/assertions';
import { testIds, tid } from '../../helpers/testids';

const R = testIds.received;
const DETAIL_ROUTE = '**/api/v1/federation/received-assets/*';
const PUBLISHED = '2026-11-23T10:00:00.123456789Z';

const uuid = (n: number) => `00000000-0000-4000-8000-${String(1577_000 + n).padStart(12, '0')}`;

function summary(id: string, version: number) {
  const original = {
    position: 0,
    role: 'original',
    sha256: 'ab'.repeat(32),
    size_bytes: 2048,
    declared_media_type: 'image/png',
    declared_original_filename: `harbour-v${version}.png`,
    detected_media_type: 'image/png',
    detected_by: 'net/http.DetectContentType',
    state: 'verified',
  };
  return {
    id,
    peer_id: '11111111-2222-4333-8444-555555555555',
    peer_url: 'https://studio-a.example',
    peer_name: 'Studio A',
    origin_asset_uri: `https://studio-a.example/assets/${id}`,
    version_uri: `https://studio-a.example/assets/${id}/versions/${version}`,
    version_number: version,
    sensitivity: 'public',
    received_at: '2026-11-20T10:00:00Z',
    completed_at: '2026-11-20T10:01:00Z',
    original,
  };
}

function current(id: string, snapshot: Record<string, unknown>, extra: Record<string, unknown> = {}) {
  return {
    profile: 'asset-metadata-1',
    snapshot,
    revision_id: '99999999-8888-4777-8666-555555555555',
    peer_id: '11111111-2222-4333-8444-555555555555',
    origin_asset_uri: `https://studio-a.example/assets/${id}`,
    activity_uri: 'https://studio-a.example/activities/metadata-2',
    actor_uri: 'https://studio-a.example/users/bob',
    version_uri: `https://studio-a.example/assets/${id}/versions/1`,
    file_set_sha256: '0'.repeat(64),
    published: PUBLISHED,
    published_seconds: 1795428000,
    published_nanos: 123456789,
    origin_signer_key_id: 'https://studio-a.example/users/bob#main-key',
    origin_signature_algorithm: 'Ed25519',
    origin_signature_value: 'c2lnbmVkIGJ5IHN0dWRpbyBh',
    recipient_signer_key_id: 'https://receiver.example/actor#main-key',
    received_at: '2026-11-23T10:00:05.654321Z',
    ...extra,
  };
}

function detail(id: string, opts: { version?: number; available?: boolean; current?: object } = {}) {
  const s = summary(id, opts.version ?? 1);
  return {
    asset: s,
    activity_uri: 'https://studio-a.example/activities/1',
    actor_uri: 'https://studio-a.example/users/alice',
    files: [s.original],
    declared: { rights: 'http://rightsstatements.org/vocab/InC/1.0/', description: { title: 'Harbour at dusk' } },
    original_available: opts.available ?? true,
    ...(opts.current ? { current_metadata: opts.current } : {}),
  };
}

type Answer = { status: number; json: object };

/** Answer the detail GET for each id from `answers`; every other id is a 404. */
async function serve(page: Page, answers: Record<string, Answer>): Promise<string[]> {
  const asked: string[] = [];
  await page.route(DETAIL_ROUTE, async (route: Route) => {
    const id = new URL(route.request().url()).pathname.split('/').pop() ?? '';
    asked.push(id);
    const a = answers[id] ?? { status: 404, json: { error: 'received asset not found' } };
    await route.fulfill({ status: a.status, json: a.json });
  });
  return asked;
}

const claim = (page: Page, name: string) => page.locator(`${tid(R.metadataClaim)}[data-claim="${name}"]`);
const field = (page: Page, name: string) => page.locator(`${tid(R.metadataField)}[data-field="${name}"]`);

test.describe('received asset latest metadata (#1577, mocked detail API)', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('hostile text in every claim and provenance field is shown as text, runs nothing and loads nothing', async ({ page }) => {
    const id = uuid(1);
    const evil = (n: number) =>
      `<img src=https://evil.example/${n}.png onerror="window.__pwnedMeta=${n}"><script>window.__pwnedMeta=${n}</script>`;
    const snapshot = {
      title: evil(1),
      description: `${evil(2)} [x](javascript:alert(2))`,
      rights: 'javascript:alert(3)//https://creativecommons.org/licenses/by/4.0/',
      requiredStatement: {
        label: { en: [evil(4)] },
        value: { none: ['<a href="https://evil.example/5">Studio A</a>', '<iframe src="https://evil.example/6"></iframe>'] },
      },
    };
    const asked = await serve(page, {
      [id]: {
        status: 200,
        json: detail(id, {
          current: current(id, snapshot, {
            activity_uri: 'javascript:alert(7)',
            version_uri: 'https://evil.example/9.png',
            origin_signer_key_id: '<svg onload="window.__pwnedMeta=10"></svg>',
          }),
        }),
      },
    });
    const foreign: string[] = [];
    page.on('request', (r) => {
      if (r.url().includes('evil.example') || r.url().startsWith('javascript:')) foreign.push(r.url());
    });
    let dialog = false;
    page.on('dialog', async (d) => {
      dialog = true;
      await d.dismiss();
    });

    await page.goto(`/account/received/${id}`);
    const panel = page.locator(tid(R.metadata));
    await expect(panel).toBeVisible();
    await expect(claim(page, 'title').locator(tid(R.metadataValue))).toHaveText(evil(1));
    await expect(claim(page, 'rights').locator(tid(R.metadataValue))).toHaveText(snapshot.rights);
    await expect(claim(page, 'requiredStatement').locator(tid(R.metadataText))).toHaveText([
      evil(4),
      snapshot.requiredStatement.value.none[0],
      snapshot.requiredStatement.value.none[1],
    ]);
    await panel.locator('summary').click();
    await expect(field(page, 'activity')).toHaveText('javascript:alert(7)');
    await expect(field(page, 'origin_key')).toHaveText('<svg onload="window.__pwnedMeta=10"></svg>');
    await expect(panel.locator('img, script, iframe, a, svg, object, embed, style')).toHaveCount(0);
    expect(await page.evaluate(() => (window as unknown as { __pwnedMeta?: number }).__pwnedMeta)).toBeUndefined();
    expect(foreign).toEqual([]);
    expect(dialog).toBe(false);
    expect(asked).toEqual([id]);
    await expectPageRendersCleanly(page);
  });

  test('the latest snapshot sits apart from the original details, and four nulls clear every claim', async ({ page }) => {
    const shown = uuid(2);
    const cleared = uuid(3);
    await serve(page, {
      [shown]: {
        status: 200,
        json: detail(shown, {
          current: current(shown, {
            title: 'Harbour at dawn',
            description: 'Line one\nLine two',
            rights: 'https://creativecommons.org/licenses/by/4.0/',
            requiredStatement: { label: { en: ['Attribution'], fr: ['Attribution FR'] }, value: { none: ['Studio A'] } },
          }),
        }),
      },
      [cleared]: {
        status: 200,
        json: detail(cleared, {
          current: current(cleared, { title: null, description: null, rights: null, requiredStatement: null }),
        }),
      },
    });

    await page.goto(`/account/received/${shown}`);
    await expect(page.locator(tid(R.metadata))).toContainText('Latest received metadata');
    await expect(page.locator(tid(R.metadataPeer))).toHaveText('Studio A');
    await expect(claim(page, 'title').locator(tid(R.metadataValue))).toHaveText('Harbour at dawn');
    await expect(claim(page, 'requiredStatement')).toContainText('No language specified');
    await expect(claim(page, 'requiredStatement').locator(tid(R.metadataText))).toHaveText([
      'Attribution',
      'Attribution FR',
      'Studio A',
    ]);
    await expect(page.locator(tid(R.metadataPublished))).toHaveText(PUBLISHED);
    await expect(page.locator(tid(R.metadata))).not.toContainText('Harbour at dusk');
    await expect(page.locator(tid(R.claimsTitle))).toHaveText('Original declared details');
    await expect(page.locator(tid(R.claims))).toContainText('Harbour at dusk');
    await expect(page.locator(tid(R.originalActivity))).toHaveText('https://studio-a.example/activities/1');
    await expectPageRendersCleanly(page);

    await page.goto(`/account/received/${cleared}`);
    await expect(page.locator(tid(R.metadata))).toBeVisible();
    for (const name of ['title', 'description', 'rights', 'requiredStatement']) {
      await expect(claim(page, name).locator(tid(R.metadataEmpty))).toHaveText('Not supplied in this snapshot');
    }
    await expect(page.locator(tid(R.metadata))).not.toContainText('Harbour at dusk');
    await expect(page.locator(tid(R.claims))).toContainText('Harbour at dusk');
    await expectPageRendersCleanly(page);
  });

  test("a snapshot bound to an older version keeps the current version's files, and a view-only share offers no download", async ({ page }) => {
    const id = uuid(4);
    await serve(page, {
      [id]: {
        status: 200,
        json: detail(id, {
          version: 2,
          available: false,
          current: current(
            id,
            { title: 'Named by version 1', description: null, rights: null, requiredStatement: null },
            { file_set_sha256: '1'.repeat(64) },
          ),
        }),
      },
    });
    await page.goto(`/account/received/${id}`);
    await expect(claim(page, 'title').locator(tid(R.metadataValue))).toHaveText('Named by version 1');
    await page.locator(tid(R.metadataProvenance)).locator('summary').click();
    await expect(field(page, 'bound_version')).toHaveText(`https://studio-a.example/assets/${id}/versions/1`);
    await expect(page.locator(tid(R.title))).toContainText('harbour-v2.png');
    await expect(page.locator(tid(R.file))).toHaveCount(1);
    await expect(page.locator(tid(R.files))).toContainText('ab'.repeat(32));
    await expect(page.locator(tid(R.downloadUnavailable))).toBeVisible();
    await expect(page.locator(tid(R.download))).toHaveCount(0);
    await expectPageRendersCleanly(page);
  });

  test('no snapshot shows the original details as before; a refusal, a withdrawal and an error show no snapshot', async ({ page }) => {
    const plain = uuid(5);
    const refused = uuid(6);
    const withdrawn = uuid(7);
    const failed = uuid(8);
    await serve(page, {
      [plain]: { status: 200, json: detail(plain) },
      [withdrawn]: {
        status: 410,
        json: {
          id: withdrawn,
          origin_asset_uri: `https://studio-a.example/assets/${withdrawn}`,
          withdrawn_at: '2026-11-21T09:00:05Z',
          deleted: '2026-11-21T09:00:00Z',
          last_version: `https://studio-a.example/assets/${withdrawn}/versions/1`,
        },
      },
      [failed]: { status: 500, json: { error: 'upstream failed' } },
    });

    await page.goto(`/account/received/${plain}`);
    await expect(page.locator(tid(R.claimsTitle))).toHaveText('Declared details');
    await expect(page.locator(tid(R.claims))).toContainText('Harbour at dusk');
    await expect(page.locator(tid(R.metadata))).toHaveCount(0);
    await expect(page.locator(tid(R.originalActivity))).toHaveCount(0);
    await expectPageRendersCleanly(page);

    await page.goto(`/account/received/${refused}`);
    await expect(page.locator(tid(R.missing))).toBeVisible();
    await expect(page.locator(tid(R.metadata))).toHaveCount(0);

    await page.goto(`/account/received/${withdrawn}`);
    await expect(page.locator(tid(R.withdrawn))).toBeVisible();
    await expect(page.locator(tid(R.metadata))).toHaveCount(0);

    await page.goto(`/account/received/${failed}`);
    await expect(page.getByRole('alert')).toContainText('upstream failed');
    await expect(page.locator(tid(R.metadata))).toHaveCount(0);
  });
});
