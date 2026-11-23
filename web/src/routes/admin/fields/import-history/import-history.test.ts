// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1576 S4 /admin/fields/import-history: RENDERED-DOM assertions against
// a mocked API and a reactive router stub. What must hold: nothing loads
// without a resolved fields.admin; Older follows the server's cursor and
// Newer walks back the exact cursors used, with both ends disabled; the
// selected receipt lives in the URL and its detail renders every item in
// recorded order; a removed account, a missing field id and an unknown
// receipt are shown honestly; operator-typed text renders as text.

import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { auth } from '$stores/auth.svelte';

vi.mock('$app/state', async () => await import('../../../../../vitest-stubs/reactive-router.svelte'));
vi.mock('$app/navigation', async () => await import('../../../../../vitest-stubs/reactive-router.svelte'));
import { resetRouter, visits } from '../../../../../vitest-stubs/reactive-router.svelte';

const ID = (n: number) => `${String(n).padStart(8, '0')}-0000-4000-8000-000000000000`;
function summary(n: number, extra: Record<string, unknown> = {}) {
  return {
    receipt_id: ID(n),
    applied_at: '2026-11-22T07:00:00Z',
    actor_ref: 7,
    format: 'aa-field-schema/1',
    document_digest: 'sha256:' + 'c'.repeat(64),
    decisions_digest: 'sha256:' + 'd'.repeat(64),
    source_label: null,
    source_uri: null,
    changed: 1,
    field_count: 2,
    ...extra,
  };
}

const state = vi.hoisted(() => ({
  calls: [] as { path: string; query?: Record<string, unknown>; id?: string }[],
  pages: {} as Record<string, { items: unknown[]; next_cursor: string | null }>,
  details: {} as Record<string, unknown>,
}));

vi.mock('$api/client', () => ({
  api: {
    GET: async (path: string, opts?: { params?: { query?: Record<string, unknown>; path?: { receipt_id?: string } } }) => {
      if (path === '/fields/import/receipts') {
        const q = opts?.params?.query ?? {};
        state.calls.push({ path, query: q });
        const p = state.pages[String(q.cursor ?? '')];
        if (!p) return { error: { error: 'cursor is not a well-formed history cursor' }, response: { status: 400 } };
        return { data: p, response: { status: 200 } };
      }
      if (path === '/fields/import/receipts/{receipt_id}') {
        const id = opts?.params?.path?.receipt_id ?? '';
        state.calls.push({ path, id });
        const d = state.details[id];
        if (!d) return { error: { error: 'no such receipt' }, response: { status: 404 } };
        return { data: d, response: { status: 200 } };
      }
      return { error: { error: 'unexpected ' + path }, response: { status: 500 } };
    },
  },
}));

import Page from './+page.svelte';

beforeEach(() => {
  resetRouter('http://localhost/admin/fields/import-history');
  state.calls = [];
  state.pages = {
    '': { items: [summary(3), summary(2)], next_cursor: 'v1.page2' },
    'v1.page2': { items: [summary(1, { actor_ref: null, source_label: '<img src=x onerror=alert(1)>', changed: 0 })], next_cursor: null },
  };
  state.details = {
    [ID(1)]: {
      ...summary(1, { actor_ref: null, source_label: '<img src=x onerror=alert(1)>', source_uri: 'javascript:alert(1)', changed: 0 }),
      preview_id: '6f6c9a7e-5d1f-4c55-9a43-6f0a7d1c9b10',
      fields: [
        { code: 'zz_last_by_code', decision: 'skip', outcome: 'skipped', field_id: null, incoming_field_digest: 'sha256:' + 'e'.repeat(64) },
        { code: 'aa_first_by_code', decision: 'keep_local', outcome: 'kept_local', field_id: '11111111-2222-4333-8444-555555555555', incoming_field_digest: 'sha256:' + 'f'.repeat(64) },
      ],
    },
  };
  auth.ready = true;
  auth.capsStatus = 'resolved';
  auth.caps = ['fields.admin'];
});

afterEach(() => {
  auth.caps = [];
  auth.capsStatus = 'resolved';
  auth.ready = false;
});

const rows = () => screen.queryAllByTestId('import-history-row').map((r) => r.dataset.receipt);

describe('/admin/fields/import-history access', () => {
  for (const [name, setup, testid] of [
    ['not ready', () => (auth.ready = false), 'import-history-loading'],
    ['rights unknown', () => (auth.capsStatus = 'unavailable'), 'import-history-caps-unavailable'],
    ['no fields.admin', () => (auth.caps = ['system.audit.read']), 'import-history-denied'],
  ] as const) {
    it(`${name}: shows its state and loads nothing`, async () => {
      setup();
      render(Page);
      expect(screen.getByTestId(testid)).toBeTruthy();
      expect(screen.queryByTestId('import-history-list')).toBeNull();
      await new Promise((r) => setTimeout(r, 10));
      expect(state.calls).toEqual([]);
    });
  }
});

