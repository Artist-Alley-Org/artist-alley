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
