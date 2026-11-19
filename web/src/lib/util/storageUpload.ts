// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/** Push one file's bytes to the content-addressed store.
 *
 *  Extracted from the upload store when #1207 gave the cover editor a
 *  second reason to do it. The call is four lines of XHR and three
 *  load-bearing details that are invisible from the call site — the
 *  raw `application/octet-stream` body, the `X-Content-Type` header the
 *  server sniffs the real type from, and `withCredentials` — and a
 *  second hand-written copy is a second place for one of them to be
 *  missing. It is XHR rather than `fetch` for the reason it always was:
 *  `fetch` still exposes no upload-progress events.
 *
 *  #1595: it also sends `file.name` as `X-Original-Filename` and hands
 *  back the server's `upload_receipt`. The receipt is what carries that
 *  name to `POST /assets` as the asset's write-once original base name;
 *  both callers must pass it on, and `metadata.original_filename` is no
 *  substitute (any client can send it and anyone with edit rights can
 *  change it). The name is percent-encoded because XHR refuses header
 *  values outside ISO-8859-1, and a Japanese or accented file name is
 *  not.
 *
 *  It does NOT create an asset. The two steps are separate because the
 *  callers legitimately differ on the second one: the upload queue
 *  builds an AssetCreate carrying tags, a mature label, pending field
 *  values and companions; a cover upload builds one carrying a title
 *  and a status. Sharing the bytes and not the body is the seam that
 *  actually has one right answer.
 */
export interface StorageUploadResult {
  hash: string;
  deduped?: boolean;
  /** Opaque, single use: pass it as `AssetCreate.upload_receipt`. Absent
   *  only from a server that predates #1595. */
  upload_receipt?: string;
  /** What the server recorded from `X-Original-Filename`, or null. */
  original_basename?: string | null;
  original_basename_source?: 'upload_request' | 'unknown';
}

/** The `X-Original-Filename` value for a file name: UTF-8,
 *  percent-encoded, so any name survives a header. The server strips any
 *  path and checks the rest; an empty name is not sent at all. */
export function originalFilenameHeader(name: string): string | null {
  return name ? encodeURIComponent(name) : null;
}

export interface StorageUploadOptions {
  /** 0..1, fired as the bytes go out. */
  onProgress?: (fraction: number) => void;
  /** Message for a transport failure — passed in so each caller keeps
   *  its own translated string rather than this module reaching for a
   *  catalogue it has no other business with. */
  networkMessage: string;
  /** Message for an aborted request. */
  abortMessage: string;
}

export function putStorageObject(
  file: File,
  opts: StorageUploadOptions,
): Promise<StorageUploadResult> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('POST', '/api/v1/storage/objects', true);
    xhr.setRequestHeader('Content-Type', 'application/octet-stream');
    xhr.setRequestHeader('X-Content-Type', file.type || 'application/octet-stream');
    const originalName = originalFilenameHeader(file.name);
    if (originalName) xhr.setRequestHeader('X-Original-Filename', originalName);
    xhr.responseType = 'json';
    xhr.withCredentials = true;

    xhr.upload.addEventListener('progress', (e) => {
      if (e.lengthComputable) opts.onProgress?.(e.loaded / e.total);
    });
    xhr.addEventListener('load', () => {
      if (xhr.status >= 200 && xhr.status < 300 && xhr.response) {
        opts.onProgress?.(1);
        resolve(xhr.response as StorageUploadResult);
      } else {
        const err =
          (xhr.response && (xhr.response as { error?: string }).error) || `HTTP ${xhr.status}`;
        reject(new Error(err));
      }
    });
    xhr.addEventListener('error', () => reject(new Error(opts.networkMessage)));
    xhr.addEventListener('abort', () => reject(new Error(opts.abortMessage)));

    xhr.send(file);
  });
}
