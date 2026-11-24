// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1574 /account/received and /account/received/[id]: RENDERED-DOM
// assertions against a mocked API. What must hold on the page: an entry
// links to its detail; the sender's claims are labelled declared and the
// detected type is shown beside the declared one; only a verified
// original's size and SHA-256 read as verified here, every other file's
// as declared; the download link is offered only when the server says
// the original is available, at the API's original URL; a refused record
// (404) renders "not available", never a partial page; a withdrawn one
// (410, #1575) renders the withdrawal notice and nothing of the record;
// and when the id
// changes under a mounted view (a client navigation between two received
// assets), the previous record and its link disappear at once and a late
// answer for the old id never overwrites the new one.

import { render, screen, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';

const ID = '7d6f1c2a-3b4e-4f5a-8b9c-0d1e2f3a4b5c';
const ID2 = '0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d';

function summary(id: string, name: string) {
  return {
    id,
    peer_id: '11111111-2222-4333-8444-555555555555',
    peer_url: 'https://studio-a.example',
    peer_name: 'Studio A',
    origin_asset_uri: `https://studio-a.example/assets/${id}`,
    version_uri: `https://studio-a.example/assets/${id}/versions/1`,
    version_number: 1,
    sensitivity: 'public',
    received_at: '2026-11-20T10:00:00Z',
    completed_at: '2026-11-20T10:01:00Z',
    original: {
      position: 0,
      role: 'original',
      sha256: 'ab'.repeat(32),
      size_bytes: 2048,
      declared_media_type: 'image/jpeg',
      declared_original_filename: name,
      detected_media_type: 'image/png',
      detected_by: 'net/http.DetectContentType',
      state: 'verified',
    },
  };
}

function detailOf(id: string, name: string, extra: Record<string, unknown> = {}) {
  const s = summary(id, name);
  return {
    asset: s,
    activity_uri: 'https://studio-a.example/activities/1',
    actor_uri: 'https://studio-a.example/users/alice',
    files: [s.original],
    declared: {},
    original_available: true,
    ...extra,
  };
}

type Answer = { data?: unknown; error?: unknown; response: { status: number } };
const ok = (data: unknown): Answer => ({ data, error: undefined, response: { status: 200 } });
const gone = (): Answer => ({ data: undefined, error: { error: 'received asset not found' }, response: { status: 404 } });

const state = vi.hoisted(() => ({
  list: [] as unknown[],
  // id -> the answer (or a promise of it) the detail GET returns.
  detail: new Map<string, Answer | Promise<Answer>>(),
  asked: [] as string[],
}));

vi.mock('$api/client', () => ({
  api: {
    GET: async (path: string, opts?: { params?: { path?: { id?: string } } }) => {
      if (path === '/federation/received-assets') {
        return { data: { items: state.list }, error: undefined, response: { status: 200 } };
      }
      const id = opts?.params?.path?.id ?? '';
      state.asked.push(id);
      return (await state.detail.get(id)) ?? gone();
    },
  },
}));

vi.mock('$app/state', () => ({ page: { params: { id: '7d6f1c2a-3b4e-4f5a-8b9c-0d1e2f3a4b5c' }, url: new URL('http://x/') } }));

vi.mock('$stores/lang.svelte', () => ({ t: (key: string) => key }));

import ListPage from './+page.svelte';
import DetailPage from './[id]/+page.svelte';
import ReceivedAssetView from '$lib/received/ReceivedAssetView.svelte';
import ReceivedMetadata from '$lib/received/ReceivedMetadata.svelte';
import type { ReceivedAssetCurrentMetadata } from '$lib/received/received';

function deferred(): { promise: Promise<Answer>; resolve: (a: Answer) => void } {
  let resolve!: (a: Answer) => void;
  const promise = new Promise<Answer>((r) => (resolve = r));
  return { promise, resolve };
}

beforeEach(() => {
  state.list = [];
  state.detail = new Map();
  state.asked = [];
});

describe('/account/received (rendered)', () => {
  it('links each received asset to its detail and names its sender', async () => {
    state.list = [summary(ID, 'harbour.jpg')];
    const { container } = render(ListPage);
    await waitFor(() => expect(container.querySelector('[data-testid="received-item"]')).not.toBeNull());
    const a = container.querySelector<HTMLAnchorElement>('[data-testid="received-item"]');
    expect(a?.getAttribute('href')).toBe(`/account/received/${ID}`);
    expect(a?.textContent).toContain('harbour.jpg');
    expect(a?.textContent).toContain('Studio A');
    expect(a?.textContent).toContain('image/png');
  });

  it('says so when nothing was received', async () => {
    render(ListPage);
    await waitFor(() => expect(screen.getByTestId('received-empty')).toBeTruthy());
  });
});

describe('/account/received/[id] (rendered)', () => {
  it('labels the claims, shows the detected type, and offers the original when available', async () => {
    state.detail.set(
      ID,
      ok(detailOf(ID, 'harbour.jpg', {
        declared: { rights: 'http://rightsstatements.org/vocab/InC/1.0/', description: { title: 'Harbour at dusk' } },
      })),
    );
    const { container } = render(DetailPage);
    await waitFor(() => expect(screen.getByTestId('received-title')).toBeTruthy());
    const download = container.querySelector<HTMLAnchorElement>('[data-testid="received-download"]');
    expect(download?.getAttribute('href')).toBe(`/api/v1/federation/received-assets/${ID}/original`);
    expect(screen.getByTestId('received-detected').textContent).toContain('image/png');
    expect(screen.getByTestId('received-files').textContent).toContain('image/jpeg');
    expect(screen.getByTestId('received-claims').textContent).toContain('rightsstatements.org');
    expect(container.textContent).toContain('account.received.declared_label');
    expect(screen.getByTestId('received-source').textContent).toBe(`https://studio-a.example/assets/${ID}`);
  });

  // #1577 S1: the baseline claims a peer sends are untrusted text. Markup
  // in them is never an element, and a URI-looking value is never a link.
  it('renders sent markup and URIs in the claims as plain text, never as elements or links', async () => {
    const title = '<img src=x onerror="window.__pwned=1"> Harbour';
    const statement = '<a href="javascript:alert(3)">Studio A</a>';
    state.detail.set(
      ID,
      ok(detailOf(ID, 'harbour.jpg', {
        declared: {
          rights: 'https://creativecommons.org/licenses/by/4.0/',
          description: { title, description: '<script>window.__pwned=2</script> javascript:alert(1)' },
          requiredStatement: { label: { en: ['Attribution'] }, value: { none: [statement] } },
        },
      })),
    );
    const { container } = render(DetailPage);
    await waitFor(() => expect(screen.getByTestId('received-claims')).toBeTruthy());
    const claims = screen.getByTestId('received-claims');
    expect(claims.querySelector('img, script, a, iframe, [onerror]')).toBeNull();
    expect(claims.querySelectorAll('*').length).toBe(claims.querySelectorAll('dt, dd').length);
    expect(claims.textContent).toContain('<img src=x onerror=');
    expect(claims.textContent).toContain('<script>window.__pwned=2</script>');
    expect(claims.textContent).toContain('javascript:alert(3)');
    expect(claims.textContent).toContain('https://creativecommons.org/licenses/by/4.0/');
    expect(container.querySelector('a[href^="javascript:"], a[href*="creativecommons.org"]')).toBeNull();
    expect((window as unknown as { __pwned?: number }).__pwned).toBeUndefined();
    // Labelled as the sender's declaration, with the sending studio and version shown.
    expect(container.textContent).toContain('account.received.declared_label');
    expect(container.textContent).toContain('Studio A');
    expect(container.textContent).toContain(`https://studio-a.example/assets/${ID}/versions/1`);
  });

  it('offers no download when the share does not include the original', async () => {
    state.detail.set(ID, ok(detailOf(ID, 'harbour.jpg', { original_available: false })));
    const { container } = render(DetailPage);
    await waitFor(() => expect(screen.getByTestId('received-download-unavailable')).toBeTruthy());
    expect(container.querySelector('[data-testid="received-download"]')).toBeNull();
  });

  it('renders a refused record as not available, with nothing of it shown', async () => {
    const { container } = render(DetailPage);
    await waitFor(() => expect(screen.getByTestId('received-missing')).toBeTruthy());
    expect(container.querySelector('[data-testid="received-title"]')).toBeNull();
    expect(container.querySelector('[data-testid="received-download"]')).toBeNull();
  });

  // #1575: a record the sending studio withdrew answers 410 with the
  // notice; the page shows the notice and nothing of the record.
  it('shows the withdrawal notice for a withdrawn record, and nothing of the record', async () => {
    state.detail.set(ID, {
      data: undefined,
      error: {
        id: ID,
        origin_asset_uri: `https://studio-a.example/assets/${ID}`,
        withdrawn_at: '2026-11-21T09:00:05Z',
        deleted: '2026-11-21T09:00:00Z',
        reason: 'Withdrawn at the artist\'s request',
        last_version: `https://studio-a.example/assets/${ID}/versions/1`,
      },
      response: { status: 410 },
    });
    const { container } = render(DetailPage);
    await waitFor(() => expect(screen.getByTestId('received-withdrawn')).toBeTruthy());
    expect(container.textContent).toContain('account.received.withdrawn_title');
    expect(screen.getByTestId('received-withdrawn-reason').textContent).toBe("Withdrawn at the artist's request");
    expect(container.textContent).toContain(`https://studio-a.example/assets/${ID}/versions/1`);
    for (const id of ['received-title', 'received-download', 'received-files', 'received-missing']) {
      expect(container.querySelector(`[data-testid="${id}"]`)).toBeNull();
    }
    expect(container.querySelector('[role="alert"]')).toBeNull();
  });

  it('shows no reason row when the sending studio gave none', async () => {
    state.detail.set(ID, {
      data: undefined,
      error: {
        id: ID,
        origin_asset_uri: `https://studio-a.example/assets/${ID}`,
        withdrawn_at: '2026-11-21T09:00:05Z',
        deleted: '2026-11-21T09:00:00Z',
        last_version: `https://studio-a.example/assets/${ID}/versions/1`,
      },
      response: { status: 410 },
    });
    const { container } = render(DetailPage);
    await waitFor(() => expect(screen.getByTestId('received-withdrawn')).toBeTruthy());
    expect(container.querySelector('[data-testid="received-withdrawn-reason"]')).toBeNull();
  });

  it("shows a verified original's size and SHA-256 as verified, a not-transferred file's as declared", async () => {
    const d = detailOf(ID, 'harbour.jpg');
    const preservation = {
      position: 1,
      role: 'preservation',
      sha256: 'cd'.repeat(32),
      size_bytes: 4096,
      declared_media_type: 'image/tiff',
      state: 'not_transferred',
    };
    d.files = [d.asset.original, preservation] as unknown as typeof d.files;
    state.detail.set(ID, ok(d));
    const { container } = render(DetailPage);
    await waitFor(() => expect(screen.getByTestId('received-files')).toBeTruthy());
    const rows = [...container.querySelectorAll<HTMLElement>('[data-testid="received-file"]')];
    expect(rows.map((r) => [r.dataset.role, r.dataset.verified])).toEqual([
      ['original', 'true'],
      ['preservation', 'false'],
    ]);
    const basis = (row: HTMLElement, which: string) =>
      row.querySelector(`[data-testid="received-${which}-basis"]`)?.textContent?.trim();
    expect(basis(rows[0], 'size')).toBe('account.received.value_verified');
    expect(basis(rows[0], 'sha256')).toBe('account.received.value_verified');
    // The claimed checksum and size of a file never transferred are not
    // presented as a fixity result.
    expect(rows[1].textContent).toContain('cd'.repeat(32));
    expect(basis(rows[1], 'size')).toBe('account.received.value_declared');
    expect(basis(rows[1], 'sha256')).toBe('account.received.value_declared');
    expect(rows[1].textContent).not.toContain('account.received.value_verified');
    expect(rows[1].querySelector('[data-testid="received-detected"]')?.textContent?.trim()).toBe(
      'account.received.not_detected',
    );
    expect(rows[1].textContent).toContain('account.received.state_not_transferred');
    expect(container.textContent).toContain('account.received.files_note');
  });
});

describe('ReceivedAssetView when the id changes (client navigation)', () => {
  it('clears the previous record and its link at once, then shows the new one', async () => {
    state.detail.set(ID, ok(detailOf(ID, 'first.jpg')));
    const second = deferred();
    state.detail.set(ID2, second.promise);
    const { container, rerender } = render(ReceivedAssetView, { props: { id: ID } });
    await waitFor(() => expect(screen.getByTestId('received-title').textContent).toContain('first.jpg'));

    await rerender({ id: ID2 });
    // While the new record loads, nothing of the old one remains.
    await waitFor(() => expect(screen.getByTestId('received-loading')).toBeTruthy());
    expect(container.querySelector('[data-testid="received-title"]')).toBeNull();
    expect(container.querySelector('[data-testid="received-download"]')).toBeNull();
    expect(container.textContent).not.toContain('first.jpg');

    second.resolve(ok(detailOf(ID2, 'second.jpg')));
    await waitFor(() => expect(screen.getByTestId('received-title').textContent).toContain('second.jpg'));
    expect(container.querySelector('[data-testid="received-download"]')?.getAttribute('href')).toBe(
      `/api/v1/federation/received-assets/${ID2}/original`,
    );
    expect(state.asked).toEqual([ID, ID2]);
  });

  it('never lets a late answer for the old id overwrite the new one', async () => {
    const first = deferred();
    state.detail.set(ID, first.promise);
    state.detail.set(ID2, ok(detailOf(ID2, 'second.jpg')));
    const { container, rerender } = render(ReceivedAssetView, { props: { id: ID } });
    await waitFor(() => expect(state.asked).toEqual([ID]));

    await rerender({ id: ID2 });
    await waitFor(() => expect(screen.getByTestId('received-title').textContent).toContain('second.jpg'));

    first.resolve(ok(detailOf(ID, 'first.jpg')));
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.getByTestId('received-title').textContent).toContain('second.jpg');
    expect(container.textContent).not.toContain('first.jpg');
    expect(container.querySelector('[data-testid="received-download"]')?.getAttribute('href')).toBe(
      `/api/v1/federation/received-assets/${ID2}/original`,
    );
  });

  it('clears a shown record when the next one is refused (revoked or not granted)', async () => {
    state.detail.set(ID, ok(detailOf(ID, 'first.jpg')));
    const { container, rerender } = render(ReceivedAssetView, { props: { id: ID } });
    await waitFor(() => expect(screen.getByTestId('received-title')).toBeTruthy());

    await rerender({ id: ID2 }); // no answer set: 404
    await waitFor(() => expect(screen.getByTestId('received-missing')).toBeTruthy());
    expect(container.querySelector('[data-testid="received-title"]')).toBeNull();
    expect(container.querySelector('[data-testid="received-download"]')).toBeNull();
    expect(container.querySelector('[data-testid="received-files"]')).toBeNull();
    expect(container.textContent).not.toContain('first.jpg');
  });

  it('a record refused on reload (revoked since) shows nothing of it', async () => {
    state.detail.set(ID, ok(detailOf(ID, 'first.jpg')));
    const one = render(ReceivedAssetView, { props: { id: ID } });
    await waitFor(() => expect(screen.getByTestId('received-title')).toBeTruthy());
    one.unmount();

    state.detail.set(ID, gone());
    const { container } = render(ReceivedAssetView, { props: { id: ID } });
    await waitFor(() => expect(screen.getByTestId('received-missing')).toBeTruthy());
    expect(container.querySelector('[data-testid="received-title"]')).toBeNull();
    expect(container.querySelector('[data-testid="received-download"]')).toBeNull();
  });
});

