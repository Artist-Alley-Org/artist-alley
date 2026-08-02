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

/** The term carrying `slug`, at any depth, or undefined. */
export function findOption(all: FieldOption[], slug: string): FieldOption | undefined {
  return flattenOptions(all).find((o) => o.value === slug);
}

/** Map a stored slug to its display label, falling back to the slug. */
export function optionLabel(all: FieldOption[], slug: string): string {
  const hit = findOption(all, slug);
  return hit ? hit.label : slug;
}

/**
 * Free text → the lowercase, hyphenated form a stored option value
 * takes.
 *
 * Mirrors metadata.Slugify (app/internal/metadata/open_vocabulary.go)
 * exactly — lowercase, every run of non-alphanumerics collapsed to one
 * hyphen, trimmed, capped at 80 — because an open-vocabulary picker
 * PREVIEWS the slug the server is about to mint. A preview that does
 * not match what gets stored is worse than no preview: it tells the
 * operator a specific untruth about their own catalogue.
 *
 * Returns '' for input with no alphanumerics at all. That term has no
 * addressable form and cannot be created, which is what the server
 * says too.
 */
const SLUG_MAX_LEN = 80;
export function slugify(s: string): string {
  const out = s
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '');
  if (out.length <= SLUG_MAX_LEN) return out;
  return out.slice(0, SLUG_MAX_LEN).replace(/-+$/g, '');
}

/** What one free-text term resolves to against a field's vocabulary. */
export interface TermResolution {
  /** The existing term it addresses, if any. */
  option?: FieldOption;
  /** True when it addresses a term the field already has. */
  matched: boolean;
  /**
   * The slug it would be stored as. The matched term's slug when it
   * matched; the slugified input when it would be created; '' when the
   * term has no addressable form at all.
   */
  slug: string;
}

/**
 * Resolve one typed term the way the server's write path does, so the
 * picker's preview equals what gets stored.
 *
 * Mirrors indexVocabulary + resolveOrMint
 * (app/internal/metadata/open_vocabulary.go): match on slug OR label,
 * case-insensitive and whitespace-trimmed, at full depth; then on the
 * slugified form, which is how `Black & White` and `black_and_white`
 * both address `black-and-white` without becoming new terms. Archived
 * terms do not match by name — but their slugs are still taken, so a
 * term slugifying onto one comes back matched-and-archived rather than
 * as something creatable. The caller refuses it, which is the answer
 * the server gives.
 *
 * First-writer-wins on a duplicate key, matching the server's `add`.
 */
export function resolveTerm(all: FieldOption[], term: string): TermResolution {
  const trimmed = term.trim();
  const flat = flattenOptions(all);
  const matchable = new Map<string, FieldOption>();
  const taken = new Map<string, FieldOption>();
  for (const o of flat) {
    const slug = o.value.trim();
    if (!slug) continue;
    const key = slug.toLowerCase();
    if (!taken.has(key)) taken.set(key, { ...o, value: slug });
    if (o.status === 'archived') continue;
    if (!matchable.has(key)) matchable.set(key, o);
    const label = o.label.trim().toLowerCase();
    if (label && !matchable.has(label)) matchable.set(label, o);
  }

  const key = trimmed.toLowerCase();
  const byName = matchable.get(key);
  if (byName) return { option: byName, matched: true, slug: byName.value };

  const minted = slugify(trimmed);
  if (!minted) return { matched: false, slug: '' };
  const clash = taken.get(minted);
  if (clash) return { option: clash, matched: true, slug: clash.value };
  return { matched: false, slug: minted };
}

