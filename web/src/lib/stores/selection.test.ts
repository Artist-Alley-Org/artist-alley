// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The selection store's IDENTITY contract (#1119, #1173).
//
// # What these are red against
//
// The shipped store held `ids = $state<string[]>([])` and said so in
// its own header: "What the ids MEAN is context-dependent and
// intentionally NOT baked in here ... The store stays a plain string
// set." Every assertion in the first two describes below fails against
// that store, and fails for the intended reason rather than by
// accident:
//
//   - `selection.entries` does not exist on it at all, so a mixed
//     asset+post selection cannot be READ BACK as typed identities.
//   - `selection.has(kind, id)` is a one-argument `has(id)` on it, so
//     the same-uuid pair test asks it a question it cannot answer, and
//     the pair it stored is two indistinguishable strings.
//
// This is exactly the gap the SERVER anticipated:
// `BatchAssetFieldSelectionEntry` is `required: [kind, id]` because "a
// server that guessed would expand a post id as an asset id and
// silently write nothing".
//
// # Why the pair, and not a uuid with a kind attached at send time
//
// A store keyed on the bare uuid cannot hold an asset and a post that
// share one. Nothing in the schema forbids that pair, both cards are
// separately visible and separately tickable, and de-duplication is
// the SERVER's job at expansion time. A client that collapsed them
// would drop a selection the operator made and could still see on
// screen.

import { beforeEach, describe, expect, it } from 'vitest';
import { selection, selectionKey, type SelectionEntry } from './selection.svelte';

/** A uuid an asset and a post both use, for the coexistence cases. */
const SHARED = '11111111-1111-4111-8111-111111111111';
const A = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const B = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
const C = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';

const asset = (id: string): SelectionEntry => ({ kind: 'asset', id });
const post = (id: string): SelectionEntry => ({ kind: 'post', id });

beforeEach(() => {
  selection.clear();
});

describe('a mixed selection is retained as TYPED identities', () => {
  it('keeps each entry\'s kind, in insertion order', () => {
    selection.add(post(A));
    selection.add(asset(B));
    selection.add(post(C));

    expect(selection.entries).toEqual([
      { kind: 'post', id: A },
      { kind: 'asset', id: B },
      { kind: 'post', id: C },
    ]);
  });

  it('IS the batch request payload, not a re-derivation of it', () => {
    // The entries already have the shape
    // `BatchAssetFieldSelectionEntry` requires, which is the whole
    // reason the kind is stored rather than attached at send time:
    // there is no later moment where a kind has to be guessed.
    selection.add(asset(A));
    selection.add(post(B));

    for (const e of selection.entries) {
      expect(Object.keys(e).sort()).toEqual(['id', 'kind']);
      expect(['asset', 'post']).toContain(e.kind);
      expect(typeof e.id).toBe('string');
    }
  });

  it('reports membership per KIND, so a post id is not an asset id', () => {
    selection.add(post(A));

    expect(selection.has('post', A)).toBe(true);
    expect(selection.has('asset', A)).toBe(false);
  });
});

describe('an asset and a post sharing ONE uuid both survive', () => {
  it('holds both, and counts both', () => {
    selection.add(asset(SHARED));
    selection.add(post(SHARED));

    expect(selection.count).toBe(2);
    expect(selection.has('asset', SHARED)).toBe(true);
    expect(selection.has('post', SHARED)).toBe(true);
    expect(selection.entries).toEqual([
      { kind: 'asset', id: SHARED },
      { kind: 'post', id: SHARED },
    ]);
  });

  it('removes exactly one of the pair', () => {
    selection.add(asset(SHARED));
    selection.add(post(SHARED));

    selection.remove(asset(SHARED));

    expect(selection.count).toBe(1);
    expect(selection.has('asset', SHARED)).toBe(false);
    expect(selection.has('post', SHARED)).toBe(true);
  });

  it('toggles exactly one of the pair', () => {
    selection.toggle(asset(SHARED));
    selection.toggle(post(SHARED));
    expect(selection.count).toBe(2);

    selection.toggle(post(SHARED));

    expect(selection.entries).toEqual([{ kind: 'asset', id: SHARED }]);
  });

  it('keys on the pair, kind first', () => {
    expect(selectionKey(asset(SHARED))).not.toBe(selectionKey(post(SHARED)));
  });
});

