// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1576 S4 correction round 1 (rev705): regressions written BEFORE the
// fix. The history belongs to one signed-in operator, a failed read can
// be retried at the same position, and loading is visible.

import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { auth } from '$stores/auth.svelte';

vi.mock('$app/state', async () => await import('../../../../../vitest-stubs/reactive-router.svelte'));
vi.mock('$app/navigation', async () => await import('../../../../../vitest-stubs/reactive-router.svelte'));
import { resetRouter } from '../../../../../vitest-stubs/reactive-router.svelte';

const ID = (n: number) => `${String(n).padStart(8, '0')}-0000-4000-8000-000000000000`;
const summary = (n: number) => ({
  receipt_id: ID(n),
  applied_at: '2026-11-22T07:00:00Z',
  actor_ref: 7,
  format: 'aa-field-schema/1',
  document_digest: 'sha256:' + 'c'.repeat(64),
  decisions_digest: 'sha256:' + 'd'.repeat(64),
  source_label: null,
  source_uri: null,
  changed: 1,
  field_count: 1,
});

const state = vi.hoisted(() => ({
  calls: [] as { path: string; query?: Record<string, unknown>; id?: string }[],
  fail: 0,
  hold: null as null | Promise<void>,
}));

vi.mock('$api/client', () => ({
  api: {
    GET: async (path: string, opts?: { params?: { query?: Record<string, unknown>; path?: { receipt_id?: string } } }) => {
      state.calls.push({ path, query: opts?.params?.query, id: opts?.params?.path?.receipt_id });
      if (state.hold) await state.hold;
      if (state.fail > 0) {
        state.fail--;
        return { error: { error: 'internal error' }, response: { status: 500 } };
      }
      if (path === '/fields/import/receipts') {
        const c = opts?.params?.query?.cursor;
        return c
          ? { data: { items: [summary(1)], next_cursor: null }, response: { status: 200 } }
          : { data: { items: [summary(3), summary(2)], next_cursor: 'v1.p2' }, response: { status: 200 } };
      }
      const id = opts?.params?.path?.receipt_id ?? '';
      return {
        data: {
          ...summary(Number(id.slice(0, 8))),
          preview_id: '6f6c9a7e-5d1f-4c55-9a43-6f0a7d1c9b10',
          fields: [{ code: 'aa', decision: 'create', outcome: 'created', field_id: null, incoming_field_digest: 'sha256:' + 'e'.repeat(64) }],
        },
        response: { status: 200 },
      };
    },
  },
}));

import Page from './+page.svelte';

beforeEach(() => {
  resetRouter('http://localhost/admin/fields/import-history');
  state.calls = [];
  state.fail = 0;
  state.hold = null;
  auth.ready = true;
  auth.capsStatus = 'resolved';
  auth.caps = ['fields.admin'];
  auth.user = { ref: 1, username: 'alice', matureContentAllowed: false, matureOptedIn: false };
});

afterEach(() => {
  auth.caps = [];
  auth.user = null;
  auth.ready = false;
});

const rows = () => screen.queryAllByTestId('import-history-row').map((r) => r.dataset.receipt);

