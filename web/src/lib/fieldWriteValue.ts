// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// THE WRITE SERIALIZER: field type + the control's current value →
// exactly ONE `value_*` member.
//
// This is the mapping FieldValuesSection has carried since #1389 and
// it is now shared, because the batch metadata editor (#1119, #1173)
// writes the same eleven types through a different endpoint. A second
// copy of an eleven-arm switch is a second place for a type to be
// forgotten, and the two would disagree silently: both endpoints
// answer 400 `value_type_mismatch` on the wrong member, so the failure
// would look like a server bug on one surface only.
//
// ⛔ NOT `fieldDisplay.ts`. That module is the READ side: it decides
// how a stored value is rendered and what counts as empty for display.
// The write side is a different question with a different answer, and
// conflating them is how a "clear" turns into a "render nothing".
//
// WHAT THIS DOES NOT DO, deliberately:
//
//   - No `if_absent` / `if_unchanged_since`. Those are the SINGLE
//     writer's per-value concurrency guard, keyed to a baseline that
//     surface loaded. A batch has no such baseline: its concurrency
//     guard is the preview token, which binds each target's own
//     `set_at`, and inventing a guard from a form would guard the
//     wrong thing.
//   - No validation and no canonicalisation. The server owns both.
//     Vocabulary aliases, merge tombstones, casing and whitespace
//     collapse, rich-text sanitising and pattern matching all resolve
//     server-side, and the batch preview hands back the canonical
//     `resolved_value` it will actually store.
//   - No invented clear. A `multi_select` with an empty option set is
//     a value the server refuses (400 `value_type_mismatch`), and it
//     stays refused: turning "the operator emptied the box" into "take
//     everything off every selected record" would be a destructive
//     operation nobody asked for. `remove` is the mode that removes.

/** The eleven operator-defined field types. */
export type FieldWriteType =
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

/** The five typed members, as the API models them. */
export interface FieldWriteValue {
  value_text?: string | null;
  value_num?: number | null;
  value_date?: string | null;
  value_options?: string[] | null;
  value_ref?: string | null;
}

/** Which member a type writes. The single source of the mapping the
 *  `AssetFieldValueWrite` and `BatchAssetFieldValue` schemas both
 *  describe in prose. */
export function writeMemberFor(type: FieldWriteType): keyof FieldWriteValue {
  switch (type) {
    case 'text':
    case 'longtext':
    case 'rich_text':
    case 'select':
    case 'tree':
      return 'value_text';
    case 'number':
    case 'boolean':
      return 'value_num';
    case 'date':
    case 'datetime':
      return 'value_date';
    case 'multi_select':
      return 'value_options';
    case 'reference':
      return 'value_ref';
  }
}

/**
 * The typed body: the ONE `value_*` member the field's type uses.
 *
 * Sending all five with `undefined` for the unused ones is what the
 * shipped model did, and it is why an emptied control produced a
 * request with no value member at all. One member, chosen by type,
 * cannot express that mistake.
 *
 * The three text-shaped types and the two single-slug vocabulary types
 * coalesce a missing value to `''`, which is how those types SAY empty.
 * The other five pass their member through as-is, `null` included,
 * which the server refuses with 400 `value_type_mismatch` rather than
 * inventing a meaning for. That refusal is the correct outcome: those
 * five types cannot express emptiness, so a blank control on one of
 * them is an unfinished form and not a request to clear.
 */
export function fieldWriteBody(type: FieldWriteType, v: FieldWriteValue): FieldWriteValue {
  switch (writeMemberFor(type)) {
    case 'value_text':
      return { value_text: v.value_text ?? '' };
    case 'value_num':
      return { value_num: v.value_num ?? null };
    case 'value_date':
      return { value_date: v.value_date ?? null };
    case 'value_options':
      return { value_options: v.value_options ?? [] };
    case 'value_ref':
      return { value_ref: v.value_ref ?? null };
  }
}

/**
 * Is this proposed value ready to send at all?
 *
 * The FIVE types that cannot express emptiness (number, boolean, date,
 * datetime, reference) must carry their member, and a `multi_select`
 * must carry at least one option. The batch schema says both outright
 * and answers 400 `value_type_mismatch` otherwise. Asking here as well
 * is a courtesy that keeps an operator from spending a round trip on
 * an obviously blank form; it is NOT the rule, and the server's
 * refusal stays visible when this check is not the one that ran.
 *
 * The text-shaped and single-slug types return true even when empty,
 * because "" is a value they can legitimately store and whether THIS
 * field accepts it is a property of the definition (a required field
 * answers 422 `required_value_empty`), not of the form.
 */
export function fieldWriteValuePresent(type: FieldWriteType, v: FieldWriteValue): boolean {
  switch (writeMemberFor(type)) {
    case 'value_text':
      return true;
    case 'value_num':
      return v.value_num != null;
    case 'value_date':
      return !!v.value_date;
    case 'value_options':
      return (v.value_options ?? []).length > 0;
    case 'value_ref':
      return !!v.value_ref;
  }
}
