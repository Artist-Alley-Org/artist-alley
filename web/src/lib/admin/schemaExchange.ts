// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Request bodies, bounds and calls for the field schema exchange (#1576 S4).
//
// The preview is computed over the document AS SENT, so the page must
// not re-encode it: a parse and re-serialize can change number
// spellings, escapes and member order, and the operator would be
// previewing a document they never held. buildPreviewBody therefore
// embeds the raw text unchanged. It parses the text only to prove it is
// exactly one JSON value, so that nothing typed into the box can add
// members to the envelope around it. Duplicate members and escapes are
// left for the server's strict decoder to judge.
//
// Bounds are checked before anything expensive: a file's size before it
// is read, a request's exact UTF-8 size before the text is parsed. The
// client refuses only what the server would refuse for size alone; the
// 2 MiB canonical document limit stays the server's to judge.
//
// The apply body is built once and then frozen: a retry after an
// ambiguous failure must send the same bytes, so the server either
// applies it once or answers with the recorded receipt.

import type { components } from '$api/schema';

export type Preview = components['schemas']['FieldSchemaImportPreview'];
export type PreviewField = components['schemas']['FieldSchemaImportPreviewField'];
export type ExportField = components['schemas']['FieldSchemaExportField'];
export type ExportOption = components['schemas']['FieldSchemaExportOption'];
export type Receipt = components['schemas']['FieldSchemaImportReceipt'];
export type Refusal = components['schemas']['FieldSchemaImportRefusal'];
export type ExportRefusal = components['schemas']['FieldSchemaExportRefusal'];

export const EXPORT_MAX_CODES = 100;

// The server's preview body bound (FieldSchemaImportPreviewMaxBodyBytes:
// the 2 MiB canonical document plus 64 KiB).
export const MAX_PREVIEW_REQUEST_BYTES = 2 * 1024 * 1024 + 64 * 1024;
// The bounds the server's S1 source-note decoder applies.
export const SOURCE_LABEL_MAX_CHARS = 200;
export const SOURCE_URI_MAX_BYTES = 2048;

// The 17 members of an aa-field-schema/1 field, in review order.
export const PORTABLE_KEYS = [
  'code', 'label', 'description', 'type', 'subject_kind', 'required', 'searchable', 'display_group',
  'display_order', 'options', 'regexp_filter', 'display_condition', 'show_on_card', 'show_in_advanced_search',
  'show_on_upload', 'edit_tab', 'open_vocabulary',
] as const;

export const ENDPOINTS = {
  export: '/api/v1/fields/export',
  preview: '/api/v1/fields/import/preview',
  apply: '/api/v1/fields/import/apply',
} as const;

const ENVELOPE_OVERHEAD = '{"document":}'.length;

/** A refusal before anything is sent; `key` is an i18n key under admin.fields.exchange.prepare. */
export class PrepareError extends Error {
  constructor(
    public key: string,
    public vars: Record<string, string | number> = {},
  ) {
    super(key);
  }
}

export function utf8Length(s: string): number {
  return new TextEncoder().encode(s).length;
}

/** True when the string holds a UTF-16 surrogate without its pair. */
export function hasLoneSurrogate(s: string): boolean {
  return /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/.test(s);
}

/**
 * Read a chosen file as exact UTF-8. It is refused before any read when
 * it cannot fit a preview request, and invalid bytes are refused rather
 * than replaced. A byte-order mark is kept, as a byte of the file.
 */
export async function readDocumentFile(file: File): Promise<string> {
  if (file.size > MAX_PREVIEW_REQUEST_BYTES - ENVELOPE_OVERHEAD) {
    throw new PrepareError('file_too_large', { max: MAX_PREVIEW_REQUEST_BYTES });
  }
  let buf: ArrayBuffer;
  try {
    buf = await file.arrayBuffer();
  } catch {
    throw new PrepareError('file_unreadable');
  }
  try {
    return new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(buf);
  } catch {
    throw new PrepareError('file_not_utf8');
  }
}

