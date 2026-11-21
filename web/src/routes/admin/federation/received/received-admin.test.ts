// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1574 slice 2 /admin/federation/received: RENDERED-DOM assertions
// against a mocked API. What must hold: every record state is listed;
// a ledger event shows what was listed against what arrived; events of a
// removed record or peer are still shown and marked; an overlong body is
// shown as "more than" its listed size, never as an exact size; choosing a
// record asks the ledger for that record's events only.

import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';

const REC = '7d6f1c2a-3b4e-4f5a-8b9c-0d1e2f3a4b5c';
const PEER = '11111111-2222-4333-8444-555555555555';

const record = {
  id: REC,
  peer_id: PEER,
  peer_url: 'https://studio-a.example',
  peer_name: 'Studio A',
  activity_uri: 'https://studio-a.example/activities/1',
  origin_asset_uri: 'https://studio-a.example/assets/1',
  version_uri: 'https://studio-a.example/assets/1/versions/1',
  version_number: 1,
  sensitivity: 'public',
  state: 'rejected',
  reason: 'fixity_mismatch',
  attempts: 1,
  received_at: '2026-11-20T10:00:00Z',
  file_count: 1,
  original_count: 1,
  original_bytes: 2048,
};

const base = {
  peer_id: PEER,
  peer_instance_url: 'https://studio-a.example',
  peer_display_name: 'Studio A',
  received_asset_id: REC,
  activity_uri: record.activity_uri,
  origin_asset_uri: record.origin_asset_uri,
  version_uri: record.version_uri,
  version_number: 1,
  sensitivity: 'public',
  attempt: 1,
  agent_name: 'artist-alley assetingest',
  observed_truncated: false,
  record_exists: true,
  peer_exists: true,
};

const mismatch = {
  ...base,
  id: 'e1',
  occurred_at: '2026-11-20T10:00:05Z',
  event_type: 'fixity-check',
  outcome: 'failure',
  reason: 'fixity_mismatch',
  record_state_after: 'rejected',
  file_role: 'original',
  file_position: 0,
  object_hash: 'ab'.repeat(32),
  declared_media_type: 'image/png',
  expected_sha256: 'ab'.repeat(32),
  expected_size_bytes: 2048,
  observed_basis: 'received_bytes',
  observed_sha256: 'cd'.repeat(32),
  observed_size_bytes: 2048,
  record_exists: false,
  peer_exists: false,
};

const overlong = {
  ...mismatch,
  id: 'e2',
  observed_sha256: undefined,
  observed_size_bytes: 2049,
  observed_truncated: true,
};

const state = vi.hoisted(() => ({ queries: [] as Record<string, unknown>[], events: [] as unknown[] }));

vi.mock('$api/client', () => ({
  api: {
    GET: async (path: string, opts?: { params?: { query?: Record<string, unknown> } }) => {
      if (path === '/admin/federation/peers') return { data: { items: [] } };
      if (path === '/admin/federation/received-assets') return { data: { items: [record] } };
      if (path === '/admin/federation/transfer-events') {
        state.queries.push(opts?.params?.query ?? {});
        return { data: { items: state.events } };
      }
      return { error: { error: 'unexpected ' + path } };
    },
  },
}));

import Page from './+page.svelte';

beforeEach(() => {
  state.queries = [];
  state.events = [mismatch];
});

describe('/admin/federation/received (rendered)', () => {
  it('lists the record and shows a mismatch as listed against arrived, kept after removal', async () => {
    const { container } = render(Page);
    await waitFor(() => expect(container.querySelector('[data-testid="received-admin-event"]')).not.toBeNull());
    const rec = container.querySelector<HTMLElement>('[data-testid="received-admin-record"]');
    expect(rec?.dataset.state).toBe('rejected');
    expect(rec?.textContent).toContain('fixity_mismatch');
    const ev = container.querySelector<HTMLElement>('[data-testid="received-admin-event"]');
    expect(ev?.dataset.outcome).toBe('failure');
    expect(ev?.querySelector('[data-testid="received-admin-expected"]')?.textContent).toContain('ab'.repeat(6));
    const observed = ev?.querySelector('[data-testid="received-admin-observed"]')?.textContent ?? '';
    expect(observed).toContain('cd'.repeat(6));
    expect(observed).toContain('received_bytes');
    expect(ev?.querySelector('[data-testid="received-admin-record-gone"]')).not.toBeNull();
    expect(ev?.querySelector('[data-testid="received-admin-peer-gone"]')).not.toBeNull();
  });

  it('shows an overlong body as more than its listed size', async () => {
    state.events = [overlong];
    const { container } = render(Page);
    await waitFor(() => expect(container.querySelector('[data-testid="received-admin-event"]')).not.toBeNull());
    const observed = container.querySelector('[data-testid="received-admin-observed"]')?.textContent ?? '';
    expect(observed).toContain('more than 2048 B');
    expect(observed).not.toContain('2049');
  });

  it("asks the ledger for one record's events when it is chosen", async () => {
    render(Page);
    await waitFor(() => expect(screen.getByTestId('received-admin-show-events')).toBeTruthy());
    await fireEvent.click(screen.getByTestId('received-admin-show-events'));
    await waitFor(() => expect(state.queries.at(-1)?.received_asset_id).toBe(REC));
    expect(screen.getByTestId('received-admin-event-scope').textContent).toContain(REC.slice(0, 8));
  });
});
