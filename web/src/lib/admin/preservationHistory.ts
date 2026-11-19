// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Pure helpers for the operator's preservation-history view (#1598,
// /admin/storage/history). Kept out of the component so the wording and
// the paging rules are unit-tested, not just rendered.

import type { components } from '$api/schema';

export type PreservationEvent = components['schemas']['PreservationEvent'];
export type PreservationEventPage = components['schemas']['PreservationEventPage'];

/** Events per page. The API allows up to 200; a page this size stays
 *  readable and keeps the rendered list bounded. */
export const HISTORY_PAGE_SIZE = 25;

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** The asset id an operator typed or pasted, normalised, or null when
 *  it is not a UUID. Surrounding whitespace is forgiven. */
export function parseAssetId(input: string): string | null {
  const s = input.trim().toLowerCase();
  return UUID_RE.test(s) ? s : null;
}

export type OutcomeTone = 'success' | 'warning' | 'failure';

/** The visible outcome word. Colour is never the only signal: the word
 *  and an icon travel with it. */
export function outcomeLabel(outcome: PreservationEvent['outcome']): string {
  switch (outcome) {
    case 'success':
      return 'Passed';
    case 'warning':
      return 'Warning';
    case 'failure':
      return 'Failed';
  }
}

/** What the reason means, in plain words. */
export function reasonLabel(reason: PreservationEvent['outcome_reason']): string {
  switch (reason) {
    case 'digest_match':
      return 'The bytes match the checksum recorded for this file.';
    case 'no_recorded_digest':
      return 'The bytes were read and hashed, but this file has no recorded checksum to compare them with, so they could not be confirmed.';
    case 'checksum_mismatch':
      return 'The bytes do not match the checksum this file should have.';
    case 'size_mismatch':
      return 'The file is not the size it should be.';
    case 'missing_object':
      return 'The file is missing from storage.';
    case 'read_error':
      return 'Storage could not deliver the file, so it was not checked.';
  }
}

/** "Original" or "Derivative: <variant>". */
export function variantLabel(e: Pick<PreservationEvent, 'is_original' | 'variant_key'>): string {
  return e.is_original ? 'Original' : `Derivative: ${e.variant_key}`;
}

/** The row's fixity status after the check, in plain words. Neutral
 *  about WHY a row is unverifiable (no recorded checksum, an interrupted
 *  write, a failed read): the event's reason says that, not the status. */
export function statusLabel(status: PreservationEvent['fixity_status']): string {
  switch (status) {
    case 'verified':
      return 'verified';
    case 'unverifiable':
      return 'not verified';
    case 'mismatch':
      return 'does not match';
    case 'missing':
      return 'missing';
  }
}

/** A digest shortened for display; the full value goes in a title. */
export function shortDigest(d: string | null | undefined): string {
  if (!d) return 'none';
  return d.length > 16 ? `${d.slice(0, 12)}…${d.slice(-4)}` : d;
}

export function bytesLabel(n: number | null | undefined): string {
  if (n == null) return 'none';
  if (n < 1024) return `${n} B`;
  const units = ['KiB', 'MiB', 'GiB', 'TiB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`;
}

/** One labelled detail of an event, as the page renders it. */
export type EventDetail = { label: string; value: string; title?: string; mono?: boolean };

/** The details shown under an event, in order. Each says only what the
 *  event records: an absent recorded checksum is named as absent, a
 *  present one is shown, whatever the row's status. */
export function eventDetails(e: PreservationEvent): EventDetail[] {
  const run = e.sweep_run_id
    ? `${e.sweep_run_id.slice(0, 8)}${e.sweep_run_present ? '' : ' (run record deleted)'}`
    : 'none';
  return [
    { label: 'File', value: shortDigest(e.object_hash), title: e.object_hash, mono: true },
    { label: 'Status after the check', value: statusLabel(e.fixity_status) },
    {
      label: 'Read',
      value: e.observed_sha256
        ? `${e.algorithm ?? 'sha256'} ${shortDigest(e.observed_sha256)} · ${bytesLabel(e.observed_size_bytes)}`
        : 'nothing read',
      title: e.observed_sha256 ?? undefined,
      mono: true,
    },
    {
      label: 'Recorded',
      value: e.recorded_sha256 ? shortDigest(e.recorded_sha256) : 'no recorded checksum',
      title: e.recorded_sha256 ?? undefined,
      mono: true,
    },
    { label: 'Sweep run', value: run, title: e.sweep_run_id ?? undefined },
    { label: 'Recorded by', value: e.agent.version ? `${e.agent.name} ${e.agent.version}` : e.agent.name },
  ];
}

/**
 * Keyset paging only moves forward (older), so "Newer" is a stack of
 * the cursors that opened each page. Page 1 has no cursor. The page
 * changes the trail only after the page it asked for has loaded, so a
 * failed request leaves the trail, the page number and the rendered
 * page in agreement.
 */
export class CursorTrail {
  private trail: (string | null)[] = [null];

  /** The cursor that opened the current page (null for the newest). */
  get current(): string | null {
    return this.trail[this.trail.length - 1];
  }

  /** 1-based page number. */
  get page(): number {
    return this.trail.length;
  }

  get canGoNewer(): boolean {
    return this.trail.length > 1;
  }

  /** The cursor that opened the page before this one (null for the
   *  newest), i.e. what "Newer" will request. */
  get previous(): string | null {
    return this.trail.length > 1 ? this.trail[this.trail.length - 2] : null;
  }

  older(next: string): void {
    this.trail.push(next);
  }

  newer(): void {
    if (this.trail.length > 1) this.trail.pop();
  }

  reset(): void {
    this.trail = [null];
  }
}

/** "Events 26–50 of 120", for the page header. */
export function rangeLabel(page: number, shown: number, total: number): string {
  if (total === 0 || shown === 0) return `No events (${total} in total)`;
  const first = (page - 1) * HISTORY_PAGE_SIZE + 1;
  return `Events ${first}–${first + shown - 1} of ${total}`;
}
