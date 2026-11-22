// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1574 received assets: the shapes /federation/received-assets returns
// and the small formatting the two account pages share.
//
// Every name and type a sender put in its announcement is a CLAIM. Only
// a VERIFIED ORIGINAL (role original, state verified) has facts this
// instance established: its bytes were fetched and matched its `sha256`
// and `size_bytes`, and `detected_media_type` identifies them. Any other
// file (a preservation or access entry, state not_transferred) was never
// transferred: its `sha256` and `size_bytes` are the sender's claims and
// no fixity check was run on them. See [verifiedHere].

import type { components } from '$api/schema';

export type ReceivedAsset = components['schemas']['ReceivedAsset'];
export type ReceivedAssetDetail = components['schemas']['ReceivedAssetDetail'];
export type ReceivedAssetFile = components['schemas']['ReceivedAssetFile'];
/** The notice a withdrawn record answers (410) to a caller who could view it (#1575). */
export type ReceivedAssetWithdrawal = components['schemas']['ReceivedAssetWithdrawal'];

/** Human byte size, base 1024. */
export function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return i === 0 ? `${v} ${units[i]}` : `${v.toFixed(1)} ${units[i]}`;
}

/**
 * The sender's declared descriptive claims as label/value rows, in a
 * stable order. Objects (a description, a required statement) are shown
 * as their JSON; nothing here is interpreted or trusted.
 */
export function declaredEntries(declared: Record<string, unknown> | undefined): { key: string; value: string }[] {
  if (!declared) return [];
  return Object.keys(declared)
    .sort()
    .map((key) => {
      const v = declared[key];
      return { key, value: typeof v === 'string' ? v : JSON.stringify(v) };
    });
}

/** The original-file download URL; offered only when original_available. */
export function originalUrl(id: string): string {
  return `/api/v1/federation/received-assets/${encodeURIComponent(id)}/original`;
}

/**
 * Whether this instance itself established a file's size and SHA-256 (a
 * verified original). Anything else is shown as declared.
 */
export function verifiedHere(file: Pick<ReceivedAssetFile, 'role' | 'state'>): boolean {
  return file.role === 'original' && file.state === 'verified';
}