/**
 * The column each field type's value lives in. ONE table, so a display
 * surface and an editing surface cannot disagree about where to look —
 * which is exactly what happened to `tree` (#778): the editor wrote
 * `value_options`, the asset writer wrote `value_text`, and the detail
 * panel read `value_ref`, so a tree value rendered empty however it
 * had been written.
 *
 * Mirrors valueColumnFor in app/internal/metadata/valuecolumn_test.go.
 *
 * `tree` is `value_text` and holds ONE option slug — the node — not a
 * path string and not the array of slugs along the path. See the
 * 2026-07-31 tree-storage amendment to ADR 0012.
 *
 * `boolean` is `value_num`, holding 1 or 0. This table said
 * `value_text` until #791, matching what three frontend surfaces did
 * and contradicting both ADR 0012 and the API — see encodeBoolean /
 * decodeBoolean below, which every boolean surface now goes through.
 */
export const VALUE_COLUMN: Record<string, keyof FieldValueColumns> = {
  text: 'value_text',
  longtext: 'value_text',
  rich_text: 'value_text',
  select: 'value_text',
  tree: 'value_text',
  number: 'value_num',
  boolean: 'value_num',
  date: 'value_date',
  datetime: 'value_date',
  multi_select: 'value_options',
  reference: 'value_ref',
};

/**
 * A `boolean` field's value is the number 1 or 0 in `value_num` — ADR
 * 0012's encoding, chosen so the partial index on
 * (field_id, value_num) serves a "where flag = true" filter.
 *
 * Every boolean-handling surface goes through these two functions
 * rather than spelling the encoding out again. #791 was three
 * frontend sites each writing or reading the strings "true"/"false"
 * in `value_text` while the API accepted only 0/1 in `value_num`: the
 * detail panel rendered blank, and the upload modal's checkbox was
 * rejected outright by the asset write endpoint. Agreeing on the
 * column is not enough — "1" in value_text and 1 in value_num are
 * different answers, and so are 1 and true.
 */
export function encodeBoolean(v: boolean): number {
  return v ? 1 : 0;
}

/**
 * Read a stored boolean back. `null` means "not set" and is distinct
 * from `false` — a display surface renders nothing for the former and
 * "No" for the latter.
 */
export function decodeBoolean(n: number | null | undefined): boolean | null {
  if (n === 1) return true;
  if (n === 0) return false;
  return null;
}

export interface FieldValueColumns {
  value_text?: string | null;
  value_num?: number | null;
  value_date?: string | null;
  value_options?: string[] | null;
  value_ref?: string | null;
}

/** A vocabulary entry plus where it sits in the hierarchy. */
export interface FlatOption extends FieldOption {
  /** 0 for a top-level term, 1 for its children, and so on. */
  depth: number;
  /** Labels from the root down to and including this term. */
  path: string[];
}

/**
 * Walk a vocabulary depth-first into a flat list, carrying each term's
 * depth and ancestor labels.
 *
 * A flat select / multi_select vocabulary comes back unchanged at
 * depth 0, so callers do not need to know which kind they hold. A
 * `tree` field's nested terms come back in document order, which is
 * the order a picker should show them in.
 *
 * Slugs are unique across a field's WHOLE vocabulary — the server
 * rejects a duplicate at any depth — so the flattened list never
 * contains two entries with the same `value`.
 */
export function flattenOptions(
  opts: FieldOption[],
  depth = 0,
  ancestors: string[] = [],
): FlatOption[] {
  const out: FlatOption[] = [];
  for (const o of opts) {
    const path = [...ancestors, o.label];
    out.push({ ...o, depth, path });
    if (o.children?.length) {
      out.push(...flattenOptions(o.children, depth + 1, path));
    }
  }
  return out;
}

/**
 * The list to offer in a `tree` picker: every term at every depth,
 * filtered by the same selectable/held rule the flat picker uses.
 *
 * A non-leaf term stays selectable. "Europe" is a legitimate answer
 * when the operator does not know the city, and forbidding it would
 * force a fake leaf under every branch.
 */
export function selectableTreeOptions(
  all: FieldOption[],
  held: string[] = [],
): FlatOption[] {
  const heldSet = new Set(held.filter(Boolean));
  return flattenOptions(all).filter(
    (o) => isSelectable(o) || (heldSet.has(o.value) && isResolvable(o)),
  );
}
