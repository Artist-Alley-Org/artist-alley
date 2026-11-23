// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1576 S4 /admin/fields/schema-exchange: RENDERED-DOM assertions
// against a mocked network. What must hold:
//   - nothing renders or loads without a resolved fields.admin;
//   - export sends a deliberate selection and saves the response bytes;
//   - preview sends the document text byte for byte, never re-encoded;
//   - no decision is pre-selected, and Apply appears only when every
//     code has one;
//   - an edit, a new preview, or a late answer invalidates everything
//     built on the old preview;
//   - an ambiguous apply keeps the SAME request for the retry;
//   - stale, expired, refused and rate-limited answers are told apart;
//   - text from the document or the source note renders as text.

import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { auth } from '$stores/auth.svelte';
import { buildApplyBody, buildPreviewBody, checkSource, allDecided } from '$lib/admin/schemaExchange';

const api = vi.hoisted(() => ({ calls: [] as string[] }));
vi.mock('$api/client', () => ({
  api: {
    GET: async (path: string) => {
      api.calls.push(path);
      if (path === '/fields')
        return {
          data: [
            { code: 'zeta', label: 'Zeta', type: 'text', mirrors_column: null },
            { code: 'alpha', label: 'Alpha', type: 'select', mirrors_column: null },
            { code: 'title_m', label: 'Title', type: 'text', mirrors_column: 'title' },
          ],
        };
      return { error: { error: 'unexpected ' + path } };
    },
  },
}));

import Page from './+page.svelte';

type Call = { url: string; body: string };
let calls: Call[] = [];
let answers: Array<(c: Call) => Promise<Response>> = [];

function json(status: number, body: unknown): Promise<Response> {
  return Promise.resolve(new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }));
}

const DIGEST = 'sha256:' + 'a'.repeat(64);
function previewOf(extra: Record<string, unknown> = {}) {
  return {
    preview_id: '6f6c9a7e-5d1f-4c55-9a43-6f0a7d1c9b10',
    created_at: '2026-11-22T07:00:00Z',
    expires_at: new Date(Date.now() + 15 * 60_000).toISOString(),
    document_digest: DIGEST,
    source: { label: '<img src=x onerror="window.pwned=1">' },
    advisory: 'Apply re-validates the final configuration.',
    unresolved_terms: [{ field: 'alpha', term: 'ghost=1' }],
    fields: [
      {
        code: 'alpha',
        local: 'different',
        local_id: '11111111-2222-4333-8444-555555555555',
        local_status: 'active',
        problem: null,
        incoming: { code: 'alpha', label: '<b>Alpha</b>', type: 'select' },
        differences: [
          { property: 'options', local: [{ value: 'a' }], incoming: [{ value: 'b' }], terms_only_incoming: ['b'], terms_only_local: ['a'], terms_changed: [] },
        ],
        decisions: ['use_existing', 'keep_local', 'skip'],
      },
      {
        code: 'beta',
        local: 'absent',
        local_id: null,
        local_status: null,
        problem: null,
        incoming: { code: 'beta', label: 'Beta', type: 'text' },
        differences: [],
        decisions: ['create', 'skip'],
      },
    ],
    ...extra,
  };
}

const RECEIPT = {
  receipt_id: '99999999-8888-4777-8666-555555555555',
  preview_id: '6f6c9a7e-5d1f-4c55-9a43-6f0a7d1c9b10',
  applied_at: '2026-11-22T07:01:00Z',
  document_digest: DIGEST,
  decisions_digest: 'sha256:' + 'b'.repeat(64),
  replayed: false,
  changed: 1,
  fields: [
    { code: 'alpha', decision: 'keep_local', outcome: 'kept_local', field_id: '11111111-2222-4333-8444-555555555555' },
    { code: 'beta', decision: 'create', outcome: 'created', field_id: '22222222-3333-4444-8555-666666666666' },
  ],
};