describe('de-duplication is per PAIR', () => {
  it('adding the same pair twice is one entry', () => {
    selection.add(asset(A));
    selection.add(asset(A));

    expect(selection.count).toBe(1);
  });

  it('selectAll unions without duplicating', () => {
    selection.add(post(A));
    selection.selectAll([post(A), asset(A), post(B)]);

    expect(selection.entries).toEqual([
      { kind: 'post', id: A },
      { kind: 'asset', id: A },
      { kind: 'post', id: B },
    ]);
  });

  it('replace de-duplicates and copies, so the caller cannot alias the store', () => {
    const incoming = [asset(A), asset(A), post(B)];
    selection.replace(incoming);

    expect(selection.count).toBe(2);
    incoming[0].id = 'mutated';
    expect(selection.entries[0].id).toBe(A);
  });
});

describe('range selection over a MIXED ordered list', () => {
  // Feed order on the profile is posts then assets, one band sweeping
  // both (#1177). The run between two entries therefore crosses a kind
  // boundary, and the index lookups have to compare the PAIR: matching
  // on the id alone would collapse a same-uuid pair into one position
  // and select the wrong run.
  const ordered = [post(A), post(SHARED), asset(SHARED), asset(B), asset(C)];

  it('selects the whole run, kinds intact', () => {
    selection.toggle(post(A));
    selection.setAnchor(post(A));

    selection.extendTo(asset(B), ordered);

    expect(selection.entries).toEqual([
      { kind: 'post', id: A },
      { kind: 'post', id: SHARED },
      { kind: 'asset', id: SHARED },
      { kind: 'asset', id: B },
    ]);
  });

  it('distinguishes the two same-uuid positions as the anchor', () => {
    selection.setAnchor(asset(SHARED));

    selection.extendTo(asset(C), ordered);

    // From the ASSET at SHARED, not from the POST that precedes it.
    expect(selection.entries).toEqual([
      { kind: 'asset', id: SHARED },
      { kind: 'asset', id: B },
      { kind: 'asset', id: C },
    ]);
  });

  it('moves the anchor to the entry it ended on', () => {
    selection.setAnchor(post(A));
    selection.extendTo(asset(B), ordered);

    expect(selection.anchor).toEqual({ kind: 'asset', id: B });
  });

  it('degrades to a plain add for an entry outside the order', () => {
    const stray = asset('dddddddd-dddd-4ddd-8ddd-dddddddddddd');
    selection.setAnchor(post(A));

    selection.extendTo(stray, ordered);

    expect(selection.entries).toEqual([stray]);
  });
});

describe('the marquee seam', () => {
  // The band used to assign `selection.ids` directly, which meant the
  // store's own invariants were enforced everywhere except in the one
  // caller that rewrote the entire set.
  it('snapshot is detached from the store', () => {
    selection.add(asset(A));
    const snap = selection.snapshot();

    selection.add(post(B));

    expect(snap).toEqual([{ kind: 'asset', id: A }]);
  });

  it('replace(snapshot) is a complete undo', () => {
    selection.add(asset(A));
    selection.add(post(B));
    const snap = selection.snapshot();

    selection.replace([asset(C)]);
    expect(selection.entries).toEqual([{ kind: 'asset', id: C }]);

    selection.replace(snap);
    expect(selection.entries).toEqual([
      { kind: 'asset', id: A },
      { kind: 'post', id: B },
    ]);
  });
});

describe('nothing but clear() empties it', () => {
  it('clear drops the entries and the anchor', () => {
    selection.add(asset(A));
    selection.setAnchor(asset(A));

    selection.clear();

    expect(selection.count).toBe(0);
    expect(selection.active).toBe(false);
    expect(selection.anchor).toBeNull();
  });
});
