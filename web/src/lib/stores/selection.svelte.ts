// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Shared multi-select primitive (#515 slice 3, feeds #39 bulk ops and
// the batch metadata editor of #1119 / #1173).
//
// #39 is explicit that selection is a SHARED primitive — "reusable
// multi-select state, not card-local, so browse + collections + batch
// ops all consume it." So the selected set lives here, in a global
// singleton (same shape as site / browseView), NOT on any card or grid.
//
// Because it's a live singleton, the selection survives pagination,
// infinite-scroll re-renders, view-mode switches, and cross-surface
// navigation for free — exactly what #39 wants (select on page 1, keep
// it while scrolling to page 5). Nothing clears it but an explicit
// clear() or the user. In particular a batch preview, a refused batch
// preview, a refused apply and a COMMITTED apply all leave it standing:
// the operator decides when a selection is finished, not the server.
//
// This file MUST stay `.svelte.ts`: `$state` in a plain `.ts` compiles
// but throws at runtime (rune_outside_svelte). See
// feedback_svelte_state_rune_extension.
//
// # IDENTITY IS THE PAIR {kind, id}
//
// This store used to hold a bare `string[]` of ids and say, in its own
// header, that "what the ids MEAN is context-dependent and
// intentionally NOT baked in here". That was tenable while the only
// consumer was a count and a Clear button. It is not tenable now: the
// batch endpoints take a TYPED selection
// (`BatchAssetFieldSelectionEntry`, `required: [kind, id]`) precisely
// because "a server that guessed would expand a post id as an asset id
// and silently write nothing".
//
// So the kind is stored, not derived and not attached at serialisation
// time. Two consequences follow and both are the point:
//
//   1. A MIXED selection is expressible. The profile sweeps posts and
//      assets in one band (#1177) and each card contributes its own
//      kind, so the resulting set is exactly what the operator drew.
//   2. THE SAME UUID CAN BE SELECTED TWICE, once as an asset and once
//      as a post, and both survive. Nothing in the schema forbids an
//      asset and a post sharing an id, membership de-duplication is the
//      SERVER'S job at expansion time, and a store keyed on the bare
//      uuid would silently drop one of the two entries, which is a
//      selection the operator made and cannot see was lost.
//
// Kinds are exactly `asset` and `post`, mirroring
// `BatchAssetFieldSelectionKind`. COLLECTIONS ARE NOT A SELECTION KIND.

/** What one selection entry names. Mirrors the server's
 *  `BatchAssetFieldSelectionKind` enum exactly. */
export type SelectionKind = 'asset' | 'post';

/** ONE typed selection entry, the same shape the batch preview takes,
 *  so the request body is the stored entries and not a re-derivation of
 *  them. */
export interface SelectionEntry {
  kind: SelectionKind;
  id: string;
}

/** The set key. `kind` FIRST so the two halves can never run together
 *  into an ambiguous string, and so an asset and a post sharing a uuid
 *  land on two distinct keys. */
export function selectionKey(e: SelectionEntry): string {
  return `${e.kind}:${e.id}`;
}

/** Structural equality for two entries. */
export function sameEntry(a: SelectionEntry, b: SelectionEntry): boolean {
  return a.kind === b.kind && a.id === b.id;
}

class SelectionState {
  /** Selected entries, insertion-ordered. Source of truth (reactive
   *  array — the codebase's reactive-store convention; see auth.caps). */
  entries = $state<SelectionEntry[]>([]);

  /** O(1) membership without an includes() scan per card render. */
  private lookup = $derived(new Set(this.entries.map(selectionKey)));

  /** How many are selected. Entry count, NOT an expanded target count:
   *  what a selected post expands to is the server's answer and this
   *  store never guesses it. */
  count = $derived(this.entries.length);

  /** True once anything is selected — cards use this to keep their
   *  checkbox visible (standard "selection mode" affordance). */
  active = $derived(this.entries.length > 0);

  has(kind: SelectionKind, id: string): boolean {
    return this.lookup.has(selectionKey({ kind, id }));
  }

  hasEntry(e: SelectionEntry): boolean {
    return this.lookup.has(selectionKey(e));
  }

  toggle(e: SelectionEntry): void {
    if (this.hasEntry(e)) this.remove(e);
    else this.add(e);
  }

  add(e: SelectionEntry): void {
    if (!this.hasEntry(e)) this.entries = [...this.entries, { kind: e.kind, id: e.id }];
  }

  remove(e: SelectionEntry): void {
    if (!this.hasEntry(e)) return;
    this.entries = this.entries.filter((x) => !sameEntry(x, e));
  }

