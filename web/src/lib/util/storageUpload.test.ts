// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1595 — the shared raw upload sends the file's own name and returns
// the receipt that carries it to POST /assets.
//
// Pinned against a stub XHR, both directions:
//   - the header is the file name percent-encoded as UTF-8, so a name
//     outside ISO-8859-1 (which XHR would refuse raw) still travels, and
//     it is exactly File.name, nothing derived from title or extension;
//   - a file with no name sends no header at all (the server records
//     unknown) rather than an empty one (which it refuses);
//   - the receipt and the recorded name come back unchanged.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { originalFilenameHeader, putStorageObject } from './storageUpload';

class FakeXHR {
  static last: FakeXHR | null = null;
  headers: Record<string, string> = {};
  status = 0;
  response: unknown = null;
  responseType = '';
  withCredentials = false;
  upload = { addEventListener: () => {} };
  private listeners: Record<string, () => void> = {};
  constructor() {
    FakeXHR.last = this;
  }
  open() {}
  setRequestHeader(name: string, value: string) {
    // What a browser does with a non-ISO-8859-1 header value.
    if ([...value].some((c) => c.codePointAt(0)! > 0xff)) {
      throw new TypeError(`header ${name} is not ISO-8859-1`);
    }
    this.headers[name] = value;
  }
  addEventListener(type: string, fn: () => void) {
    this.listeners[type] = fn;
  }
  send() {
    this.status = 201;
    this.response = {
      hash: 'a'.repeat(64),
      deduped: false,
      upload_receipt: '6f1c2b8e-0d3a-4a5e-9b7f-2c4d6e8f0a1b',
      original_basename: '写真 été.png',
      original_basename_source: 'upload_request',
    };
    queueMicrotask(() => this.listeners.load?.());
  }
}

const opts = { networkMessage: 'network', abortMessage: 'aborted' };

describe('putStorageObject (#1595)', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    FakeXHR.last = null;
  });

  it('sends File.name percent-encoded and returns the receipt', async () => {
    vi.stubGlobal('XMLHttpRequest', FakeXHR);
    const file = new File(['x'], '写真 été.png', { type: 'image/png' });
    const res = await putStorageObject(file, opts);
    const sent = FakeXHR.last!.headers['X-Original-Filename'];
    expect(sent).toBe('%E5%86%99%E7%9C%9F%20%C3%A9t%C3%A9.png');
    expect(decodeURIComponent(sent)).toBe(file.name);
    expect(res.upload_receipt).toBe('6f1c2b8e-0d3a-4a5e-9b7f-2c4d6e8f0a1b');
    expect(res.original_basename).toBe('写真 été.png');
    expect(res.original_basename_source).toBe('upload_request');
  });

  it('sends no header for a nameless file', async () => {
    vi.stubGlobal('XMLHttpRequest', FakeXHR);
    await putStorageObject(new File(['x'], ''), opts);
    expect(FakeXHR.last!.headers).not.toHaveProperty('X-Original-Filename');
  });
});

describe('originalFilenameHeader', () => {
  it('encodes UTF-8, keeps ASCII, and encodes a literal percent', () => {
    expect(originalFilenameHeader('photo.jpg')).toBe('photo.jpg');
    expect(originalFilenameHeader('100%.jpg')).toBe('100%25.jpg');
    expect(originalFilenameHeader('a b.png')).toBe('a%20b.png');
    expect(originalFilenameHeader('')).toBeNull();
  });
});
