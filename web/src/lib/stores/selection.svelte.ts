// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Shared multi-select primitive (#515 slice 3, feeds #39 bulk ops).
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
// clear() or the user.
//
// This file MUST stay `.svelte.ts`: `$state` in a plain `.ts` compiles
// but throws at runtime (rune_outside_svelte). See
// feedback_svelte_state_rune_extension.
//
// What the ids MEAN is context-dependent and intentionally NOT baked in
// here: a browse card contributes its post id, a collection / profile
// asset card contributes its asset id. This slice only builds the set +
// the card affordance; #39 formalises cross-kind bulk-op semantics (and
// may scope selection per surface). The store stays a plain string set.

class SelectionState {
  /** Selected ids, insertion-ordered. Source of truth (reactive array —
   *  the codebase's reactive-store convention; see auth.caps). */
  ids = $state<string[]>([]);

  /** O(1) membership without an includes() scan per card render. */
  private lookup = $derived(new Set(this.ids));

  /** How many are selected. */
  count = $derived(this.ids.length);

  /** True once anything is selected — cards use this to keep their
   *  checkbox visible (standard "selection mode" affordance). */
  active = $derived(this.ids.length > 0);

  has(id: string): boolean {
    return this.lookup.has(id);
  }

  toggle(id: string): void {
    if (this.lookup.has(id)) {
      this.ids = this.ids.filter((x) => x !== id);
    } else {
      this.ids = [...this.ids, id];
    }
  }

  add(id: string): void {
    if (!this.lookup.has(id)) this.ids = [...this.ids, id];
  }

  remove(id: string): void {
    if (this.lookup.has(id)) this.ids = this.ids.filter((x) => x !== id);
  }

  /** Union the given ids into the selection (for #39's select-all-in-view
   *  — the expansion itself is #39, this is the primitive it builds on). */
  selectAll(ids: string[]): void {
    const merged = new Set(this.ids);
    for (const id of ids) merged.add(id);
    this.ids = [...merged];
  }

  clear(): void {
    this.ids = [];
    this.anchor = null;
  }

  // ── Range selection (#1127) ────────────────────────────────────────
  //
  // The anchor and the range operation live HERE, in the one store, and
  // not in the grid or the list. #1063's rule: one selection store, no
  // per-view logic. Marquee, Shift+click and the list's Shift+Space all
  // end up in `extendTo` / `selectAll`, so "what is selected" has one
  // implementation and the three gestures differ only in how they
  // produce an id.
  //
  // FEED ORDER IS THE CALLER'S TO SUPPLY, and that is the important
  // seam. This store holds an insertion-ordered SET; it has no idea what
  // order the wall is in, and it must not guess — masonry's visual order
  // differs from feed order by design, and a store that sorted by its
  // own `ids` array would produce a range in click order rather than
  // feed order. So every range call takes the ordered id list the view
  // is actually rendering.
  //
  // #1127 names `aria-posinset` as the definition of feed order. That
  // attribute is only rendered by MasonryColumns — grid, thumbnail, feed
  // and list do not set it — so reading it would have worked in exactly
  // one of five modes. The ordered array each view already has IS the
  // feed order, in all five, and it is what these take.

  /** The last item a range gesture pivoted on. Public so a view can
   *  render it (nothing does yet) and, more importantly, so the
   *  "third shift-click extends from the new anchor" rule is visible
   *  state rather than a hidden variable. */
  anchor = $state<string | null>(null);

  /**
   * Shift-click semantics, exactly as specified.
   *
   * With NO anchor this is a plain check that also drops an anchor —
   * the first Shift+click of a flow selects one post. With an anchor it
   * selects the whole FEED-ORDER run between the anchor and `id`, and
   * then moves the anchor to `id`, so a third Shift+click extends from
   * there rather than from the original.
   *
   * ADDITIVE, never subtractive: a range never clears what was selected
   * before it. Shift+click in a file manager extends a selection; the
   * gesture that removes things is the plain checkbox, which is
   * untouched.
   *
   * An `id` outside `ordered` (a post whose page has been dropped, or a
   * stale anchor from a filter change) degrades to a plain add rather
   * than throwing — and cannot select a phantom, because everything it
   * can reach came out of the list the view just rendered.
   */
  extendTo(id: string, ordered: string[]): void {
    const to = ordered.indexOf(id);
    const from = this.anchor === null ? -1 : ordered.indexOf(this.anchor);
    if (to < 0 || from < 0) {
      this.add(id);
      this.anchor = id;
      return;
    }
    const [lo, hi] = from <= to ? [from, to] : [to, from];
    this.selectAll(ordered.slice(lo, hi + 1));
    this.anchor = id;
  }

  /** Set the pivot without changing the selection — what a plain
   *  (unmodified) checkbox click does, so the NEXT Shift+click has
   *  somewhere to extend from. */
  setAnchor(id: string | null): void {
    this.anchor = id;
  }
}

export const selection = new SelectionState();