// Unusual but valid spelling: member order, spacing, an escape and a
// number form that a parse and re-serialize would all change.
const RAW = '{ "fields" : [ ],\n  "format":"aa-field-schema/1", "x": 1.0, "e": "\\u00e9" }';

beforeEach(() => {
  api.calls = [];
  calls = [];
  answers = [];
  auth.ready = true;
  auth.capsStatus = 'resolved';
  auth.caps = ['fields.admin'];
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      const c = { url: String(url), body: String(init?.body ?? '') };
      calls.push(c);
      const next = answers.shift();
      if (!next) throw new Error('unexpected fetch ' + url);
      return next(c);
    }),
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  auth.caps = [];
  auth.capsStatus = 'resolved';
  auth.ready = false;
});

async function pasteAndPreview(text = RAW, preview: unknown = previewOf()) {
  await fireEvent.input(screen.getByTestId('schema-import-text'), { target: { value: text } });
  answers.push(() => json(200, preview));
  await fireEvent.click(screen.getByTestId('schema-import-preview-button'));
  await waitFor(() => expect(screen.queryByTestId('schema-import-preview')).not.toBeNull());
}

function choose(code: string, choice: string) {
  const el = document.querySelector<HTMLInputElement>(`[data-code="${code}"] [data-choice="${choice}"]`);
  if (!el) throw new Error(`no ${choice} for ${code}`);
  return fireEvent.click(el);
}

describe('request bodies', () => {
  it('embeds the document text unchanged and adds only typed source members', () => {
    expect(buildPreviewBody(RAW, null)).toBe('{"document":' + RAW + '}');
    expect(buildPreviewBody(RAW, checkSource('Partner', ''))).toBe('{"document":' + RAW + ',"source":{"label":"Partner"}}');
    expect(checkSource('', '')).toBeNull();
    // Typed, not trimmed: the server judges a blank-looking note.
    expect(checkSource('  ', '')).toEqual({ label: '  ' });
    expect(checkSource('', 'HTTPS://user@example.invalid/x')).toEqual({ uri: 'HTTPS://user@example.invalid/x' });
    for (const bad of ['example.invalid/x', 'http:///x', 'mailto:a@b', 'ftp://h/x', 'https://' + 'a'.repeat(2048)]) {
      expect(() => checkSource('', bad), bad).toThrow();
    }
    expect(() => checkSource('a\u0000b', '')).toThrow();
    // A raw unpaired surrogate is refused before encoding; the JSON escape
    // spelling of one is ASCII text and goes to the server unchanged.
    expect(() => buildPreviewBody('"' + String.fromCharCode(0xd800) + '"', null)).toThrow();
    expect(buildPreviewBody('"\\uD800"', null)).toBe('{"document":"\\uD800"}');
    expect(() => buildPreviewBody('{"fields":[]},"source":{"label":"x"}', null)).toThrow();
    expect(() => buildPreviewBody('', null)).toThrow();
  });

  it('requires an allowed choice for every code and orders decisions as the document', () => {
    const p = previewOf() as never;
    expect(allDecided((p as { fields: never[] }).fields, { alpha: 'keep_local' })).toBe(false);
    expect(allDecided((p as { fields: never[] }).fields, { alpha: 'create', beta: 'skip' })).toBe(false);
    expect(allDecided((p as { fields: never[] }).fields, { alpha: 'keep_local', beta: 'skip' })).toBe(true);
    expect(buildApplyBody(p, { beta: 'skip', alpha: 'keep_local' })).toBe(
      `{"preview_id":"6f6c9a7e-5d1f-4c55-9a43-6f0a7d1c9b10","document_digest":"${DIGEST}","decisions":{"alpha":"keep_local","beta":"skip"}}`,
    );
  });
});

