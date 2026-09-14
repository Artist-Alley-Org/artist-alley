// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1119 sprint 21e: the create flow's "publish later" is TWO durable
// operations, and the second can fail after the first has succeeded.
//
// The invariant under test is what the store does in that window. The
// draft is real; the schedule is not. A store that threw would take
// submit() down its catch arm, keep the rows, and hand the artist a
// retry that POSTs a SECOND post around the same files. So the
// failure is captured on `scheduleFailure`, submit() still resolves
// true, the rows are torn down, and the page reports "saved, not
// scheduled" with the draft one click away.
//
// Counting the calls is the assertion. One POST /posts, one PUT, no
// second POST on any path through the store. The persisted-row half
// of the same invariant (one draft, zero schedule, not published) is
// the Playwright spec's, against the real server.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const calls: { method: string; path: string; body?: unknown }[] = [];
let putOutcome: { data?: unknown; error?: unknown } = {
  data: {},
  error: undefined,
};

vi.mock('$api/client', () => ({
  api: {
    GET: async () => ({ data: undefined, error: undefined }),
    PATCH: async () => ({ data: {}, error: undefined }),
    POST: async (path: string, init: { body?: unknown }) => {
      calls.push({ method: 'POST', path, body: init?.body });
      return {
        data: { id: `post-${calls.filter((c) => c.method === 'POST').length}` },
        error: undefined,
      };
    },
    PUT: async (path: string, init: { body?: unknown }) => {
      calls.push({ method: 'PUT', path, body: init?.body });
      return putOutcome;
    },
  },
}));

import { upload, type UploadRow } from './upload.svelte';

function readyRow(id: string): UploadRow {
  return {
    id,
    file: new File(['x'], `${id}.txt`, { type: 'text/plain' }),
    relPath: `${id}.txt`,
    relPathKnown: false,
    batchId: null,
    objectUrl: `blob:${id}`,
    state: 'ready',
    progress: 100,
    hash: 'h',
    assetId: `asset-${id}`,
    deduped: false,
    title: id,
    tags: [],
    mature: false,
    aiProvenance: null,
    sentMature: null,
    sentAiProvenance: undefined,
    assetType: null,
    requirements: null,
    error: null,
    fieldValues: new Map(),
    fieldsWritten: false,
    fieldErrors: new Map(),
    companions: [],
    companionsWritten: false,
  } as unknown as UploadRow;
}

beforeEach(() => {
  calls.length = 0;
  putOutcome = { data: {}, error: undefined };
  vi.stubGlobal('URL', {
    ...URL,
    revokeObjectURL: () => undefined,
    createObjectURL: () => 'blob:',
  });
  upload.reset();
  upload.rows = [readyRow('a')];
  upload.compose.enabled = true;
  upload.compose.mode = 'one-post';
});

afterEach(() => {
  vi.unstubAllGlobals();
  upload.reset();
});

describe('publish later (#1119 21e)', () => {
  it('records the schedule against the post it just made', async () => {
    upload.compose.draft = true;
    upload.compose.scheduledFor = '2030-01-01T10:00:00.000Z';
    const ok = await upload.submit();
    expect(ok).toBe(true);
    expect(calls.map((c) => `${c.method} ${c.path}`)).toEqual([
      'POST /posts',
      'PUT /posts/{id}/publication-schedule',
    ]);
    expect((calls[0].body as { draft: boolean }).draft).toBe(true);
    expect(calls[1].body).toEqual({
      scheduled_for: '2030-01-01T10:00:00.000Z',
    });
    expect(upload.scheduleFailure).toBeNull();
    expect(upload.createdPostIds).toEqual(['post-1']);
  });

  it('a failed schedule leaves ONE draft, reports it, and cannot be retried into a second post', async () => {
    upload.compose.draft = true;
    upload.compose.scheduledFor = '2030-01-01T10:00:00.000Z';
    putOutcome = {
      data: undefined,
      error: { error: 'scheduled_for must be in the future' },
    };

    const ok = await upload.submit();

    // The draft exists, so this is a success with a caveat, not a
    // failure: composeError stays clear and the outcome is named.
    expect(ok, 'submit resolves true: the draft was made').toBe(true);
    expect(upload.composeError).toBeNull();
    expect(upload.scheduleFailure).toEqual({
      postId: 'post-1',
      message: 'scheduled_for must be in the future',
    });
    expect(upload.createdPostIds).toEqual(['post-1']);
    expect(calls.filter((c) => c.method === 'POST').length, 'exactly one post').toBe(1);

    // The rows are gone. A second submit has nothing to make a post
    // from, so the page cannot produce a second draft by retrying.
    expect(upload.rows).toEqual([]);
    const again = await upload.submit();
    expect(again).toBe(false);
    expect(calls.filter((c) => c.method === 'POST').length, 'still exactly one post').toBe(1);
  });

  it('a thrown schedule request is captured the same way', async () => {
    upload.compose.draft = true;
    upload.compose.scheduledFor = '2030-01-01T10:00:00.000Z';
    putOutcome = new Proxy(
      {},
      {
        get: () => {
          throw new Error('network down');
        },
      },
    ) as never;
    const ok = await upload.submit();
    expect(ok).toBe(true);
    expect(upload.scheduleFailure?.postId).toBe('post-1');
    expect(upload.scheduleFailure?.message).toBe('network down');
    expect(calls.filter((c) => c.method === 'POST').length).toBe(1);
  });

  it('ordinary publish and ordinary draft send no schedule', async () => {
    upload.compose.draft = false;
    upload.compose.scheduledFor = null;
    expect(await upload.submit()).toBe(true);
    expect(calls.map((c) => `${c.method} ${c.path}`)).toEqual(['POST /posts']);
    expect((calls[0].body as { draft: boolean }).draft).toBe(false);

    calls.length = 0;
    upload.rows = [readyRow('b')];
    upload.compose.enabled = true;
    upload.compose.draft = true;
    expect(await upload.submit()).toBe(true);
    expect(calls.map((c) => `${c.method} ${c.path}`)).toEqual(['POST /posts']);
    expect((calls[0].body as { draft: boolean }).draft).toBe(true);
    expect(upload.scheduleFailure).toBeNull();
  });

  it('a schedule without draft is not sent: scheduling is a draft plus an instruction', async () => {
    upload.compose.draft = false;
    upload.compose.scheduledFor = '2030-01-01T10:00:00.000Z';
    expect(await upload.submit()).toBe(true);
    expect(calls.map((c) => `${c.method} ${c.path}`)).toEqual(['POST /posts']);
  });

  it('one-per-file: each post carries its own instruction', async () => {
    upload.rows = [readyRow('a'), readyRow('b')];
    upload.compose.mode = 'one-per-file';
    upload.compose.draft = true;
    upload.compose.scheduledFor = '2030-01-01T10:00:00.000Z';
    expect(await upload.submit()).toBe(true);
    expect(calls.map((c) => `${c.method} ${c.path}`)).toEqual([
      'POST /posts',
      'PUT /posts/{id}/publication-schedule',
      'POST /posts',
      'PUT /posts/{id}/publication-schedule',
    ]);
    expect(upload.createdPostIds).toEqual(['post-1', 'post-2']);
  });

  it('reset clears the schedule from the composition', () => {
    upload.compose.scheduledFor = '2030-01-01T10:00:00.000Z';
    upload.reset();
    expect(upload.compose.scheduledFor).toBeNull();
  });
});
