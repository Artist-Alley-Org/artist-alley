// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/**
 * The controlled-vocabulary half of `field_definition.options`
 * (ADR 0012 + its 2026-07-30 amendment).
 *
 * Two entry shapes exist in stored data and both must keep working:
 *
 *   {"values": ["sRGB", "Linear"]}                  // bare slug
 *   {"values": [{"value": "srgb", "label": "sRGB"}]} // object
 *
 * The bare form is what the seeder writes and what every
 * options-carrying field holds on a live instance today; the object
 * form is what ADR 0012 documents. The server decodes both and writes
 * each entry back in the narrowest form that still carries its
 * information, so a vocabulary nobody has given a label or a status to
 * round-trips unchanged. Mirror that here rather than picking a side —
 * before this module the two frontend consumers each assumed a
 * different shape and only agreed by accident.
 */

export type OptionStatus = 'active' | 'deprecated' | 'archived';

/** One vocabulary entry, normalised. */
export interface FieldOption {
  value: string;
  label: string;
  status: OptionStatus;
  replaced_by?: string;
  children?: FieldOption[];
}

/** The wire shape: either a bare slug or a partial object. */
type RawOption =
  | string
  | {
      value?: unknown;
      label?: unknown;
      status?: unknown;
      replaced_by?: unknown;
      children?: unknown;
    };

function isStatus(v: unknown): v is OptionStatus {
  return v === 'active' || v === 'deprecated' || v === 'archived';
}

function normalizeOne(raw: RawOption): FieldOption | null {
  if (typeof raw === 'string') {
    return { value: raw, label: raw, status: 'active' };
  }
  if (!raw || typeof raw !== 'object') return null;
  const value = typeof raw.value === 'string' ? raw.value : '';
  if (!value) return null;
  const label = typeof raw.label === 'string' && raw.label ? raw.label : value;
  // An absent status means active — that is what keeps every
  // pre-lifecycle document valid.
  const status = isStatus(raw.status) ? raw.status : 'active';
  const out: FieldOption = { value, label, status };
  if (typeof raw.replaced_by === 'string' && raw.replaced_by) {
    out.replaced_by = raw.replaced_by;
  }
  if (Array.isArray(raw.children)) {
    const kids = normalizeOptions({ values: raw.children });
    if (kids.length) out.children = kids;
  }
  return out;
}

/** Read `options.values` from a field definition into the model. */
export function normalizeOptions(options: unknown): FieldOption[] {
  if (!options || typeof options !== 'object') return [];
  const values = (options as { values?: unknown }).values;
  if (!Array.isArray(values)) return [];
  return values
    .map((v) => normalizeOne(v as RawOption))
    .filter((v): v is FieldOption => v !== null);
}

/**
 * Write the model back out, narrowest-form-first so an untouched
 * vocabulary serialises byte-identical to what was loaded. Mirrors
 * FieldOption.MarshalJSON in app/internal/metadata/options.go.
 */
export function serializeOptions(opts: FieldOption[]): unknown[] {
  return opts.map((o) => {
    const bare =
      (o.label === '' || o.label === o.value) &&
      o.status === 'active' &&
      !o.replaced_by &&
      !o.children?.length;
    if (bare) return o.value;
    const out: Record<string, unknown> = { value: o.value };
    if (o.label && o.label !== o.value) out.label = o.label;
    if (o.status !== 'active') out.status = o.status;
    if (o.replaced_by) out.replaced_by = o.replaced_by;
    if (o.children?.length) out.children = serializeOptions(o.children);
    return out;
  });
}

/** Offered for a NEW value. Deprecated and archived terms are not. */
export function isSelectable(o: FieldOption): boolean {
  return o.status === 'active';
}

/** Still resolves and displays on an asset already carrying it. */
export function isResolvable(o: FieldOption): boolean {
  return o.status !== 'archived';
}

/**
 * The list to offer in a picker.
 *
 * `held` is whatever the record already stores. A deprecated term must
 * stop being offered WITHOUT vanishing from a record that already
 * carries it — dropping it would blank the value on an asset nobody
 * edited, which is the failure ADR 0012 exists to prevent. So held
 * values survive the filter even when they are no longer selectable.
 */
export function selectableOptions(
  all: FieldOption[],
  held: string[] = [],
): FieldOption[] {
  const heldSet = new Set(held.filter(Boolean));
  return all.filter(
    (o) => isSelectable(o) || (heldSet.has(o.value) && isResolvable(o)),
  );
}

/** Map a stored slug to its display label, falling back to the slug. */
export function optionLabel(all: FieldOption[], slug: string): string {
  const hit = all.find((o) => o.value === slug);
  return hit ? hit.label : slug;
}
