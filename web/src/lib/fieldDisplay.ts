// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/**
 * The read-side formatter: one stored field value → what a reader
 * should see.
 *
 * This lived inside PostHost.svelte until #815/#817. It is out here
 * now for two reasons, in order of importance:
 *
 *  1. It was untestable in there. Both bugs this module was extracted
 *     to fix were timezone- and identity-shaped — the kind that pass a
 *     hand-check on the author's machine and fail on half the planet.
 *     A `<script>` block has no seam a unit test can reach.
 *  2. It is display logic with no component state, so it does not
 *     belong in a component in the first place.
 *
 * PostHost remains the only caller today. Keep it that way or make it
 * deliberate: a second copy of this switch is exactly the divergence
 * `fieldOptions.ts` was written to end.
 */

import { decodeBoolean } from '$lib/fieldOptions';

/**
 * A formatted value, ready to render.
 *
 * `text` is always present and always plain — the caller interpolates
 * it, never `{@html}`s it. `rich_text` deliberately returns its source
 * as escaped text here; rendering it as markup is #816's problem and
 * has a boundary of its own to design.
 *
 * `href` turns the entry into a link. Present only where the value
 * genuinely points at something in-app (today: a resolved `reference`).
 * It is a route path, never a URL from the server — nothing in the API
 * response is ever placed in an href, so this cannot become an open
 * redirect or a `javascript:` sink no matter what a peer sends us.
 */
export interface FieldDisplay {
  text: string;
  href?: string;
  /**
   * The value's independently-meaningful pieces, for a surface that
   * can render them as separate things — today, a multi_select's terms
   * as chips.
   *
   * `text` stays authoritative and stays the flat rendering: every
   * caller that only wants a string keeps working unchanged, and the
   * "does this field have a value" test is still `text !== ''`. A
   * renderer that understands `parts` uses it INSTEAD of `text`, never
   * as well.
   *
   * Deliberately optional and deliberately here rather than in a
   * second formatter. `formatFieldValue` is the one place that knows
   * how a stored value reads; a parallel `formatFieldValueAsChips`
   * would be the second copy of that switch, which is exactly the
   * divergence this module was extracted to end.
   */
  parts?: string[];
}

/** The `t()` from the lang store, as a parameter. */
export type Translate = (key: string, vars?: Record<string, string | number>) => string;

/** Display data for one resolved vocabulary slug (see ResolvedOption). */
export interface ResolvedOption {
  label: string;
  status: string;
  path?: string[] | null;
}

/** The asset a `reference` value points at (see ResolvedReference). */
export interface ResolvedReference {
  id: string;
  title: string;
}

/** One field value as the API ships it. Mirrors AssetFieldValue. */
export interface AssetFieldValue {
  field_id: string;
  field_code: string;
  field_label?: string;
  type:
    | 'text'
    | 'longtext'
    | 'rich_text'
    | 'number'
    | 'boolean'
    | 'date'
    | 'datetime'
    | 'select'
    | 'multi_select'
    | 'tree'
    | 'reference';
  value_text?: string | null;
  value_num?: number | null;
  value_date?: string | null;
  value_options?: string[] | null;
  value_ref?: string | null;
  set_by: string;
  set_at: string;
  // Display data for the vocabulary slugs this value holds, keyed by
  // slug. The server resolves it (ADR 0012 keeps only the slug on the
  // record) so a read surface never needs the field definition. A slug
  // missing from the map does not resolve and renders as itself.
  resolved_options?: Record<string, ResolvedOption> | null;
  // The asset a `reference` value points at, resolved server-side
  // (#817). Absent means it did not resolve — soft-deleted target, or
  // a dangling ref — and the value degrades to the bare UUID.
  resolved_reference?: ResolvedReference | null;
}

/**
 * Render one stored vocabulary slug the way a reader should see it.
 *
 * Falls back to the slug whenever the server could not resolve it,
 * which covers a term dropped from the vocabulary and — far more
 * commonly — an option written in the bare-string form, where the slug
 * IS the display text. Anything not active is marked with the same
 * string the picker uses, so a term stops looking current on the
 * detail surface the moment an operator retires it.
 */
export function displaySlug(f: AssetFieldValue, slug: string, t: Translate): string {
  const opt = f.resolved_options?.[slug];
  if (!opt) return slug;
  return opt.status === 'active' ? opt.label : t('common.option_deprecated', { label: opt.label });
}

/**
 * Render a stored tree slug as its path through the hierarchy.
 *
 * `path` is absent for a term sitting at the top level of its
 * vocabulary, where the label already says everything — so fall back
 * to the same single-slug rendering `select` uses, and to the raw slug
 * when the vocabulary no longer carries the term at all.
 */