  /** Union the given entries into the selection (for #39's
   *  select-all-in-view, and for the range gestures below). */
  selectAll(list: SelectionEntry[]): void {
    const seen = new Set(this.lookup);
    const next = [...this.entries];
    for (const e of list) {
      const k = selectionKey(e);
      if (seen.has(k)) continue;
      seen.add(k);
      next.push({ kind: e.kind, id: e.id });
    }
    this.entries = next;
  }

  /** Replace the whole set in one assignment.
   *
   *  This exists FOR THE MARQUEE, which recomputes its answer from a
   *  pre-gesture snapshot on every frame and needs to commit the result
   *  atomically. It used to reach into `selection.ids` and assign the
   *  internal array directly, which meant the store's own invariants
   *  (de-duplication, entry copying, the anchor's relationship to the
   *  set) were enforced everywhere except in the one caller that
   *  rewrote everything. Entries are COPIED in, so the caller's
   *  snapshot array cannot alias the store's. */
  replace(list: SelectionEntry[]): void {
    const seen = new Set<string>();
    const next: SelectionEntry[] = [];
    for (const e of list) {
      const k = selectionKey(e);
      if (seen.has(k)) continue;
      seen.add(k);
      next.push({ kind: e.kind, id: e.id });
    }
    this.entries = next;
  }

  /** A detached copy, for a caller that needs to restore it later (the
   *  marquee's baseline, and its Escape undo). */
  snapshot(): SelectionEntry[] {
    return this.entries.map((e) => ({ kind: e.kind, id: e.id }));
  }

  clear(): void {
    this.entries = [];
    this.anchor = null;
  }

  // ── Range selection (#1127) ────────────────────────────────────────
  //
  // The anchor and the range operation live HERE, in the one store, and
  // not in the grid or the list. #1063's rule: one selection store, no
  // per-view logic. Marquee, Shift+click and the list's Shift+Space all
  // end up in `extendTo` / `selectAll`, so "what is selected" has one
  // implementation and the three gestures differ only in how they
  // produce an entry.
  //
  // FEED ORDER IS THE CALLER'S TO SUPPLY, and that is the important
  // seam. This store holds an insertion-ordered SET; it has no idea what
  // order the wall is in, and it must not guess — masonry's visual order
  // differs from feed order by design, and a store that sorted by its
  // own `entries` array would produce a range in click order rather than
  // feed order. So every range call takes the ordered entry list the
  // view is actually rendering.
  //
  // #1127 names `aria-posinset` as the definition of feed order. That
  // attribute is only rendered by MasonryColumns — grid, thumbnail, feed
  // and list do not set it — so reading it would have worked in exactly
  // one of five modes. The ordered array each view already has IS the
  // feed order, in all five, and it is what these take.

  /** The last entry a range gesture pivoted on. Public so a view can
   *  render it (nothing does yet) and, more importantly, so the
   *  "third shift-click extends from the new anchor" rule is visible
   *  state rather than a hidden variable. */
  anchor = $state<SelectionEntry | null>(null);

  /**
   * Shift-click semantics, exactly as specified.
   *
   * With NO anchor this is a plain check that also drops an anchor —
   * the first Shift+click of a flow selects one card. With an anchor it
   * selects the whole FEED-ORDER run between the anchor and `entry`,
   * and then moves the anchor to `entry`, so a third Shift+click
   * extends from there rather than from the original.
   *
   * ADDITIVE, never subtractive: a range never clears what was selected
   * before it. Shift+click in a file manager extends a selection; the
   * gesture that removes things is the plain checkbox, which is
   * untouched.
   *
   * An `entry` outside `ordered` (a post whose page has been dropped,
   * or a stale anchor from a filter change) degrades to a plain add
   * rather than throwing — and cannot select a phantom, because
   * everything it can reach came out of the list the view just
   * rendered.
   *
   * The index lookups compare the PAIR, not the id: a run that contains
   * both an asset and a post sharing a uuid has two distinct positions
   * in it, and matching on the id alone would collapse them.
   */
  extendTo(entry: SelectionEntry, ordered: SelectionEntry[]): void {
    const to = ordered.findIndex((e) => sameEntry(e, entry));
    const anchor = this.anchor;
    const from = anchor === null ? -1 : ordered.findIndex((e) => sameEntry(e, anchor));
    if (to < 0 || from < 0) {
      this.add(entry);
      this.anchor = { kind: entry.kind, id: entry.id };
      return;
    }
    const [lo, hi] = from <= to ? [from, to] : [to, from];
    this.selectAll(ordered.slice(lo, hi + 1));
    this.anchor = { kind: entry.kind, id: entry.id };
  }

  /** Set the pivot without changing the selection — what a plain
   *  (unmodified) checkbox click does, so the NEXT Shift+click has
   *  somewhere to extend from. */
  setAnchor(entry: SelectionEntry | null): void {
    this.anchor = entry === null ? null : { kind: entry.kind, id: entry.id };
  }
}

export const selection = new SelectionState();
