// totalItemCount is the cap-check the whiteboard uses to gate
// inserts past ITEM_SOFT_CAP / ITEM_HARD_CAP. A miscount silently
// disables the cap or trips it at the wrong size.

import { describe, expect, it } from 'vitest';
import { totalItemCount } from './session.svelte';
import type { BrushContent, Layer, Item } from './types';

function makeLayer(id: string, items: Item[]): Layer {
  return { id, name: id, visible: true, opacity: 1, items };
}

function makeDoc(layers: Layer[]): BrushContent {
  return { source_w: 1024, source_h: 768, layers };
}

function stub(id: string): Item {
  // Items are a tagged union — the count function only walks
  // l.items.length so any object stands in.
  return { type: 'stroke', id, points: [] } as unknown as Item;
}

describe('totalItemCount', () => {
  it('returns 0 for an empty doc', () => {
    expect(totalItemCount(makeDoc([]))).toBe(0);
    expect(totalItemCount(makeDoc([makeLayer('a', [])]))).toBe(0);
  });

  it('sums items across every layer', () => {
    const doc = makeDoc([
      makeLayer('a', [stub('1'), stub('2'), stub('3')]),
      makeLayer('b', [stub('4'), stub('5')]),
      makeLayer('c', [stub('6')]),
    ]);
    expect(totalItemCount(doc)).toBe(6);
  });

  it('counts items in hidden + locked layers — the cap is per-doc, not per-visible', () => {
    const doc = makeDoc([
      { id: 'shown', visible: true, opacity: 1, items: [stub('a'), stub('b')] },
      { id: 'hidden', visible: false, opacity: 1, items: [stub('c')] },
      { id: 'locked', visible: true, opacity: 1, locked: true, items: [stub('d'), stub('e')] },
    ]);
    expect(totalItemCount(doc)).toBe(5);
  });
});
