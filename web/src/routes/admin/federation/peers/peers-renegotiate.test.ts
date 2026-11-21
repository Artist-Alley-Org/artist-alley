// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1574, spec v1 §16.2 /admin/federation/peers: RENDERED-DOM assertions
// for "Negotiate again" against a mocked API. What must hold: only a
// connected peer offers it, and a disabled one offers it disabled; a
// click POSTs that peer's renegotiate route and shows a busy label; the
// result says what changed (added and removed capabilities) or that
// nothing did; a 502 says the other instance did not complete it, and
// warns when this instance already recorded the new set; a 409 shows the
// server's reason.

import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';

const peer = (id: string, status: string, enabled: boolean) => ({
  id,
  instance_url: `https://${id}.example`,
  display_name: `Peer ${id}`,
  instance_public_key: 'k',
  status,
  trust_tier: 'connected',
  encryption_policy: 'plaintext',
  enabled,
  handshake_at: '2026-11-20T10:00:00Z',
  handshake_by_user_ref: 1,
  notes: '',
  share_in_visible_list: false,
});

const state = vi.hoisted(() => ({
  posts: [] as string[],
  answer: undefined as undefined | (() => Promise<{ data?: unknown; error?: unknown }>),
}));

vi.mock('$api/client', () => ({
  api: {
    GET: async (path: string) => {
      if (path === '/admin/federation/peers') {
        return { data: { items: [peer('alpha', 'connected', true), peer('beta', 'connected', false), peer('gamma', 'pending_outbound', true)] } };
      }
      if (path === '/admin/federation/peers/pending-inbound') return { data: { items: [] } };
      if (path === '/admin/federation/suggestions') return { data: { items: [] } };
      if (path === '/federation/instance') return { error: { error: 'none' } };
      return { error: { error: 'unexpected ' + path } };
    },
    POST: async (path: string, opts?: { params?: { path?: { id?: string } } }) => {
      state.posts.push(path.replace('{id}', opts?.params?.path?.id ?? ''));
      return state.answer ? state.answer() : { error: { error: 'no answer' } };
    },
    PATCH: async () => ({ data: {} }),
    DELETE: async () => ({ data: {} }),
  },
}));

import Page from './+page.svelte';

const outcome = (previous: string[], capabilities: string[], error?: string) => ({
  peer: peer('alpha', 'connected', true),
  previous_capabilities: previous,
  capabilities,
  ...(error ? { error } : {}),
});

beforeEach(() => {
  state.posts = [];
  state.answer = undefined;
});

async function buttons(): Promise<HTMLButtonElement[]> {
  await waitFor(() => expect(screen.getAllByTestId('renegotiate').length).toBeGreaterThan(0));
  return screen.getAllByTestId('renegotiate') as HTMLButtonElement[];
}

describe('/admin/federation/peers "Negotiate again" (rendered)', () => {
  it('is offered for connected peers only, and disabled for a disabled one', async () => {
    render(Page);
    const b = await buttons();
    expect(b).toHaveLength(2);
    expect(b[0].disabled).toBe(false);
    expect(b[1].disabled).toBe(true);
  });

  it('POSTs the peer and reports the added and removed capabilities', async () => {
    let release!: () => void;
    state.answer = () =>
      new Promise((resolve) => {
        release = () => resolve({ data: outcome(['e2e-encrypted', 'x25519'], ['e2e-encrypted', 'share-lifecycle-1']) });
      });
    render(Page);
    const [alpha] = await buttons();
    const label = alpha.textContent;
    void fireEvent.click(alpha);
    await waitFor(() => expect(state.posts).toEqual(['/admin/federation/peers/alpha/renegotiate']));
    await waitFor(() => expect(screen.getAllByTestId('renegotiate')[0].textContent).not.toBe(label));
    release();
    const result = await screen.findByTestId('renegotiate-result');
    expect(result.getAttribute('role')).toBe('status');
    expect(result.textContent).toContain('Capabilities updated');
    expect(result.textContent).toContain('Added: share-lifecycle-1');
    expect(result.textContent).toContain('Removed: x25519');
  });

  it('says when nothing changed', async () => {
    state.answer = async () => ({ data: outcome(['e2e-encrypted'], ['e2e-encrypted']) });
    render(Page);
    await fireEvent.click((await buttons())[0]);
    const result = await screen.findByTestId('renegotiate-result');
    expect(result.textContent).toContain('No change');
    expect(result.textContent).not.toContain('Added');
  });

  it('reports a 502, and warns when this instance already recorded the new set', async () => {
    state.answer = async () => ({ error: outcome(['e2e-encrypted'], ['e2e-encrypted', 'share-lifecycle-1'], 'answer lost') });
    render(Page);
    await fireEvent.click((await buttons())[0]);
    const result = await screen.findByTestId('renegotiate-result');
    expect(result.getAttribute('role')).toBe('alert');
    expect(result.textContent).toContain('did not complete the negotiation');
    expect(result.textContent).toContain('Negotiate again so both match');
    expect(result.textContent).toContain('answer lost');
  });

  it('shows the reason of a 409 without a partial warning', async () => {
    state.answer = async () => ({ error: { error: 'handshake: the peer is disabled; enable it first' } });
    render(Page);
    await fireEvent.click((await buttons())[0]);
    const result = await screen.findByTestId('renegotiate-result');
    expect(result.textContent).toContain('enable it first');
    expect(result.textContent).not.toContain('Negotiate again so both match');
  });
});