describe('history corrections', () => {
  it('shows loading while the first page is pending', async () => {
    let release: () => void = () => {};
    state.hold = new Promise((r) => (release = r));
    render(Page);
    await waitFor(() => expect(screen.getByTestId('import-history-list-loading')).toBeTruthy());
    state.hold = null;
    release();
    await waitFor(() => expect(rows()).toEqual([ID(3), ID(2)]));
  });

  it('a failed page read keeps the current page and Retry asks for the same cursor', async () => {
    render(Page);
    await waitFor(() => expect(rows()).toEqual([ID(3), ID(2)]));
    state.fail = 1;
    await fireEvent.click(screen.getByTestId('import-history-older'));
    await waitFor(() => expect(screen.getByTestId('import-history-error')).toBeTruthy());
    expect(rows()).toEqual([ID(3), ID(2)]);
    await fireEvent.click(screen.getByTestId('import-history-retry'));
    await waitFor(() => expect(rows()).toEqual([ID(1)]));
    expect(state.calls.at(-1)?.query).toEqual({ limit: 25, cursor: 'v1.p2' });
  });

  it('a failed detail read can be retried for the same id', async () => {
    resetRouter(`http://localhost/admin/fields/import-history?receipt=${ID(3)}`);
    state.fail = 2;
    render(Page);
    await waitFor(() => expect(screen.getByTestId('import-history-detail-error')).toBeTruthy());
    await fireEvent.click(screen.getByTestId('import-history-detail-retry'));
    await waitFor(() => expect(screen.getAllByTestId('import-history-item').length).toBe(1));
    expect(state.calls.at(-1)?.id).toBe(ID(3));
  });

  it('a change of account clears the history and reloads it fresh', async () => {
    resetRouter(`http://localhost/admin/fields/import-history?receipt=${ID(3)}`);
    render(Page);
    await waitFor(() => expect(screen.getAllByTestId('import-history-item').length).toBe(1));
    let release: () => void = () => {};
    state.hold = new Promise((r) => (release = r));
    const before = state.calls.length;
    auth.user = { ref: 2, username: 'bob', matureContentAllowed: false, matureOptedIn: false };
    await waitFor(() => expect(state.calls.length).toBeGreaterThan(before));
    expect(rows()).toEqual([]);
    expect(screen.queryAllByTestId('import-history-item')).toEqual([]);
    state.hold = null;
    release();
    await waitFor(() => expect(rows()).toEqual([ID(3), ID(2)]));
  });

  it('losing the capability hides and clears the history', async () => {
    render(Page);
    await waitFor(() => expect(rows()).toEqual([ID(3), ID(2)]));
    auth.caps = [];
    await waitFor(() => expect(screen.getByTestId('import-history-denied')).toBeTruthy());
    state.hold = new Promise(() => {});
    auth.caps = ['fields.admin'];
    await waitFor(() => expect(screen.getByTestId('import-history-list')).toBeTruthy());
    expect(rows()).toEqual([]);
  });
});

describe('history corrections, further', () => {
  it('a failed first page can be retried, and nothing is shown as loaded meanwhile', async () => {
    state.fail = 1;
    render(Page);
    await waitFor(() => expect(screen.getByTestId('import-history-error')).toBeTruthy());
    expect(screen.queryByTestId('import-history-empty')).toBeNull();
    await fireEvent.click(screen.getByTestId('import-history-retry'));
    await waitFor(() => expect(rows()).toEqual([ID(3), ID(2)]));
    expect(state.calls.at(-1)?.query).toEqual({ limit: 25 });
  });

  it('choosing another receipt while one is loading shows only the last chosen', async () => {
    render(Page);
    await waitFor(() => expect(rows()).toEqual([ID(3), ID(2)]));
    let release: () => void = () => {};
    state.hold = new Promise((r) => (release = r));
    const opens = screen.getAllByTestId('import-history-open');
    await fireEvent.click(opens[0]);
    await fireEvent.click(opens[1]);
    state.hold = null;
    release();
    await waitFor(() => expect(screen.getAllByTestId('import-history-item').length).toBe(1));
    expect(screen.getByTestId('import-history-detail').textContent).toContain(ID(2));
    expect(screen.getByTestId('import-history-past-note')).toBeTruthy();
    expect(screen.getByTestId('import-history-item-digest').textContent).toBe('sha256:' + 'e'.repeat(64));
  });

  it('Newest is offered from the second page', async () => {
    render(Page);
    await waitFor(() => expect(rows()).toEqual([ID(3), ID(2)]));
    expect(screen.queryByTestId('import-history-newest')).toBeNull();
    await fireEvent.click(screen.getByTestId('import-history-older'));
    await waitFor(() => expect(rows()).toEqual([ID(1)]));
    await fireEvent.click(screen.getByTestId('import-history-newest'));
    await waitFor(() => expect(rows()).toEqual([ID(3), ID(2)]));
  });
});