/** The typed source note, checked as the server checks it. Nothing is trimmed or repaired. */
export function checkSource(label: string, uri: string): { label?: string; uri?: string } | null {
  const s: { label?: string; uri?: string } = {};
  if (label !== '') {
    if (label.includes('\u0000')) throw new PrepareError('source_label_nul');
    if ([...label].length > SOURCE_LABEL_MAX_CHARS) throw new PrepareError('source_label_long', { max: SOURCE_LABEL_MAX_CHARS });
    s.label = label;
  }
  if (uri !== '') {
    if (utf8Length(uri) > SOURCE_URI_MAX_BYTES) throw new PrepareError('source_uri_long', { max: SOURCE_URI_MAX_BYTES });
    const m = /^([A-Za-z][A-Za-z0-9+.-]*):\/\/([^/?#]*)/.exec(uri);
    const scheme = m?.[1].toLowerCase();
    const host = m ? m[2].slice(m[2].lastIndexOf('@') + 1) : '';
    if (!m || (scheme !== 'http' && scheme !== 'https') || host === '') throw new PrepareError('source_uri_scheme');
    s.uri = uri;
  }
  return Object.keys(s).length ? s : null;
}

/**
 * The preview envelope, with the document text embedded byte for byte.
 * Checked in order of cost: unpaired surrogates (which UTF-8 encoding
 * would silently replace), then the exact request size, then that the
 * text is one JSON value.
 */
export function buildPreviewBody(rawDocument: string, source: { label?: string; uri?: string } | null): string {
  if (rawDocument === '') throw new PrepareError('document_empty');
  if (hasLoneSurrogate(rawDocument)) throw new PrepareError('document_lone_surrogate');
  let body = '{"document":' + rawDocument;
  if (source) body += ',"source":' + JSON.stringify(source);
  body += '}';
  if (utf8Length(body) > MAX_PREVIEW_REQUEST_BYTES) throw new PrepareError('request_too_large', { max: MAX_PREVIEW_REQUEST_BYTES });
  try {
    JSON.parse(rawDocument);
  } catch {
    throw new PrepareError('document_not_json');
  }
  return body;
}

/** Every code has a decision, and each is one the preview allows. */
export function allDecided(fields: PreviewField[], decisions: Record<string, string>): boolean {
  return fields.length > 0 && fields.every((f) => f.decisions.includes(decisions[f.code] ?? ''));
}

/** The apply body, decisions in document order. Built once per request. */
export function buildApplyBody(preview: Preview, decisions: Record<string, string>): string {
  const ordered: Record<string, string> = {};
  for (const f of preview.fields) ordered[f.code] = decisions[f.code];
  return JSON.stringify({ preview_id: preview.preview_id, document_digest: preview.document_digest, decisions: ordered });
}

export type PostResult =
  | { kind: 'answered'; status: number; body: unknown }
  // No usable answer: the request may or may not have taken effect.
  | { kind: 'ambiguous'; status: number | null; detail: string };

/** POST a prepared JSON text. A missing or unreadable answer is ambiguous, never a refusal. */
export async function postText(url: string, body: string, fetchImpl: typeof fetch = fetch): Promise<PostResult> {
  let res: Response;
  try {
    res = await fetchImpl(url, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body,
    });
  } catch (e) {
    return { kind: 'ambiguous', status: null, detail: e instanceof Error ? e.message : 'network error' };
  }
  let parsed: unknown = null;
  try {
    parsed = await res.json();
  } catch {
    return { kind: 'ambiguous', status: res.status, detail: 'unreadable answer' };
  }
  if (res.status >= 500) return { kind: 'ambiguous', status: res.status, detail: 'server error' };
  return { kind: 'answered', status: res.status, body: parsed };
}

const KNOWN_REASONS = new Set([
  'request_invalid', 'document_invalid', 'unsupported_format', 'document_too_large', 'preview_limit',
  'preview_not_found', 'digest_mismatch', 'preview_consumed', 'preview_stale', 'preview_expired',
  'decision_invalid', 'graph_refused', 'unstorable_value',
]);

/** The i18n key for a refusal reason; the reason code is always shown beside it. */
export function refusalKey(reason: string): string {
  return KNOWN_REASONS.has(reason) ? `admin.fields.exchange.reason.${reason}` : 'admin.fields.exchange.reason.other';
}

/** Refusals after which the same preview can never apply. */
export const DEAD_PREVIEW_REASONS = new Set(['preview_not_found', 'digest_mismatch', 'preview_consumed', 'preview_stale', 'preview_expired']);
