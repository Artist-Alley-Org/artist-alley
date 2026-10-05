// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1119 sprint 21d: the comments setting on the create wire.
//
// The store sends `comments_enabled` explicitly on every POST /posts,
// true unless the artist turned it off. What is under test is that the
// default is true, that an explicit false is SENT as false (a client
// that dropped falsy fields would turn the setting into a no-op), and
// that reset() puts the default back so one artist's choice does not
// follow the next composition.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const calls: { method: string; path: string; body?: unknown }[] = [];

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
    PUT: async () => ({ data: {}, error: undefined }),
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

function postBodies(): { comments_enabled?: unknown }[] {
  return calls.filter((c) => c.method === 'POST').map((c) => c.body as { comments_enabled?: unknown });
}

beforeEach(() => {
  calls.length = 0;
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

describe('comments setting on create (#1119 21d)', () => {
  it('defaults to enabled and says so on the wire', async () => {
    expect(upload.compose.commentsEnabled).toBe(true);
    expect(await upload.submit()).toBe(true);
    expect(postBodies()).toHaveLength(1);
    expect(postBodies()[0].comments_enabled).toBe(true);
  });

  it('an explicit off is sent as false, not dropped', async () => {
    upload.compose.commentsEnabled = false;
    expect(await upload.submit()).toBe(true);
    expect(postBodies()).toHaveLength(1);
    expect(postBodies()[0].comments_enabled).toBe(false);
    expect('comments_enabled' in postBodies()[0]).toBe(true);
  });

  it('one-per-file: every post gets the one answer the artist gave', async () => {
    upload.rows = [readyRow('a'), readyRow('b')];
    upload.compose.mode = 'one-per-file';
    upload.compose.commentsEnabled = false;
    expect(await upload.submit()).toBe(true);
    expect(postBodies().map((b) => b.comments_enabled)).toEqual([false, false]);
  });

  it('reset() restores the default for the next composition', async () => {
    upload.compose.commentsEnabled = false;
    upload.reset();
    expect(upload.compose.commentsEnabled).toBe(true);
  });
});
