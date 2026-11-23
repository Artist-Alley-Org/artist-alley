// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1576 S4 correction round 1 (rev705): regressions written BEFORE the
// fix, against the page as rev704 left it.
//   C1.1 an apply whose outcome is unknown keeps its exact request: no
//        edit, file, source change or new preview can discard it;
//   C1.2 the page's state belongs to one signed-in operator: a change of
//        account, a loss of rights or a sign-out clears it, and a late
//        answer from before the change is ignored;
//   C2   the full incoming definition is shown for review, and input is
//        bounded and checked before anything is read, parsed or sent.

import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { auth } from '$stores/auth.svelte';

const api = vi.hoisted(() => ({ calls: [] as string[] }));
vi.mock('$api/client', () => ({
  api: {
    GET: async (path: string) => {
      api.calls.push(path);
      if (path === '/fields') return { data: [{ code: 'alpha', label: 'Alpha', type: 'select', mirrors_column: null }] };
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

async function bodyText(init?: RequestInit): Promise<string> {
  const b = init?.body;
  if (b === undefined || b === null) return '';
  if (typeof b === 'string') return b;
  return new TextDecoder().decode(b as Uint8Array);
}

const DIGEST = 'sha256:' + 'a'.repeat(64);

// A complete aa-field-schema/1 field: every one of the 17 members, with
// nested options, a null label, a condition, a pattern and a tab.
const FULL = {
  code: 'region',
  label: 'Region',
  description: 'Where the work was made',
  type: 'tree',
  subject_kind: 'asset',
  required: true,
  searchable: false,
  display_group: 'technical',
  display_order: 7,
  options: [
    { value: 'europe', label: 'Europe', children: [{ value: 'uk', label: null, children: [] }] },
    { value: 'asia', label: null, children: [] },
  ],
  regexp_filter: '^[a-z]+$',
  display_condition: ['medium=paint'],
  show_on_card: true,
  show_in_advanced_search: false,
  show_on_upload: false,
  edit_tab: 'Technical',
  open_vocabulary: true,
};

function previewOf(extra: Record<string, unknown> = {}) {
  return {
    preview_id: '6f6c9a7e-5d1f-4c55-9a43-6f0a7d1c9b10',
    created_at: '2026-11-22T07:00:00Z',
    expires_at: new Date(Date.now() + 15 * 60_000).toISOString(),
    document_digest: DIGEST,
    source: {},
    advisory: 'Apply re-validates the final configuration.',
    unresolved_terms: [],
    fields: [
      { code: 'region', local: 'absent', local_id: null, local_status: null, problem: null, incoming: FULL, differences: [], decisions: ['create', 'skip'] },
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
  replayed: true,
  changed: 1,
  fields: [{ code: 'region', decision: 'create', outcome: 'created', field_id: '22222222-3333-4444-8555-666666666666' }],
};

const RAW = JSON.stringify({ fields: [FULL], format: 'aa-field-schema/1' });
const USER_A = { ref: 1, username: 'alice', matureContentAllowed: false, matureOptedIn: false };
const USER_B = { ref: 2, username: 'bob', matureContentAllowed: false, matureOptedIn: false };

beforeEach(() => {
  api.calls = [];
  calls = [];
  answers = [];
  auth.ready = true;
  auth.capsStatus = 'resolved';
  auth.caps = ['fields.admin'];
  auth.user = { ...USER_A };
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      const c = { url: String(url), body: await bodyText(init) };
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
  auth.user = null;
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

async function unknownOutcome() {
  await pasteAndPreview();
  await choose('region', 'create');
  answers.push(() => Promise.reject(new TypeError('Failed to fetch')));
  await fireEvent.click(screen.getByTestId('schema-apply-button'));
  await waitFor(() => expect(screen.getByTestId('schema-apply-error').dataset.ambiguous).toBe('true'));
}

describe('C1.1 an unknown outcome keeps its exact request', () => {
  it('no edit, file, source change or new preview discards it; retry sends the same bytes', async () => {
    render(Page);
    await unknownOutcome();
    const original = calls[1].body;
    await fireEvent.input(screen.getByTestId('schema-import-text'), { target: { value: RAW + ' ' } });
    await fireEvent.input(screen.getByTestId('schema-import-source-label'), { target: { value: 'changed' } });
    const input = screen.getByTestId('schema-import-file') as HTMLInputElement;
    const file = new File(['{"fields":[],"format":"aa-field-schema/1"}'], 'other.json');
    Object.defineProperty(input, 'files', { value: [file], configurable: true });
    await fireEvent.change(input);
    await fireEvent.click(screen.getByTestId('schema-import-preview-button'));
    await new Promise((r) => setTimeout(r, 20));
    expect(calls.length).toBe(2);
    expect(screen.queryByTestId('schema-import-preview')).not.toBeNull();
    expect(screen.getByTestId('schema-apply-error').dataset.ambiguous).toBe('true');
    expect(document.querySelector('[data-testid="schema-apply-history-link"]')).not.toBeNull();
    answers.push(() => json(200, RECEIPT));
    await fireEvent.click(screen.getByTestId('schema-apply-button'));
    await waitFor(() => expect(screen.queryByTestId('schema-apply-receipt')).not.toBeNull());
    expect(calls[2].body).toBe(original);
    expect(screen.getByTestId('schema-apply-receipt').dataset.replayed).toBe('true');
  });

  it('a double click sends one apply', async () => {
    render(Page);
    await pasteAndPreview();
    await choose('region', 'create');
    let release: (r: Response) => void = () => {};
    answers.push(() => new Promise<Response>((r) => (release = r)));
    const button = screen.getByTestId('schema-apply-button');
    await fireEvent.click(button);
    await fireEvent.click(button);
    release(new Response(JSON.stringify({ ...RECEIPT, replayed: false }), { status: 200 }));
    await waitFor(() => expect(screen.queryByTestId('schema-apply-receipt')).not.toBeNull());
    expect(calls.filter((c) => c.url.endsWith('/apply')).length).toBe(1);
  });
});

describe('C1.2 page state belongs to one operator', () => {
  it('a change of account clears the preview and ignores the old answer', async () => {
    render(Page);
    await pasteAndPreview();
    auth.user = { ...USER_B };
    await waitFor(() => expect(screen.queryByTestId('schema-import-preview')).toBeNull());
    expect((screen.getByTestId('schema-import-text') as HTMLTextAreaElement).value).toBe('');
  });

  it('a late preview answer from the previous account is ignored', async () => {
    render(Page);
    await fireEvent.input(screen.getByTestId('schema-import-text'), { target: { value: RAW } });
    let release: (r: Response) => void = () => {};
    answers.push(() => new Promise<Response>((r) => (release = r)));
    await fireEvent.click(screen.getByTestId('schema-import-preview-button'));
    await waitFor(() => expect(calls.length).toBe(1));
    auth.user = { ...USER_B };
    await new Promise((r) => setTimeout(r, 10));
    release(new Response(JSON.stringify(previewOf()), { status: 200 }));
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByTestId('schema-import-preview')).toBeNull();
  });

  it('losing the capability and regaining it shows nothing from before', async () => {
    render(Page);
    await pasteAndPreview();
    auth.caps = [];
    await waitFor(() => expect(screen.queryByTestId('schema-exchange-denied')).not.toBeNull());
    auth.caps = ['fields.admin'];
    await waitFor(() => expect(screen.queryByTestId('schema-import')).not.toBeNull());
    expect(screen.queryByTestId('schema-import-preview')).toBeNull();
  });

  it('an unknown outcome is not replayed under another account', async () => {
    render(Page);
    await unknownOutcome();
    auth.user = { ...USER_B };
    await waitFor(() => expect(screen.queryByTestId('schema-apply-button')).toBeNull());
    expect(screen.getByTestId('schema-lost-unknown').textContent).toContain('alice');
    expect(calls.length).toBe(2);
  });
});

describe('C2 review and bounds', () => {
  it('an absent field shows every incoming member for review', async () => {
    render(Page);
    await pasteAndPreview();
    const detail = screen.getByTestId('schema-preview-incoming');
    for (const k of Object.keys(FULL)) expect(detail.textContent, k).toContain(k);
    expect(detail.textContent).toContain('^[a-z]+$');
    expect(detail.textContent).toContain('medium=paint');
    expect(detail.textContent).toContain('uk');
    expect(screen.getByTestId('schema-decision-label-create').textContent?.length).toBeGreaterThan(10);
  });

  it('an oversize file is refused without reading it', async () => {
    render(Page);
    const input = screen.getByTestId('schema-import-file') as HTMLInputElement;
    const file = new File(['x'], 'huge.json');
    Object.defineProperty(file, 'size', { value: 3 * 1024 * 1024 });
    const text = vi.fn(async () => 'x');
    const buf = vi.fn(async () => new ArrayBuffer(1));
    Object.assign(file, { text, arrayBuffer: buf });
    Object.defineProperty(input, 'files', { value: [file], configurable: true });
    await fireEvent.change(input);
    await waitFor(() => expect(screen.getByTestId('schema-import-file-error')).toBeTruthy());
    expect(text).not.toHaveBeenCalled();
    expect(buf).not.toHaveBeenCalled();
  });

  it('a file that is not valid UTF-8 is refused, not repaired, and the old text is gone', async () => {
    render(Page);
    await fireEvent.input(screen.getByTestId('schema-import-text'), { target: { value: RAW } });
    const input = screen.getByTestId('schema-import-file') as HTMLInputElement;
    const file = new File([new Uint8Array([0x7b, 0x22, 0xff, 0x22, 0x3a, 0x31, 0x7d])], 'bad.json');
    Object.defineProperty(input, 'files', { value: [file], configurable: true });
    await fireEvent.change(input);
    await waitFor(() => expect(screen.getByTestId('schema-import-file-error')).toBeTruthy());
    await fireEvent.click(screen.getByTestId('schema-import-preview-button'));
    await new Promise((r) => setTimeout(r, 10));
    expect(calls).toEqual([]);
  });

  it('an oversize paste sends nothing', async () => {
    render(Page);
    const big = '{"fields":[],"format":"aa-field-schema/1","pad":"' + 'x'.repeat(2_162_688) + '"}';
    await fireEvent.input(screen.getByTestId('schema-import-text'), { target: { value: big } });
    await fireEvent.click(screen.getByTestId('schema-import-preview-button'));
    await new Promise((r) => setTimeout(r, 10));
    expect(calls).toEqual([]);
    expect(screen.getByTestId('schema-import-preview-error').getAttribute('role')).toBe('alert');
  });

  it('a 200-character emoji label is sent and a 201-character one is refused', async () => {
    render(Page);
    const label = '\u{1F600}'.repeat(200);
    await fireEvent.input(screen.getByTestId('schema-import-source-label'), { target: { value: label } });
    await pasteAndPreview();
    expect(calls[0].body).toContain(label);
    await fireEvent.input(screen.getByTestId('schema-import-source-label'), { target: { value: label + 'x' } });
    await fireEvent.click(screen.getByTestId('schema-import-preview-button'));
    await new Promise((r) => setTimeout(r, 10));
    expect(calls.length).toBe(1);
  });

  it('a non-http source URI is refused before sending', async () => {
    render(Page);
    await fireEvent.input(screen.getByTestId('schema-import-source-uri'), { target: { value: 'ftp://example.invalid/s' } });
    await fireEvent.input(screen.getByTestId('schema-import-text'), { target: { value: RAW } });
    await fireEvent.click(screen.getByTestId('schema-import-preview-button'));
    await new Promise((r) => setTimeout(r, 10));
    expect(calls).toEqual([]);
  });

  it('a document with a duplicate key is forwarded unmodified for the server to refuse', async () => {
    render(Page);
    const dup = '{"format":"aa-field-schema/1","fields":[],"fields":[]}';
    await fireEvent.input(screen.getByTestId('schema-import-text'), { target: { value: dup } });
    answers.push(() => json(422, { error: 'x', reason: 'document_invalid', problems: [{ path: '$.document.fields', problem: 'duplicate member' }] }));
    await fireEvent.click(screen.getByTestId('schema-import-preview-button'));
    await waitFor(() => expect(screen.getByTestId('schema-import-preview-error').textContent).toContain('duplicate member'));
    expect(calls[0].body).toBe('{"document":' + dup + '}');
  });
});

describe('C1/C3 further transitions and honest results', () => {
  it('a zero-change receipt says nothing was adopted, and a new review clears it', async () => {
    render(Page);
    await pasteAndPreview();
    await choose('region', 'skip');
    answers.push(() =>
      json(200, { ...RECEIPT, replayed: false, changed: 0, fields: [{ code: 'region', decision: 'skip', outcome: 'skipped', field_id: null }] }),
    );
    await fireEvent.click(screen.getByTestId('schema-apply-button'));
    await waitFor(() => expect(screen.queryByTestId('schema-apply-receipt')).not.toBeNull());
    expect(screen.getByTestId('schema-receipt-changed').textContent).toBe('0');
    expect(screen.getByTestId('schema-receipt-item').textContent).toContain('not adopted');
    expect(screen.getByTestId('schema-receipt-id').textContent).toBe(RECEIPT.receipt_id);
    await fireEvent.input(screen.getByTestId('schema-import-text'), { target: { value: RAW + ' ' } });
    expect(screen.queryByTestId('schema-apply-receipt')).toBeNull();
  });

  it('discarding an unknown outcome needs the acknowledgment', async () => {
    render(Page);
    await unknownOutcome();
    const discard = screen.getByTestId('schema-abandon') as HTMLButtonElement;
    expect(discard.disabled).toBe(true);
    await fireEvent.click(discard);
    expect(screen.getByTestId('schema-apply-error').dataset.ambiguous).toBe('true');
    await fireEvent.click(screen.getByTestId('schema-abandon-ack'));
    await fireEvent.click(screen.getByTestId('schema-abandon'));
    expect(screen.queryByTestId('schema-import-preview')).toBeNull();
    expect((screen.getByTestId('schema-import-text') as HTMLTextAreaElement).disabled).toBe(false);
    expect(calls.length).toBe(2);
  });

  it('degraded rights and a sign-out clear the page; restored rights start fresh', async () => {
    render(Page);
    await pasteAndPreview();
    auth.capsStatus = 'unavailable';
    await waitFor(() => expect(screen.getByTestId('schema-exchange-caps-unavailable')).toBeTruthy());
    auth.capsStatus = 'resolved';
    await waitFor(() => expect(screen.queryByTestId('schema-import')).not.toBeNull());
    expect(screen.queryByTestId('schema-import-preview')).toBeNull();
    await pasteAndPreview();
    auth.user = null;
    auth.caps = [];
    await waitFor(() => expect(screen.getByTestId('schema-exchange-denied')).toBeTruthy());
    auth.user = { ...USER_A };
    auth.caps = ['fields.admin'];
    await waitFor(() => expect(screen.queryByTestId('schema-import')).not.toBeNull());
    expect(screen.queryByTestId('schema-import-preview')).toBeNull();
    expect((screen.getByTestId('schema-import-text') as HTMLTextAreaElement).value).toBe('');
  });

  it('an export answer that arrives after a change of account is not saved', async () => {
    const create = vi.spyOn(URL, 'createObjectURL');
    render(Page);
    await waitFor(() => expect(screen.getAllByTestId('schema-export-pick').length).toBe(1));
    await fireEvent.click(screen.getAllByTestId('schema-export-pick')[0]);
    let release: (r: Response) => void = () => {};
    answers.push(() => new Promise<Response>((r) => (release = r)));
    await fireEvent.click(screen.getByTestId('schema-export-button'));
    await waitFor(() => expect(calls.length).toBe(1));
    auth.user = { ...USER_B };
    await new Promise((r) => setTimeout(r, 10));
    release(new Response('{"fields":[],"format":"aa-field-schema/1"}', { status: 200 }));
    await new Promise((r) => setTimeout(r, 20));
    expect(create).not.toHaveBeenCalled();
    expect(screen.queryByTestId('schema-export-done')).toBeNull();
  });

  it('a file that cannot be read shows an error and leaves no document', async () => {
    render(Page);
    await fireEvent.input(screen.getByTestId('schema-import-text'), { target: { value: RAW } });
    const input = screen.getByTestId('schema-import-file') as HTMLInputElement;
    const file = new File(['{}'], 'gone.json');
    Object.assign(file, { arrayBuffer: () => Promise.reject(new Error('NotReadableError')) });
    Object.defineProperty(input, 'files', { value: [file], configurable: true });
    await fireEvent.change(input);
    await waitFor(() => expect(screen.getByTestId('schema-import-file-error').textContent).toContain('could not be read'));
    expect((screen.getByTestId('schema-import-text') as HTMLTextAreaElement).value).toBe('');
    expect((screen.getByTestId('schema-import-preview-button') as HTMLButtonElement).disabled).toBe(true);
  });

  it('a failed field list can be retried', async () => {
    api.calls = [];
    const original = (await import('$api/client')).api.GET;
    let failOnce = true;
    const spy = vi.spyOn((await import('$api/client')).api, 'GET').mockImplementation(async (path: string, ...rest: unknown[]) => {
      if (failOnce) {
        failOnce = false;
        throw new TypeError('Failed to fetch');
      }
      return (original as (p: string, ...r: unknown[]) => Promise<unknown>)(path, ...rest) as never;
    });
    render(Page);
    await waitFor(() => expect(screen.getByTestId('schema-export-fields-error').getAttribute('role')).toBe('alert'));
    await fireEvent.click(screen.getByTestId('schema-export-fields-retry'));
    await waitFor(() => expect(screen.getAllByTestId('schema-export-pick').length).toBe(1));
    spy.mockRestore();
  });

  it('renders in Spanish', async () => {
    const { lang } = await import('$stores/lang.svelte');
    auth.user = null;
    await lang.set('es');
    try {
      render(Page);
      expect(screen.getByTestId('schema-import-preview-button').textContent).toContain('Vista previa');
    } finally {
      await lang.set('en');
    }
  });
});

// rev706: two asynchronous edges, written before the fix.
describe('rev706 C2.1 a pending file read survives a source-note edit', () => {
  function deferredFile(name: string) {
    let resolve: (b: ArrayBuffer) => void = () => {};
    let reject: (e: Error) => void = () => {};
    const file = new File(['placeholder'], name);
    Object.assign(file, { arrayBuffer: () => new Promise<ArrayBuffer>((res, rej) => ((resolve = res), (reject = rej))) });
    return { file, resolve: (s: string) => resolve(new TextEncoder().encode(s).buffer as ArrayBuffer), reject: () => reject(new Error('NotReadableError')) };
  }

  async function startRead(name = 'slow.json') {
    const d = deferredFile(name);
    const input = screen.getByTestId('schema-import-file') as HTMLInputElement;
    Object.defineProperty(input, 'files', { value: [d.file], configurable: true });
    await fireEvent.change(input);
    await waitFor(() => expect(screen.getByTestId('schema-import-file-loading')).toBeTruthy());
    return d;
  }

  it('completes with the file text and keeps the typed source note', async () => {
    render(Page);
    const d = await startRead();
    await fireEvent.input(screen.getByTestId('schema-import-source-label'), { target: { value: 'Partner' } });
    await fireEvent.input(screen.getByTestId('schema-import-source-uri'), { target: { value: 'https://example.invalid/s' } });
    d.resolve(RAW);
    await waitFor(() => expect(screen.queryByTestId('schema-import-file-loading')).toBeNull());
    expect((screen.getByTestId('schema-import-text') as HTMLTextAreaElement).value).toBe(RAW);
    expect((screen.getByTestId('schema-import-text') as HTMLTextAreaElement).disabled).toBe(false);
    expect((screen.getByTestId('schema-import-source-label') as HTMLInputElement).value).toBe('Partner');
    const preview = screen.getByTestId('schema-import-preview-button') as HTMLButtonElement;
    expect(preview.disabled).toBe(false);
    answers.push(() => json(200, previewOf()));
    await fireEvent.click(preview);
    await waitFor(() => expect(calls.length).toBe(1));
    expect(calls[0].body).toBe('{"document":' + RAW + ',"source":{"label":"Partner","uri":"https://example.invalid/s"}}');
  });

  it('a failed read after a source edit shows the error and leaves no document', async () => {
    render(Page);
    await fireEvent.input(screen.getByTestId('schema-import-text'), { target: { value: RAW } });
    const d = await startRead();
    await fireEvent.input(screen.getByTestId('schema-import-source-label'), { target: { value: 'Partner' } });
    d.reject();
    await waitFor(() => expect(screen.getByTestId('schema-import-file-error').textContent).toContain('could not be read'));
    expect(screen.queryByTestId('schema-import-file-loading')).toBeNull();
    expect((screen.getByTestId('schema-import-text') as HTMLTextAreaElement).value).toBe('');
    expect((screen.getByTestId('schema-import-text') as HTMLTextAreaElement).disabled).toBe(false);
    expect((screen.getByTestId('schema-import-preview-button') as HTMLButtonElement).disabled).toBe(true);
  });

  it('a read that completes after a change of account does not touch the new page', async () => {
    render(Page);
    const d = await startRead();
    auth.user = { ...USER_B };
    await waitFor(() => expect(screen.queryByTestId('schema-import-file-loading')).toBeNull());
    d.resolve(RAW);
    await new Promise((r) => setTimeout(r, 20));
    expect((screen.getByTestId('schema-import-text') as HTMLTextAreaElement).value).toBe('');
    expect(screen.queryByTestId('schema-import-file-loading')).toBeNull();
    expect((screen.getByTestId('schema-import-text') as HTMLTextAreaElement).disabled).toBe(false);
  });
});

describe('rev706 C2.2 an account change during an apply keeps the warning', () => {
  async function pendingApply() {
    await pasteAndPreview();
    await choose('region', 'create');
    let release: (r: Response) => void = () => {};
    let fail: (e: Error) => void = () => {};
    answers.push(() => new Promise<Response>((res, rej) => ((release = res), (fail = rej))));
    await fireEvent.click(screen.getByTestId('schema-apply-button'));
    await waitFor(() => expect(calls.length).toBe(2));
    return { release: (r: Response) => release(r), fail: () => fail(new TypeError('Failed to fetch')) };
  }

  for (const ending of ['success', 'network failure'] as const) {
    it(`switching account while it is in flight warns, then ignores the ${ending}`, async () => {
      render(Page);
      const p = await pendingApply();
      auth.user = { ...USER_B };
      await waitFor(() => expect(screen.getByTestId('schema-lost-unknown').textContent).toContain('alice'));
      if (ending === 'success') p.release(new Response(JSON.stringify({ ...RECEIPT, replayed: false }), { status: 200 }));
      else p.fail();
      await new Promise((r) => setTimeout(r, 20));
      expect(screen.queryByTestId('schema-apply-receipt')).toBeNull();
      expect(screen.queryByTestId('schema-import-preview')).toBeNull();
      expect(screen.getByTestId('schema-lost-unknown').textContent).toContain('alice');
      expect(screen.getByTestId('schema-lost-unknown').querySelector('a[href="/admin/fields/import-history"]')).not.toBeNull();
      expect(calls.filter((c) => c.url.endsWith('/apply')).length).toBe(1);
    });
  }

  it('losing rights while it is in flight warns once rights return', async () => {
    render(Page);
    const p = await pendingApply();
    auth.caps = [];
    await waitFor(() => expect(screen.getByTestId('schema-exchange-denied')).toBeTruthy());
    p.release(new Response(JSON.stringify({ ...RECEIPT, replayed: false }), { status: 200 }));
    auth.caps = ['fields.admin'];
    await waitFor(() => expect(screen.getByTestId('schema-lost-unknown').textContent).toContain('alice'));
    expect(screen.queryByTestId('schema-apply-receipt')).toBeNull();
    expect(calls.filter((c) => c.url.endsWith('/apply')).length).toBe(1);
  });
});
