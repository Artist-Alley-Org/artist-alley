// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/**
 * `edit_tab` gets its first consumer (#1173, #1119, ADR 0099 §9).
 *
 * # What was wrong
 *
 * Sprint 18b stored and validated `edit_tab` and shipped it with ZERO
 * consumers: an operator could assign a field to a tab and nothing
 * anywhere changed. This module is the bucketing rule that reads it, and
 * it lives here rather than inside a component because THREE surfaces
 * need the same answer (asset edit, collection edit, `/create`) and three
 * copies of a layout rule is three layouts.
 *
 * # ⛔ POLICY B, and it is the decision most likely to be undone by
 * accident
 *
 * BUCKETS DERIVE FROM COMPOSITION-ELIGIBLE **DEFINITIONS**, NOT FROM
 * CURRENTLY VISIBLE CONTROLS. So a tab whose every field is hidden by a
 * `display_condition` KEEPS ITS CHROME AND ITS SELECTION and shows an
 * empty-state line.
 *
 * The alternative, deriving the strip from what is visible, was rejected
 * because it makes the navigation move while a person is using it: a tab
 * would vanish from under the selection as a side effect of typing in a
 * different tab, and any unsaved work in the vanished tab would have
 * nowhere to be. Policy B costs one empty-state line and buys a strip
 * that does not move.
 *
 * Which is why `bucketFields` takes the DEFINITIONS and knows nothing
 * about conditions at all. Do not add a `visible` parameter here.
 *
 * # Tabs are COARSER than `display_group`
 *
 * A `display_group` fieldset lives INSIDE a tab; `display_order` remains
 * the ordering source. A tab is not a replacement for a group and the two
 * nest in one direction only.
 */

/** The subset of a field definition the bucketing rule reads. */
export interface TabbableField {
  /** `null` / absent = unassigned, which lands in the default bucket. */
  edit_tab?: string | null;
  display_order?: number;
}

/** One tab, with its members in the caller's original order. */
export interface FieldTabBucket<T extends TabbableField> {
  /**
   * Stable identity for the tab. `''` is the DEFAULT bucket, which is the
   * one representation of "unassigned"; a named tab's id is its name.
   *
   * Deliberately not an index: an index changes when a tab empties out,
   * and the selection is held by id so that it survives a reload and a
   * configuration change.
   */
  id: string;
  /**
   * The tab's name, or null for the default bucket. Null is what tells a
   * renderer to use its own translated "general" label rather than
   * printing an empty string.
   */
  name: string | null;
  fields: T[];
}

/**
 * Group composition-eligible definitions into tab buckets.
 *
 * The floor cases, which are the ones a test suite has to pin because
 * each is a different amount of chrome:
 *
 *   no eligible fields          -> 0 buckets, NO chrome, no phantom tab
 *   all unassigned              -> 1 default bucket, no strip
 *   one named only              -> 1 bucket, no strip
 *   one named + unassigned      -> 2 buckets, STRIP
 *   two named                   -> 2 buckets, STRIP
 *   two named + unassigned      -> 3 buckets, STRIP
 *
 * ORDER: the default bucket FIRST, then named buckets by their MINIMUM
 * MEMBER `display_order`, then by tab name as the tiebreak. Ordering
 * named tabs by their strongest member rather than alphabetically means
 * the operator's own `display_order` decides the strip, which is the same
 * source that decides everything else on the form; the name tiebreak is
 * what makes the strip stable across reloads when two tabs tie.
 *
 * NO UNASSIGNED FIELD MAY DISAPPEAR, which is the whole reason the
 * default bucket exists rather than the unassigned fields simply having
 * no tab.
 *
 * A blank or whitespace-only `edit_tab` is treated as unassigned. The
 * server refuses one on write and the CHECK refuses it in storage, so
 * this is belt and braces for a definition that arrived some other way,
 * and it means a stray space can never mint an unreachable tab.
 */
export function bucketFields<T extends TabbableField>(defs: T[]): FieldTabBucket<T>[] {
  if (defs.length === 0) return [];

  const named = new Map<string, T[]>();
  const unassigned: T[] = [];
  for (const d of defs) {
    const tab = (d.edit_tab ?? '').trim();
    if (tab === '') {
      unassigned.push(d);
      continue;
    }
    const bucket = named.get(tab);
    if (bucket) bucket.push(d);
    else named.set(tab, [d]);
  }

  const minOrder = (fields: T[]): number =>
    fields.reduce(
      (acc, f) => Math.min(acc, typeof f.display_order === 'number' ? f.display_order : 0),
      Number.POSITIVE_INFINITY,
    );

  const namedBuckets: FieldTabBucket<T>[] = [...named.entries()]
    .map(([name, fields]) => ({ id: name, name, fields }))
    .sort((a, b) => {
      const d = minOrder(a.fields) - minOrder(b.fields);
      if (d !== 0) return d;
      return a.name!.localeCompare(b.name!);
    });

  const out: FieldTabBucket<T>[] = [];
  if (unassigned.length > 0) out.push({ id: '', name: null, fields: unassigned });
  out.push(...namedBuckets);
  return out;
}

/**
 * Should a tab STRIP be drawn?
 *
 * Two or more buckets. One bucket is not a choice, and drawing a strip
 * with a single segment is chrome that says nothing, charged to every
 * install that never configured a tab.
 */
export function tabStripVisible(buckets: { length: number }): boolean {
  return buckets.length >= 2;
}

/**
 * Keep a selection valid as the bucket set changes.
 *
 * Returns the selection to use: the current one when it still names a
 * bucket, otherwise the FIRST bucket, otherwise null when there are none.
 *
 * ⛔ THE SELECTION IS NOT RESET WHEN A TAB MERELY EMPTIES OUT. Under
 * Policy B an emptied tab is still a bucket, so it still matches here and
 * the selection stays put. This function only moves the selection when
 * the tab genuinely stopped existing, which is a configuration change
 * rather than a consequence of typing.
 */
export function resolveTabSelection(
  current: string | null,
  buckets: { id: string }[],
): string | null {
  if (buckets.length === 0) return null;
  if (current !== null && buckets.some((b) => b.id === current)) return current;
  return buckets[0].id;
}