// #1577: the asset's latest received metadata (current_metadata), beside
// the original declared details, never in place of them. RENDERED-DOM
// assertions against a mocked API with generated-shaped fixtures; they say
// nothing about admission or signatures.
const PUBLISHED = '2026-11-23T10:00:00.123456789Z';

function currentOf(snapshot: Record<string, unknown> = {}, extra: Record<string, unknown> = {}) {
  return {
    profile: 'asset-metadata-1',
    snapshot: {
      title: 'Harbour at dawn',
      description: 'Line one\n  Line two  ',
      rights: 'https://creativecommons.org/licenses/by/4.0/',
      requiredStatement: { label: { en: ['Attribution'] }, value: { none: ['Studio A'] } },
      ...snapshot,
    },
    revision_id: '99999999-8888-4777-8666-555555555555',
    peer_id: '11111111-2222-4333-8444-555555555555',
    origin_asset_uri: `https://studio-a.example/assets/${ID}`,
    activity_uri: 'https://studio-a.example/activities/metadata-2',
    actor_uri: 'https://studio-a.example/users/bob',
    version_uri: `https://studio-a.example/assets/${ID}/versions/1`,
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

const PAST_DUSK = { rights: 'http://rightsstatements.org/vocab/InC/1.0/', description: { title: 'Harbour at dusk' } };

const q = (root: ParentNode, id: string) => root.querySelector<HTMLElement>(`[data-testid="${id}"]`);
const panelOf = (root: ParentNode) => q(root, 'received-metadata');
const claimOf = (panel: HTMLElement, name: string) =>
  panel.querySelector<HTMLElement>(`[data-testid="received-metadata-claim"][data-claim="${name}"]`);
const valueOf = (panel: HTMLElement, name: string) =>
  claimOf(panel, name)?.querySelector<HTMLElement>('[data-testid="received-metadata-value"]')?.textContent ?? null;
const emptyIn = (panel: HTMLElement, name: string) =>
  claimOf(panel, name)?.querySelector('[data-testid="received-metadata-empty"]') ?? null;
const fieldOf = (panel: HTMLElement, name: string) =>
  panel.querySelector<HTMLElement>(`[data-testid="received-metadata-field"][data-field="${name}"]`)?.textContent ?? null;
/** Each language group of one statement part: [tag, its texts in order]. */
const statementPart = (panel: HTMLElement, part: 'label' | 'value') =>
  [...(claimOf(panel, 'requiredStatement')?.querySelectorAll<HTMLElement>(
    `[data-testid="received-metadata-statement-${part}"] [data-testid="received-metadata-lang"]`,
  ) ?? [])].map((g) => [
    g.dataset.lang,
    [...g.querySelectorAll<HTMLElement>('[data-testid="received-metadata-text"]')].map((x) => x.textContent),
  ]);

describe('/account/received/[id] latest received metadata (rendered, #1577)', () => {
  it('shows the latest snapshot apart from the original declared details, each with its own provenance', async () => {
    state.detail.set(ID, ok(detailOf(ID, 'harbour.jpg', { declared: PAST_DUSK, current_metadata: currentOf() })));
    const { container } = render(DetailPage);
    await waitFor(() => expect(panelOf(container)).not.toBeNull());
    const panel = panelOf(container)!;
    expect(panel.textContent).toContain('account.received.metadata.title');
    expect(panel.textContent).toContain('account.received.metadata.note');
    expect(q(panel, 'received-metadata-peer')?.textContent).toBe('Studio A');
    expect(valueOf(panel, 'title')).toBe('Harbour at dawn');
    expect(valueOf(panel, 'description')).toBe('Line one\n  Line two  ');
    expect(valueOf(panel, 'rights')).toBe('https://creativecommons.org/licenses/by/4.0/');
    expect(statementPart(panel, 'label')).toEqual([['en', ['Attribution']]]);
    expect(statementPart(panel, 'value')).toEqual([['none', ['Studio A']]]);
    expect(panel.textContent).toContain('account.received.metadata.no_language');
    // The original claims are still there, as history, after the panel.
    expect(panel.textContent).not.toContain('Harbour at dusk');
    const claims = screen.getByTestId('received-claims');
    expect(claims.textContent).toContain('Harbour at dusk');
    expect(panel.compareDocumentPosition(claims) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByTestId('received-claims-title').textContent).toBe('account.received.metadata.original_title');
    expect(q(container, 'received-original-activity')?.textContent).toBe('https://studio-a.example/activities/1');
    expect(q(container, 'received-original-actor')?.textContent).toBe('https://studio-a.example/users/alice');
    // The snapshot's own provenance, exact.
    const published = q(panel, 'received-metadata-published')!;
    expect(published.textContent).toBe(PUBLISHED);
    expect(published.querySelector('time')?.getAttribute('datetime')).toBe(PUBLISHED);
    expect(fieldOf(panel, 'revision')).toBe('99999999-8888-4777-8666-555555555555');
    expect(fieldOf(panel, 'activity')).toBe('https://studio-a.example/activities/metadata-2');
    expect(fieldOf(panel, 'actor')).toBe('https://studio-a.example/users/bob');
    expect(fieldOf(panel, 'bound_version')).toBe(`https://studio-a.example/assets/${ID}/versions/1`);
    expect(fieldOf(panel, 'file_set')).toBe('0'.repeat(64));
    expect(fieldOf(panel, 'origin_key')).toBe('https://studio-a.example/users/bob#main-key');
    expect(fieldOf(panel, 'signature_algorithm')).toBe('Ed25519');
    expect(fieldOf(panel, 'origin_signature')).toBe('c2lnbmVkIGJ5IHN0dWRpbyBh');
    expect(fieldOf(panel, 'recipient_key')).toBe('https://receiver.example/actor#main-key');
    expect(fieldOf(panel, 'received_at')).toBe('2026-11-23T10:00:05.654321Z');
    expect(fieldOf(panel, 'origin_key')).not.toBe(fieldOf(panel, 'recipient_key'));
    expect(panel.querySelector('details')).not.toBeNull();
    // Provenance is not a verification of the claims.
    expect(panel.textContent).not.toContain('account.received.value_verified');
  });

  it('shows an explicit null as not supplied, never the original value, and four nulls as four markers', async () => {
    state.detail.set(
      ID,
      ok(detailOf(ID, 'harbour.jpg', { declared: PAST_DUSK, current_metadata: currentOf({ title: null, requiredStatement: null }) })),
    );
    const one = render(ReceivedAssetView, { props: { id: ID } });
    await waitFor(() => expect(panelOf(one.container)).not.toBeNull());
    let panel = panelOf(one.container)!;
    expect(emptyIn(panel, 'title')?.textContent).toBe('account.received.metadata.empty_value');
    expect(emptyIn(panel, 'requiredStatement')?.textContent).toBe('account.received.metadata.empty_value');
    expect(valueOf(panel, 'description')).toBe('Line one\n  Line two  ');
    expect(emptyIn(panel, 'description')).toBeNull();
    expect(panel.textContent).not.toContain('Harbour at dusk');
    one.unmount();

    const clear = { title: null, description: null, rights: null, requiredStatement: null };
    state.detail.set(ID, ok(detailOf(ID, 'harbour.jpg', { declared: PAST_DUSK, current_metadata: currentOf(clear) })));
    const { container } = render(ReceivedAssetView, { props: { id: ID } });
    await waitFor(() => expect(panelOf(container)).not.toBeNull());
    panel = panelOf(container)!;
    for (const name of ['title', 'description', 'rights', 'requiredStatement']) {
      expect(emptyIn(panel, name)?.textContent, name).toBe('account.received.metadata.empty_value');
    }
    expect(panel.querySelectorAll('[data-testid="received-metadata-claim"]').length).toBe(4);
    expect(panel.textContent).not.toContain('Harbour at dusk');
    expect(panel.textContent).not.toContain('rightsstatements.org');
    expect(screen.getByTestId('received-claims').textContent).toContain('Harbour at dusk');
  });

  it('without a current snapshot shows no panel and the original details as before', async () => {
    state.detail.set(ID, ok(detailOf(ID, 'harbour.jpg', { declared: PAST_DUSK })));
    const { container } = render(DetailPage);
    await waitFor(() => expect(screen.getByTestId('received-claims')).toBeTruthy());
    expect(panelOf(container)).toBeNull();
    expect(screen.getByTestId('received-claims-title').textContent).toBe('account.received.declared_claims');
    expect(q(container, 'received-original-activity')).toBeNull();
    expect(container.textContent).not.toContain('account.received.metadata.');
  });

  it('shows every language and every text of the required statement, in order, Unicode and RTL intact', async () => {
    const rtl = 'استوديو‏ أ';
    const statement = {
      label: { fr: ['Crédit'], en: ['Credit', 'Second label'] },
      value: { none: ['Studio A', 'line\nbreak'], ar: [rtl], 'zh-Hant': ['工作室 🎨'] },
    };
    state.detail.set(ID, ok(detailOf(ID, 'harbour.jpg', { current_metadata: currentOf({ requiredStatement: statement, title: 'Café‮ 東京' }) })));
    const { container } = render(DetailPage);
    await waitFor(() => expect(panelOf(container)).not.toBeNull());
    const panel = panelOf(container)!;
    expect(statementPart(panel, 'label')).toEqual([['en', ['Credit', 'Second label']], ['fr', ['Crédit']]]);
    expect(statementPart(panel, 'value')).toEqual([['ar', [rtl]], ['none', ['Studio A', 'line\nbreak']], ['zh-Hant', ['工作室 🎨']]]);
    expect(valueOf(panel, 'title')).toBe('Café‮ 東京');
    for (const el of panel.querySelectorAll<HTMLElement>('[data-testid="received-metadata-text"], [data-testid="received-metadata-value"]')) {
      expect(el.getAttribute('dir')).toBe('auto');
    }
  });

  it('renders hostile text in every claim, statement and provenance field literally, with no element, link or request', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    const evil = (n: number) => `<img src=https://evil.example/${n}.png onerror="window.__pwnedMeta=${n}"><script>window.__pwnedMeta=${n}</script>`;
    const snapshot = {
      title: evil(1),
      description: `${evil(2)} [x](javascript:alert(2)) **bold**`,
      rights: 'javascript:alert(3)//https://creativecommons.org/licenses/by/4.0/',
      requiredStatement: {
        label: { en: [evil(4)] },
        value: { none: ['<a href="https://evil.example/5">Studio A</a>', '<iframe src="https://evil.example/6"></iframe><style>*{display:none}</style>'] },
      },
    };
    const provenance = {
      activity_uri: 'javascript:alert(7)',
      actor_uri: '<a href="https://evil.example/8">bob</a>',
      version_uri: 'https://evil.example/9.png',
      origin_signer_key_id: '<svg onload="window.__pwnedMeta=10"></svg>',
      recipient_signer_key_id: 'https://receiver.example/actor#main-key',
    };
    const fixture = detailOf(ID, 'harbour.jpg', { current_metadata: currentOf(snapshot, provenance) });
    const before = JSON.stringify(fixture);
    state.detail.set(ID, ok(fixture));
    const { container } = render(DetailPage);
    await waitFor(() => expect(panelOf(container)).not.toBeNull());
    const panel = panelOf(container)!;
    expect(panel.querySelectorAll('img, script, iframe, style, a, svg, object, embed, link, form, video, audio, source, math').length).toBe(0);
    for (const el of panel.querySelectorAll('*')) {
      for (const attr of el.getAttributeNames()) {
        expect(/^(on|href$|src$|srcset$|action$|formaction$|xlink:href$|style$)/.test(attr), `${el.tagName} ${attr}`).toBe(false);
      }
    }
    expect(valueOf(panel, 'title')).toBe(evil(1));
    expect(valueOf(panel, 'description')).toBe(`${evil(2)} [x](javascript:alert(2)) **bold**`);
    expect(valueOf(panel, 'rights')).toBe('javascript:alert(3)//https://creativecommons.org/licenses/by/4.0/');
    expect(statementPart(panel, 'label')).toEqual([['en', [evil(4)]]]);
    expect(statementPart(panel, 'value')).toEqual([['none', snapshot.requiredStatement.value.none]]);
    expect(fieldOf(panel, 'activity')).toBe('javascript:alert(7)');
    expect(fieldOf(panel, 'actor')).toBe('<a href="https://evil.example/8">bob</a>');
    expect(fieldOf(panel, 'bound_version')).toBe('https://evil.example/9.png');
    expect(fieldOf(panel, 'origin_key')).toBe('<svg onload="window.__pwnedMeta=10"></svg>');
    expect((window as unknown as { __pwnedMeta?: number }).__pwnedMeta).toBeUndefined();
    expect(fetchSpy).not.toHaveBeenCalled();
    fetchSpy.mockRestore();
    // The page's own shell and download link stay links, outside the panel.
    expect(container.querySelector('[data-testid="received-download"]')?.getAttribute('href')).toBe(
      `/api/v1/federation/received-assets/${ID}/original`,
    );
    expect(JSON.stringify(fixture)).toBe(before);
  });

  it('keeps a legacy-valid oversized rights URI and required statement whole', async () => {
    // The same legal shapes the API's own fixtures self-validate (rev744):
    // a 2242-byte pattern-valid CC URI, nine distinct 4096-byte texts.
    const rights = 'https://creativecommons.org/licenses/by/4' + '.0'.repeat(1100) + '/';
    const texts = Array.from({ length: 9 }, (_, i) => {
      const p = `part ${i + 1} of 9: `;
      return p + String.fromCharCode(97 + i).repeat(4096 - p.length);
    });
    state.detail.set(
      ID,
      ok(detailOf(ID, 'harbour.jpg', {
        current_metadata: currentOf({
          title: 'T'.repeat(1024),
          description: 'd'.repeat(4096),
          rights,
          requiredStatement: { label: { en: ['Credit'] }, value: { en: texts } },
        }),
      })),
    );
    const { container } = render(DetailPage);
    await waitFor(() => expect(panelOf(container)).not.toBeNull());
    const panel = panelOf(container)!;
    expect(rights.length).toBe(2242);
    expect(valueOf(panel, 'rights')).toBe(rights);
    expect(valueOf(panel, 'title')).toBe('T'.repeat(1024));
    expect(valueOf(panel, 'description')).toBe('d'.repeat(4096));
    expect(statementPart(panel, 'value')).toEqual([['en', texts]]);
  });

  it("a snapshot bound to an older version leaves the current version's files and download as they are", async () => {
    const d = detailOf(ID, 'harbour-v2.png', {
      current_metadata: currentOf({}, { version_uri: `https://studio-a.example/assets/${ID}/versions/1`, file_set_sha256: '1'.repeat(64) }),
    });
    d.asset.version_number = 2;
    d.asset.version_uri = `https://studio-a.example/assets/${ID}/versions/2`;
    state.detail.set(ID, ok(d));
    const { container } = render(DetailPage);
    await waitFor(() => expect(panelOf(container)).not.toBeNull());
    const panel = panelOf(container)!;
    expect(fieldOf(panel, 'bound_version')).toBe(`https://studio-a.example/assets/${ID}/versions/1`);
    expect(screen.getByTestId('received-title').textContent).toContain('harbour-v2.png');
    expect(container.textContent).toContain(`https://studio-a.example/assets/${ID}/versions/2`);
    const rows = container.querySelectorAll('[data-testid="received-file"]');
    expect(rows.length).toBe(1);
    expect(rows[0].textContent).toContain('ab'.repeat(32));
    expect(container.querySelector('[data-testid="received-download"]')?.getAttribute('href')).toBe(
      `/api/v1/federation/received-assets/${ID}/original`,
    );
  });

  it('a view-only share still shows the snapshot, and still offers no download', async () => {
    state.detail.set(ID, ok(detailOf(ID, 'harbour.jpg', { original_available: false, current_metadata: currentOf() })));
    const { container } = render(DetailPage);
    await waitFor(() => expect(panelOf(container)).not.toBeNull());
    expect(valueOf(panelOf(container)!, 'title')).toBe('Harbour at dawn');
    expect(screen.getByTestId('received-download-unavailable')).toBeTruthy();
    expect(container.querySelector('[data-testid="received-download"]')).toBeNull();
  });
});

describe('ReceivedAssetView latest received metadata when the id changes (#1577)', () => {
  const fail = (): Answer => ({ data: undefined, error: { error: 'upstream failed' }, response: { status: 500 } });
  const withdrawnAnswer = (): Answer => ({
    data: undefined,
    error: {
      id: ID2,
      origin_asset_uri: `https://studio-a.example/assets/${ID2}`,
      withdrawn_at: '2026-11-21T09:00:05Z',
      deleted: '2026-11-21T09:00:00Z',
      last_version: `https://studio-a.example/assets/${ID2}/versions/1`,
    },
    response: { status: 410 },
  });
  const newest = (): Answer => ok(detailOf(ID2, 'second.jpg', { current_metadata: currentOf({ title: 'Newest title' }) }));
  const outcomes: [string, () => Answer, string][] = [
    ['a newer 200', newest, 'received-title'],
    ['a 404', gone, 'received-missing'],
    ['a 410', withdrawnAnswer, 'received-withdrawn'],
    ['an error', fail, ''],
  ];

  for (const [name, answer, shown] of outcomes) {
    it(`clears the old snapshot at once, and then shows only ${name}`, async () => {
      state.detail.set(ID, ok(detailOf(ID, 'first.jpg', { current_metadata: currentOf({ title: 'Old title' }) })));
      const next = deferred();
      state.detail.set(ID2, next.promise);
      const { container, rerender } = render(ReceivedAssetView, { props: { id: ID } });
      await waitFor(() => expect(panelOf(container)).not.toBeNull());

      await rerender({ id: ID2 });
      await waitFor(() => expect(screen.getByTestId('received-loading')).toBeTruthy());
      expect(panelOf(container)).toBeNull();
      expect(container.textContent).not.toContain('Old title');

      next.resolve(answer());
      if (shown) await waitFor(() => expect(q(container, shown)).not.toBeNull());
      else await waitFor(() => expect(container.querySelector('[role="alert"]')).not.toBeNull());
      expect(container.textContent).not.toContain('Old title');
      if (name === 'a newer 200') expect(valueOf(panelOf(container)!, 'title')).toBe('Newest title');
      else expect(panelOf(container)).toBeNull();
    });

    it(`never lets a late snapshot for the old id replace ${name}`, async () => {
      const late = deferred();
      state.detail.set(ID, late.promise);
      state.detail.set(ID2, answer());
      const { container, rerender } = render(ReceivedAssetView, { props: { id: ID } });
      await waitFor(() => expect(state.asked).toEqual([ID]));
      await rerender({ id: ID2 });
      if (shown) await waitFor(() => expect(q(container, shown)).not.toBeNull());
      else await waitFor(() => expect(container.querySelector('[role="alert"]')).not.toBeNull());

      late.resolve(ok(detailOf(ID, 'first.jpg', { current_metadata: currentOf({ title: 'Late old title' }) })));
      await new Promise((r) => setTimeout(r, 20));
      expect(container.textContent).not.toContain('Late old title');
      if (name === 'a newer 200') expect(valueOf(panelOf(container)!, 'title')).toBe('Newest title');
      else expect(panelOf(container)).toBeNull();
    });
  }
});

describe('ReceivedMetadata when its snapshot is replaced (#1577)', () => {
  it('follows a replaced snapshot at once, down to four explicit nulls', async () => {
    const meta = (snapshot: Record<string, unknown>, extra: Record<string, unknown> = {}) =>
      currentOf(snapshot, extra) as unknown as ReceivedAssetCurrentMetadata;
    const { container, rerender } = render(ReceivedMetadata, { props: { metadata: meta({}), peerName: 'Studio A' } });
    expect(valueOf(panelOf(container)!, 'title')).toBe('Harbour at dawn');
    expect(statementPart(panelOf(container)!, 'label')).toEqual([['en', ['Attribution']]]);

    await rerender({
      metadata: meta(
        { title: null, description: null, rights: null, requiredStatement: null },
        { revision_id: '22222222-3333-4444-8555-666666666666', published: '2026-11-24T00:00:00.000000001Z' },
      ),
      peerName: 'Studio B',
    });
    const panel = panelOf(container)!;
    for (const name of ['title', 'description', 'rights', 'requiredStatement']) {
      expect(emptyIn(panel, name)?.textContent, name).toBe('account.received.metadata.empty_value');
      expect(claimOf(panel, name)?.querySelector('[data-testid="received-metadata-value"], [data-testid="received-metadata-text"]')).toBeNull();
    }
    expect(panel.textContent).not.toContain('Harbour at dawn');
    expect(panel.textContent).not.toContain('Attribution');
    expect(q(panel, 'received-metadata-peer')?.textContent).toBe('Studio B');
    expect(fieldOf(panel, 'revision')).toBe('22222222-3333-4444-8555-666666666666');
    expect(q(panel, 'received-metadata-published')?.textContent).toBe('2026-11-24T00:00:00.000000001Z');
  });
});