describe('/admin/fields/schema-exchange access', () => {
  it('waits for auth, then refuses unknown rights and a missing capability, loading nothing', async () => {
    auth.ready = false;
    const { unmount } = render(Page);
    expect(screen.getByTestId('schema-exchange-loading')).toBeTruthy();
    unmount();
    auth.ready = true;
    auth.capsStatus = 'unavailable';
    const r2 = render(Page);
    expect(screen.getByTestId('schema-exchange-caps-unavailable')).toBeTruthy();
    expect(screen.queryByTestId('schema-import')).toBeNull();
    r2.unmount();
    auth.capsStatus = 'resolved';
    auth.caps = ['system.audit.read'];
    render(Page);
    expect(screen.getByTestId('schema-exchange-denied')).toBeTruthy();
    expect(screen.queryByTestId('schema-export')).toBeNull();
    await new Promise((r) => setTimeout(r, 10));
    expect(api.calls).toEqual([]);
    expect(calls).toEqual([]);
  });

  it('opens for system.admin', async () => {
    auth.caps = ['system.admin'];
    render(Page);
    await waitFor(() => expect(screen.getAllByTestId('schema-export-pick').length).toBe(2));
  });
});

describe('/admin/fields/schema-exchange export', () => {
  it('lists only non-mirrored fields, needs a selection, and saves the response bytes', async () => {
    const created: Blob[] = [];
    const revoked: string[] = [];
    let clicked = '';
    vi.spyOn(URL, 'createObjectURL').mockImplementation((b) => {
      created.push(b as Blob);
      return 'blob:export-1';
    });
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation((u) => void revoked.push(u));
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      clicked = this.download + ' ' + this.href;
    });
    render(Page);
    await waitFor(() => expect(screen.getAllByTestId('schema-export-pick').length).toBe(2));
    const picks = screen.getAllByTestId('schema-export-pick') as HTMLInputElement[];
    expect(picks.map((p) => p.value)).toEqual(['zeta', 'alpha']);
    expect(picks.every((p) => !p.checked)).toBe(true);
    const button = screen.getByTestId('schema-export-button') as HTMLButtonElement;
    expect(button.disabled).toBe(true);

    const bytes = '{"fields":[ ],"format":"aa-field-schema/1"}\n';
    answers.push(() => Promise.resolve(new Response(bytes, { status: 200 })));
    await fireEvent.click(picks[0]);
    await fireEvent.click(picks[1]);
    expect(button.disabled).toBe(false);
    await fireEvent.click(button);
    await waitFor(() => expect(screen.queryByTestId('schema-export-done')).not.toBeNull());
    expect(calls[0].url).toBe('/api/v1/fields/export');
    expect(JSON.parse(calls[0].body)).toEqual({ codes: ['alpha', 'zeta'] });
    expect(await created[0].text()).toBe(bytes);
    expect(clicked).toMatch(/^field-schema-\d{4}-\d{2}-\d{2}\.json blob:export-1$/);
    await waitFor(() => expect(revoked).toEqual(['blob:export-1']));
    expect(click).toHaveBeenCalledTimes(1);
  });

  it('shows a refusal and saves nothing', async () => {
    const create = vi.spyOn(URL, 'createObjectURL');
    render(Page);
    await waitFor(() => expect(screen.getAllByTestId('schema-export-pick').length).toBe(2));
    await fireEvent.click(screen.getAllByTestId('schema-export-pick')[0]);
    answers.push(() => json(422, { error: 'x', reason: 'retired_codes', codes: ['zeta'] }));
    await fireEvent.click(screen.getByTestId('schema-export-button'));
    await waitFor(() => expect(screen.getByTestId('schema-export-error').textContent).toContain('retired_codes: zeta'));
    expect(create).not.toHaveBeenCalled();
  });
});