describe('/admin/fields/import-history paging', () => {
  it('follows the server cursor older and walks back the exact cursors newer', async () => {
    render(Page);
    await waitFor(() => expect(rows()).toEqual([ID(3), ID(2)]));
    expect(state.calls[0].query).toEqual({ limit: 25 });
    const older = screen.getByTestId('import-history-older') as HTMLButtonElement;
    const newer = screen.getByTestId('import-history-newer') as HTMLButtonElement;
    expect(newer.disabled).toBe(true);
    expect(older.disabled).toBe(false);
    await fireEvent.click(older);
    await waitFor(() => expect(rows()).toEqual([ID(1)]));
    expect(state.calls.at(-1)?.query).toEqual({ limit: 25, cursor: 'v1.page2' });
    expect(screen.getByTestId('import-history-page').textContent).toContain('Page 2');
    expect(older.disabled).toBe(true);
    expect(newer.disabled).toBe(false);
    await fireEvent.click(newer);
    await waitFor(() => expect(rows()).toEqual([ID(3), ID(2)]));
    expect(state.calls.at(-1)?.query).toEqual({ limit: 25 });
    expect(newer.disabled).toBe(true);
  });

  it('a refused cursor keeps the current page and says so, never restarting', async () => {
    state.pages[''].next_cursor = 'v1.unknown';
    render(Page);
    await waitFor(() => expect(rows()).toEqual([ID(3), ID(2)]));
    await fireEvent.click(screen.getByTestId('import-history-older'));
    await waitFor(() => expect(screen.getByTestId('import-history-error').textContent).toContain('cursor is not a well-formed history cursor'));
    expect(rows()).toEqual([ID(3), ID(2)]);
    expect(screen.getByTestId('import-history-page').textContent).toContain('Page 1');
  });

  it('an empty history says so', async () => {
    state.pages[''] = { items: [], next_cursor: null };
    render(Page);
    await waitFor(() => expect(screen.getByTestId('import-history-empty')).toBeTruthy());
  });
});

describe('/admin/fields/import-history detail', () => {
  it('opens a receipt through the URL and renders it as recorded, as text', async () => {
    const { container } = render(Page);
    await waitFor(() => expect(rows()).toEqual([ID(3), ID(2)]));
    await fireEvent.click(screen.getByTestId('import-history-older'));
    await waitFor(() => expect(rows()).toEqual([ID(1)]));
    const row = screen.getByTestId('import-history-row');
    expect(row.textContent).toContain('removed account');
    expect(row.textContent).toContain('<img src=x onerror=alert(1)>');
    await fireEvent.click(screen.getByTestId('import-history-open'));
    expect(visits.at(-1)).toBe(`/admin/fields/import-history?receipt=${ID(1)}`);
    await waitFor(() => expect(screen.getAllByTestId('import-history-item').length).toBe(2));
    expect(state.calls.at(-1)).toEqual({ path: '/fields/import/receipts/{receipt_id}', id: ID(1) });
    const items = screen.getAllByTestId('import-history-item');
    expect(items.map((i) => i.querySelector('code')?.textContent)).toEqual(['zz_last_by_code', 'aa_first_by_code']);
    expect(items[0].textContent).toContain('(none)');
    expect(items[1].textContent).toContain('11111111-2222-4333-8444-555555555555');
    expect(screen.getByTestId('import-history-source-label').textContent).toBe('<img src=x onerror=alert(1)>');
    expect(screen.getByTestId('import-history-source-uri').textContent).toBe('javascript:alert(1)');
    const detail = screen.getByTestId('import-history-detail');
    expect(detail.querySelector('img')).toBeNull();
    expect(detail.querySelector('a[href^="javascript"]')).toBeNull();
    expect(container.querySelector('a[href="javascript:alert(1)"]')).toBeNull();
    expect(detail.textContent).toContain('removed account');
  });

  it('a receipt id in the URL on arrival loads that receipt; an unknown one says so', async () => {
    resetRouter(`http://localhost/admin/fields/import-history?receipt=${ID(1)}`);
    const first = render(Page);
    await waitFor(() => expect(screen.getAllByTestId('import-history-item').length).toBe(2));
    first.unmount();
    resetRouter(`http://localhost/admin/fields/import-history?receipt=${ID(9)}`);
    render(Page);
    await waitFor(() => expect(screen.getByTestId('import-history-detail-error').textContent).toContain('No receipt has this id'));
  });
});
