// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/**
 * The upload-default document (#793, ADR 0081 §3).
 *
 * A default is one of exactly two things and never an expression:
 *
 *   {"kind": "literal", "value_text": "greybox"}
 *   {"kind": "context", "context": "uploading_user"}
 *
 * The server validates both on write, so nothing here is load-bearing
 * for correctness — this module exists so the editing surface can offer
 * only choices that will be accepted, and so the upload modal can say
 * what a field is going to be filled with. An editor that offers a
 * choice the server rejects is a worse experience than one that never
 * offers it.
 */

import { VALUE_COLUMN, type FieldValueColumns } from '$lib/fieldOptions';

export type FieldDefaultKind = 'literal' | 'context';

export type DefaultContext = 'uploading_user' | 'uploading_team' | 'current_date';

export interface FieldDefault extends Partial<FieldValueColumns> {
  kind: FieldDefaultKind;
  context?: DefaultContext;
}

/**
 * Which contexts can fill which column. Mirrors contextTargetType in
 * app/internal/metadata/defaults.go, and is derived through
 * VALUE_COLUMN so a field type moving column moves its contexts with
 * it rather than silently keeping the old set.
 */
const CONTEXT_COLUMN: Record<DefaultContext, keyof FieldValueColumns> = {
  uploading_user: 'value_text',
  uploading_team: 'value_text',
  current_date: 'value_date',
};

/** Human wording for the picker, keyed so i18n can replace it. */
export const CONTEXT_KEYS: Record<DefaultContext, string> = {
  uploading_user: 'admin.fields.default_context_uploading_user',
  uploading_team: 'admin.fields.default_context_uploading_team',
  current_date: 'admin.fields.default_context_current_date',
};

/** The contexts an operator may pick for a field of this type. */
export function contextsForType(fieldType: string): DefaultContext[] {
  const col = VALUE_COLUMN[fieldType];
  if (!col) return [];
  return (Object.keys(CONTEXT_COLUMN) as DefaultContext[]).filter(
    (c) => CONTEXT_COLUMN[c] === col,
  );
}

/** Field types that can carry a default at all. */
export function typeSupportsDefault(fieldType: string): boolean {
  return Boolean(VALUE_COLUMN[fieldType]);
}

/**
 * A one-line description of what a default will put on an asset.
 *
 * Used by the upload modal, which is the whole point of the feature:
 * an artist who can see that `Stage` is about to be "Greybox" does not
 * have to open the field to find out.
 */
export function describeDefault(
  d: FieldDefault | null | undefined,
  fieldType: string,
  labelFor: (slug: string) => string,
  contextLabel: (c: DefaultContext) => string,
  boolLabel: (on: boolean) => string,
): string {
  if (!d) return '';
  if (d.kind === 'context') {
    return d.context ? contextLabel(d.context) : '';
  }
  if (typeof d.value_num === 'number') {
    // A boolean is 1/0 in value_num (ADR 0012, pinned by #791).
    // Rendering the raw number is technically accurate and useless to
    // read — "defaults to 1" tells an artist nothing.
    return fieldType === 'boolean' ? boolLabel(d.value_num === 1) : String(d.value_num);
  }
  if (typeof d.value_text === 'string') return labelFor(d.value_text);
  if (typeof d.value_date === 'string') return d.value_date;
  if (Array.isArray(d.value_options)) {
    return d.value_options.map(labelFor).join(', ');
  }
  if (typeof d.value_ref === 'string') return d.value_ref;
  return '';
}

/**
 * Build the document for a literal, putting the value in the one member
 * the field's type reads. Returns null when there is nothing to store —
 * "no default" and "a default that is blank" are different states and
 * the caller must not conflate them.
 */
export function literalDefault(fieldType: string, raw: string | string[]): FieldDefault | null {
  const col = VALUE_COLUMN[fieldType];
  if (!col) return null;

  if (col === 'value_options') {
    const list = (Array.isArray(raw) ? raw : [raw]).filter(Boolean);
    return list.length ? { kind: 'literal', value_options: list } : null;
  }

  const text = Array.isArray(raw) ? (raw[0] ?? '') : raw;
  if (!text.trim()) return null;

  switch (col) {
    case 'value_text':
      return { kind: 'literal', value_text: text.trim() };
    case 'value_num': {
      const n = Number(text);
      return Number.isFinite(n) ? { kind: 'literal', value_num: n } : null;
    }
    case 'value_date': {
      const d = new Date(text);
      return Number.isNaN(d.getTime()) ? null : { kind: 'literal', value_date: d.toISOString() };
    }
    case 'value_ref':
      return { kind: 'literal', value_ref: text.trim() };
  }
  return null;
}

/** Pull the editable scalar back out of a stored literal. */
export function literalText(d: FieldDefault | null | undefined): string {
  if (!d || d.kind !== 'literal') return '';
  if (typeof d.value_text === 'string') return d.value_text;
  if (typeof d.value_num === 'number') return String(d.value_num);
  if (typeof d.value_date === 'string') return d.value_date;
  if (typeof d.value_ref === 'string') return d.value_ref;
  return '';
}

/** Pull the editable list back out of a stored multi_select literal. */
export function literalList(d: FieldDefault | null | undefined): string[] {
  if (!d || d.kind !== 'literal' || !Array.isArray(d.value_options)) return [];
  return [...d.value_options];
}
