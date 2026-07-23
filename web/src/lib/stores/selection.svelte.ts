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
  }
}

export const selection = new SelectionState();