describe('/admin/fields/schema-exchange import', () => {
  it('sends the pasted text byte for byte with the typed source', async () => {
    render(Page);
    await fireEvent.input(screen.getByTestId('schema-import-source-label'), { target: { value: 'Partner DAM' } });
    await fireEvent.input(screen.getByTestId('schema-import-source-uri'), { target: { value: 'https://example.invalid/s' } });
    await pasteAndPreview();
    expect(calls[0].url).toBe('/api/v1/fields/import/preview');
    expect(calls[0].body).toBe('{"document":' + RAW + ',"source":{"label":"Partner DAM","uri":"https://example.invalid/s"}}');
  });

  it('sends a chosen file exactly as read', async () => {
    render(Page);
    const fileText = '{"format":"aa-field-schema/1","fields":[],"n":1E2}';
    const input = screen.getByTestId('schema-import-file') as HTMLInputElement;
    const file = new File([fileText], 'partner.json', { type: 'application/json' });
    Object.defineProperty(input, 'files', { value: [file], configurable: true });
    await fireEvent.change(input);
    await waitFor(() => expect((screen.getByTestId('schema-import-text') as HTMLTextAreaElement).value).toBe(fileText));
    answers.push(() => json(200, previewOf()));
    await fireEvent.click(screen.getByTestId('schema-import-preview-button'));
    await waitFor(() => expect(calls.length).toBe(1));
    expect(calls[0].body).toBe('{"document":' + fileText + '}');
  });

  it('refuses text that is not one JSON value without sending it', async () => {
    render(Page);
    await fireEvent.input(screen.getByTestId('schema-import-text'), { target: { value: '{"fields":[]} {"x":1}' } });
    await fireEvent.click(screen.getByTestId('schema-import-preview-button'));
    expect(screen.getByTestId('schema-import-preview-error').textContent).toContain('not valid JSON');
    expect(calls).toEqual([]);
  });

  it('shows the preview as text, with no decision chosen and no Apply', async () => {
    const { container } = render(Page);
    await pasteAndPreview();
    expect(screen.getByTestId('schema-preview-source-label').textContent).toBe('<img src=x onerror="window.pwned=1">');
    expect(container.querySelector('[data-testid="schema-import-preview"] img')).toBeNull();
    expect(container.querySelector('[data-testid="schema-import-preview"] b')).toBeNull();
    expect(screen.getByTestId('schema-preview-digest').textContent).toBe(DIGEST);
    expect(screen.getByTestId('schema-preview-advisory').textContent).toContain('re-validates');
    expect(screen.getByTestId('schema-preview-unresolved').textContent).toContain('ghost=1');
    const diff = screen.getByTestId('schema-preview-differences').textContent ?? '';
    expect(diff).toContain('Only incoming: b');
    expect(diff).toContain('Only local: a');
    const radios = screen.getAllByTestId('schema-decision') as HTMLInputElement[];
    expect(radios.map((r) => r.dataset.choice)).toEqual(['use_existing', 'keep_local', 'skip', 'create', 'skip']);
    expect(radios.some((r) => r.checked)).toBe(false);
    expect(screen.queryByTestId('schema-apply-button')).toBeNull();
    await choose('alpha', 'keep_local');
    expect(screen.queryByTestId('schema-apply-button')).toBeNull();
    await choose('beta', 'create');
    expect(screen.getByTestId('schema-apply-button')).toBeTruthy();
  });

  it('an edit after the preview discards it, its decisions and Apply', async () => {
    render(Page);
    await pasteAndPreview();
    await choose('alpha', 'keep_local');
    await choose('beta', 'create');
    expect(screen.getByTestId('schema-apply-button')).toBeTruthy();
    await fireEvent.input(screen.getByTestId('schema-import-text'), { target: { value: RAW + ' ' } });
    expect(screen.queryByTestId('schema-import-preview')).toBeNull();
    expect(screen.queryByTestId('schema-apply-button')).toBeNull();
    // An edit to the source note counts too.
    await pasteAndPreview(RAW + ' ');
    await fireEvent.input(screen.getByTestId('schema-import-source-label'), { target: { value: 'changed' } });
    expect(screen.queryByTestId('schema-import-preview')).toBeNull();
  });

  it('ignores a preview answer that arrives after an edit', async () => {
    render(Page);
    await fireEvent.input(screen.getByTestId('schema-import-text'), { target: { value: RAW } });
    let release: (r: Response) => void = () => {};
    answers.push(() => new Promise<Response>((r) => (release = r)));
    await fireEvent.click(screen.getByTestId('schema-import-preview-button'));
    await waitFor(() => expect(calls.length).toBe(1));
    await fireEvent.input(screen.getByTestId('schema-import-text'), { target: { value: RAW + '\n' } });
    release(new Response(JSON.stringify(previewOf()), { status: 200 }));
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByTestId('schema-import-preview')).toBeNull();
  });

  it('a new preview replaces the old one and starts with no decisions', async () => {
    render(Page);
    await pasteAndPreview();
    await choose('alpha', 'keep_local');
    await choose('beta', 'create');
    answers.push(() => json(200, previewOf({ preview_id: '7f6c9a7e-5d1f-4c55-9a43-6f0a7d1c9b10' })));
    await fireEvent.click(screen.getByTestId('schema-import-preview-button'));
    await waitFor(() => expect(calls.length).toBe(2));
    await waitFor(() => expect(screen.queryByTestId('schema-import-preview')).not.toBeNull());
    expect((screen.getAllByTestId('schema-decision') as HTMLInputElement[]).some((r) => r.checked)).toBe(false);
    expect(screen.queryByTestId('schema-apply-button')).toBeNull();
  });

  it('keeps the SAME request across an ambiguous failure and shows a replayed receipt', async () => {
    render(Page);
    await pasteAndPreview();
    await choose('alpha', 'keep_local');
    await choose('beta', 'create');
    answers.push(() => Promise.reject(new TypeError('Failed to fetch')));
    await fireEvent.click(screen.getByTestId('schema-apply-button'));
    await waitFor(() => expect(screen.getByTestId('schema-apply-error').dataset.ambiguous).toBe('true'));
    expect(screen.getByTestId('schema-apply-button').textContent).toContain('Retry the same request');
    // Decisions are frozen while the request may have applied.
    const frozenRadio = document.querySelector<HTMLInputElement>('[data-code="alpha"] [data-choice="skip"]')!;
    expect(frozenRadio.closest('fieldset')?.disabled).toBe(true);
    await choose('alpha', 'skip');
    answers.push(() => json(500, { error: 'internal error' }));
    await fireEvent.click(screen.getByTestId('schema-apply-button'));
    await waitFor(() => expect(calls.length).toBe(3));
    await waitFor(() => expect(screen.getByTestId('schema-apply-error').dataset.ambiguous).toBe('true'));
    answers.push(() => json(200, { ...RECEIPT, replayed: true }));
    await fireEvent.click(screen.getByTestId('schema-apply-button'));
    await waitFor(() => expect(screen.queryByTestId('schema-apply-receipt')).not.toBeNull());
    const applies = calls.filter((c) => c.url === '/api/v1/fields/import/apply').map((c) => c.body);
    expect(applies.length).toBe(3);
    expect(new Set(applies).size).toBe(1);
    expect(JSON.parse(applies[0]).decisions).toEqual({ alpha: 'keep_local', beta: 'create' });
    expect(screen.getByTestId('schema-receipt-replayed')).toBeTruthy();
    expect(screen.queryByTestId('schema-apply-button')).toBeNull();
    expect(screen.getAllByTestId('schema-receipt-item').map((r) => r.dataset.outcome)).toEqual(['kept_local', 'created']);
  });

  it('a fresh receipt reports its outcomes and links to history', async () => {
    const { container } = render(Page);
    await pasteAndPreview();
    await choose('alpha', 'keep_local');
    await choose('beta', 'create');
    answers.push(() => json(200, RECEIPT));
    await fireEvent.click(screen.getByTestId('schema-apply-button'));
    await waitFor(() => expect(screen.queryByTestId('schema-apply-receipt')).not.toBeNull());
    expect(screen.getByTestId('schema-apply-receipt').dataset.replayed).toBe('false');
    expect(screen.queryByTestId('schema-receipt-replayed')).toBeNull();
    expect(container.querySelector(`a[href="/admin/fields/import-history?receipt=${RECEIPT.receipt_id}"]`)).not.toBeNull();
  });

  for (const [status, reason, text] of [
    [409, 'preview_stale', 'has changed since'],
    [410, 'preview_expired', 'expired'],
    [409, 'preview_consumed', 'already used'],
    [404, 'preview_not_found', 'no longer exists'],
  ] as const) {
    it(`a ${reason} refusal ends this preview`, async () => {
      render(Page);
      await pasteAndPreview();
      await choose('alpha', 'keep_local');
      await choose('beta', 'create');
      answers.push(() => json(status, { error: 'x', reason, problems: [] }));
      await fireEvent.click(screen.getByTestId('schema-apply-button'));
      await waitFor(() => expect(screen.getByTestId('schema-apply-error').textContent).toContain(text));
      expect(screen.getByTestId('schema-apply-error').textContent).toContain(reason);
      expect(screen.queryByTestId('schema-apply-button')).toBeNull();
    });
  }

  it('a 422 refusal lists its problems and lets the decisions change', async () => {
    render(Page);
    await pasteAndPreview();
    await choose('alpha', 'keep_local');
    await choose('beta', 'create');
    answers.push(() => json(422, { error: 'x', reason: 'graph_refused', problems: [{ path: '$.decisions.beta', problem: 'condition cycle' }] }));
    await fireEvent.click(screen.getByTestId('schema-apply-button'));
    await waitFor(() => expect(screen.getByTestId('schema-apply-error').textContent).toContain('condition cycle'));
    await choose('beta', 'skip');
    answers.push(() => json(200, RECEIPT));
    await fireEvent.click(screen.getByTestId('schema-apply-button'));
    await waitFor(() => expect(calls.length).toBe(3));
    expect(JSON.parse(calls[2].body).decisions).toEqual({ alpha: 'keep_local', beta: 'skip' });
  });

  it('a 429 on apply keeps the same request for a retry', async () => {
    render(Page);
    await pasteAndPreview();
    await choose('alpha', 'keep_local');
    await choose('beta', 'create');
    answers.push(() => json(429, { error: 'slow down' }));
    await fireEvent.click(screen.getByTestId('schema-apply-button'));
    // Not an unknown outcome (the server answered), but the request is kept.
    await waitFor(() => expect(screen.getByTestId('schema-apply-button').textContent).toContain('Retry the same request'));
    expect(screen.getByTestId('schema-apply-error').dataset.ambiguous).toBe('false');
    answers.push(() => json(200, RECEIPT));
    await fireEvent.click(screen.getByTestId('schema-apply-button'));
    await waitFor(() => expect(calls.length).toBe(3));
    expect(calls[2].body).toBe(calls[1].body);
  });

  it('tells a preview limit and an invalid document apart', async () => {
    render(Page);
    await fireEvent.input(screen.getByTestId('schema-import-text'), { target: { value: RAW } });
    answers.push(() => json(429, { error: 'x', reason: 'preview_limit', problems: [] }));
    await fireEvent.click(screen.getByTestId('schema-import-preview-button'));
    await waitFor(() => expect(screen.getByTestId('schema-import-preview-error').textContent).toContain('Too many previews'));
    answers.push(() => json(422, { error: 'x', reason: 'document_invalid', problems: [{ path: '$.document.fields[0].type', problem: '<script>bad</script>' }] }));
    await fireEvent.click(screen.getByTestId('schema-import-preview-button'));
    await waitFor(() => expect(screen.getByTestId('schema-import-preview-error').textContent).toContain('$.document.fields[0].type'));
    expect(screen.getByTestId('schema-import-preview-error').textContent).toContain('<script>bad</script>');
    expect(document.querySelector('[data-testid="schema-import-preview-error"] script')).toBeNull();
  });
});