export function displayTreePath(f: AssetFieldValue, slug: string, t: Translate): string {
  const opt = f.resolved_options?.[slug];
  if (!opt) return slug;
  const label =
    opt.status === 'active' ? opt.label : t('common.option_deprecated', { label: opt.label });
  if (!opt.path?.length || opt.path.length < 2) return label;
  // Keep the deprecation marker attached to the term itself, not to
  // the whole path — an ancestor is not what was retired.
  return [...opt.path.slice(0, -1), label].join(' / ');
}

export function formatFieldValue(f: AssetFieldValue, t: Translate): FieldDisplay {
  switch (f.type) {
    case 'text':
    case 'longtext':
    case 'rich_text':
      return { text: (f.value_text ?? '').trim() };
    case 'number':
      return { text: f.value_num == null ? '' : String(f.value_num) };
    case 'boolean': {
      // 1/0 in value_num (ADR 0012). This read the strings
      // "true"/"false" out of value_text until #791, so an asset
      // boolean — which the API only ever accepted as value_num —
      // rendered blank whichever way it had been set.
      //
      // null is "not set" and stays blank; false is an answer and
      // prints, which is why this goes through decodeBoolean rather
      // than testing truthiness (0 is falsy).
      const b = decodeBoolean(f.value_num);
      return { text: b === null ? '' : b ? t('common.yes') : t('common.no') };
    }
    case 'date':
    case 'datetime': {
      if (!f.value_date) return { text: '' };
      const d = new Date(f.value_date);
      if (isNaN(d.getTime())) return { text: '' };
      // The #815 split. A `date` value is a CALENDAR DATE — a
      // license expiring on 2026-01-01 expires on that day for
      // everyone, and the instant it is stored at (midnight UTC) is an
      // encoding artefact, not information. Localising that instant
      // subtracted a day for every viewer west of UTC: the seeded
      // license_expires = 2026-01-01T00:00:00Z rendered "12/31/2025"
      // across the Americas and read as already-expired.
      //
      // So `date` formats from the UTC calendar fields and never
      // converts. ISO rather than toLocaleDateString(…, {timeZone:
      // 'UTC'}) on purpose: YYYY-MM-DD is unambiguous in every locale
      // (no 01/02 day-month coin-flip), and — the reason that matters
      // here — it has no timezone input at all, so the bug cannot be
      // reintroduced by a formatting tweak later.
      //
      // `datetime` keeps localising, because for a datetime the
      // instant IS the information: "ingested at" happened at a moment
      // in time and a reader wants it in their own clock.
      if (f.type === 'date') return { text: d.toISOString().slice(0, 10) };
      return { text: d.toLocaleString() };
    }
    case 'select':
      return { text: f.value_text ? displaySlug(f, f.value_text, t) : '' };
    case 'multi_select': {
      // The one type whose value is a SET, so it is the one type with
      // parts. Comma-joining them read as one long sentence in which
      // no individual term was findable, and on an open vocabulary —
      // where the set is the point — that gets worse the more useful
      // the field is.
      const parts = (f.value_options ?? []).map((s) => displaySlug(f, s, t));
      return { text: parts.join(', '), parts };
    }
    case 'tree':
      // One slug in value_text, resolved to its full ancestor path so
      // the hierarchy is visible: "Europe / United Kingdom / London".
      //
      // This case used to read value_ref, which no writer has ever
      // populated for a tree field — so a tree value rendered empty
      // regardless of which of the two columns it had been written to
      // (#778). value_ref is for `reference`, whose value is a row's
      // UUID; an option is an entry in a jsonb document and has no
      // identity of its own to point at.
      return { text: f.value_text ? displayTreePath(f, f.value_text, t) : '' };
    case 'reference': {
      // #817. value_ref is a bare UUID, and printing it raw showed a
      // reader a 36-character identifier where a title belongs.
      //
      // No resolved_reference means the server could not resolve the
      // target for THIS caller — soft-deleted, or a dangling ref.
      // Degrade to the id and, critically, do NOT link it: a link to a
      // row that did not resolve is a promise of a 404. The id itself
      // is not a leak; it was already on the record being read.
      const ref = f.resolved_reference;
      if (!ref?.id) return { text: f.value_ref ?? '' };
      // assets.title DEFAULT '' — an untitled asset is ordinary, and
      // an empty link label is not clickable, so fall back to the id
      // as the label while keeping the link.
      return { text: ref.title.trim() || ref.id, href: `/assets/${ref.id}` };
    }
    default:
      return { text: '' };
  }
}
