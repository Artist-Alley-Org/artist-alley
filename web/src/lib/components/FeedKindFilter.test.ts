// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1166 — the browse footer's asset-type filter.
//
// The anatomy this pins is ratified rather than invented (see the
// component's own header), and each rule below is one that INVERTS
// silently if it breaks — a filter that commits the wrong set looks
// exactly like a filter that works, until you count the tiles.
//
//   ALL-CHECKED = NO FILTER. The rule the whole control rests on. If
//   Apply ever emitted twelve kind names instead of an empty list, the
//   URL would grow a `?kind=` on a wall nobody filtered, the button
//   would light up, and the server would receive a filter that happens
//   to select everything — right answer, wrong contract, and the first
//   kind added to the vocabulary breaks it for real.
//
//   NONE-CHECKED ALSO MEANS NO FILTER. Nothing ticked is a half-made
//   selection, not a request for an empty wall.
//
//   THE DRAFT IS NOT THE SELECTION. Ticking boxes must not refetch;
//   only Apply commits, and dismissing throws the draft away.
//
//   THE LIST COMES FROM kindIcon. Asserted against FILTERABLE_KINDS
//   rather than a copy, because a second list here would be the exact
//   drift the component exists to avoid.

import { describe, it, expect, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import FeedKindFilter from './FeedKindFilter.svelte';
import { FILTERABLE_KINDS } from './kindIcon';

function toggle(): HTMLButtonElement {
  const el = document.querySelector('[data-testid="kind-filter-toggle"]');
  if (!el) throw new Error('the filter toggle did not render');
  return el as HTMLButtonElement;
}

function panel(): HTMLElement | null {
  return document.querySelector('[data-testid="kind-filter-panel"]');
}

function options(): HTMLInputElement[] {
  return Array.from(document.querySelectorAll('[data-testid="kind-filter-option"]'));
}

function optionFor(kind: string): HTMLInputElement {
  const el = document.querySelector(`[data-testid="kind-filter-option"][data-kind="${kind}"]`);
  if (!el) throw new Error(`no checkbox for kind ${kind}`);
  return el as HTMLInputElement;
}

function allBox(): HTMLInputElement {
  return document.querySelector('[data-testid="kind-filter-all"]') as HTMLInputElement;
}

function applyBtn(): HTMLButtonElement {
  return document.querySelector('[data-testid="kind-filter-apply"]') as HTMLButtonElement;
}

/** A real click, so the window-level capture listeners see it too. */
function click(el: Element) {
  el.dispatchEvent(new MouseEvent('click', { bubbles: true }));
}

/** A checkbox flip the way a browser produces one: the input's own
 *  `checked` moves first, then `change` fires. */
function check(el: HTMLInputElement, next: boolean) {
  el.checked = next;
  el.dispatchEvent(new Event('change', { bubbles: true }));
}

/** A real double click, in full: a browser delivers the two ordinary
 *  clicks (which the <label> forwards to its checkbox, flipping it
 *  twice) and THEN `dblclick`. Dispatching only the `dblclick` would
 *  test a gesture no browser produces, and would hide the one way this
 *  could go wrong — solo being applied and then undone by a trailing
 *  toggle. */
function dblclick(label: Element, box: HTMLInputElement) {
  check(box, !box.checked);
  check(box, !box.checked);
  label.dispatchEvent(new MouseEvent('dblclick', { bubbles: true }));
}

function labelOf(box: HTMLInputElement): HTMLElement {
  const el = box.closest('label');
  if (!el) throw new Error('the checkbox is not inside a label');
  return el;
}

async function open(selected: string[] = []) {
  const onapply = vi.fn();
  render(FeedKindFilter, { props: { selected, onapply } });
  click(toggle());
  await tick();
  return onapply;
}

describe('FeedKindFilter — the panel', () => {
  it('offers one checkbox per filterable kind, from kindIcon', async () => {
    await open();
    expect(options().map((o) => o.dataset.kind)).toEqual([...FILTERABLE_KINDS]);
    // The two the filter deliberately does not offer: a kind no single
    // asset resolves to, and the resolver's "I could not tell" answer.
    expect(options().map((o) => o.dataset.kind)).not.toContain('sequence');
    expect(options().map((o) => o.dataset.kind)).not.toContain('placeholder');
  });

  it('opens with everything ticked when nothing is applied', async () => {
    await open([]);
    expect(allBox().checked).toBe(true);
    expect(options().every((o) => o.checked)).toBe(true);
  });

  it('opens with the applied subset ticked and nothing else', async () => {
    await open(['image', 'video']);
    expect(allBox().checked).toBe(false);
    const ticked = options()
      .filter((o) => o.checked)
      .map((o) => o.dataset.kind);
    expect(ticked).toEqual(['image', 'video']);
  });
});

// The owner: "If all types is selected, and I double click PDF, it
// should deselect all but pdf."
//
// The trap this pins is ORDER. A double click delivers its two ordinary
// clicks first and `dblclick` last, so a solo written as a mutation of
// the draft — or applied before the toggles land — ends with the soloed
// type toggled back off and everything else still ticked, which looks
// like the gesture did nothing at all.
describe('FeedKindFilter — double-click solos', () => {
  it('⭐ leaves ONLY the double-clicked type ticked, from all-checked', async () => {
    await open([]);
    expect(options().every((o) => o.checked)).toBe(true);
    const pdf = optionFor('pdf');
    dblclick(labelOf(pdf), pdf);
    await tick();
    expect(
      options()
        .filter((o) => o.checked)
        .map((o) => o.dataset.kind),
    ).toEqual(['pdf']);
    expect(allBox().checked).toBe(false);
  });

  it('solos from a SUBSET too, replacing it rather than adding to it', async () => {
    await open(['image', 'video']);
    const audio = optionFor('audio');
    dblclick(labelOf(audio), audio);
    await tick();
    expect(
      options()
        .filter((o) => o.checked)
        .map((o) => o.dataset.kind),
    ).toEqual(['audio']);
  });

  it('is idempotent — soloing what is already soloed keeps it', async () => {
    await open(['pdf']);
    const pdf = optionFor('pdf');
    dblclick(labelOf(pdf), pdf);
    await tick();
    expect(
      options()
        .filter((o) => o.checked)
        .map((o) => o.dataset.kind),
    ).toEqual(['pdf']);
  });

  it('does not commit — Apply is still what the feed hears', async () => {
    const onapply = await open([]);
    const pdf = optionFor('pdf');
    dblclick(labelOf(pdf), pdf);
    await tick();
    expect(onapply).not.toHaveBeenCalled();
    click(applyBtn());
    expect(onapply).toHaveBeenCalledWith(['pdf']);
  });

  it('leaves the SINGLE click alone — it still plain-toggles', async () => {
    await open([]);
    const pdf = optionFor('pdf');
    check(pdf, false);
    await tick();
    const ticked = options()
      .filter((o) => o.checked)
      .map((o) => o.dataset.kind);
    expect(ticked).not.toContain('pdf');
    expect(ticked.length).toBe(FILTERABLE_KINDS.length - 1);
  });

  it('double-clicking "All types" lands on every type, from any state', async () => {
    // Its two ordinary clicks cancel out only when it started checked;
    // from a subset they would clear the board, which is the opposite of
    // what the row says it does.
    await open(['pdf']);
    const all = allBox();
    dblclick(labelOf(all), all);
    await tick();
    expect(options().every((o) => o.checked)).toBe(true);
    expect(allBox().checked).toBe(true);
  });
});

describe('FeedKindFilter — what Apply commits', () => {
  it('commits an EMPTY list when every box is ticked', async () => {
    const onapply = await open(['image']);
    check(allBox(), true);
    await tick();
    click(applyBtn());
    // ⭐ Not the twelve names. All-checked is the absence of a filter,
    // and the caller drops the query parameter on an empty list.
    expect(onapply).toHaveBeenCalledWith([]);
  });

  it('commits an EMPTY list when nothing is ticked', async () => {
    const onapply = await open([]);
    check(allBox(), false); // clears the board
    await tick();
    click(applyBtn());
    expect(onapply).toHaveBeenCalledWith([]);
  });

  it('commits the subset, in the vocabulary order', async () => {
    const onapply = await open([]);
    check(allBox(), false);
    await tick();
    check(optionFor('video'), true);
    await tick();
    check(optionFor('image'), true);
    await tick();
    click(applyBtn());
    // Ticked video first; the emitted order follows FILTERABLE_KINDS so
    // the URL is stable regardless of click order.
    expect(onapply).toHaveBeenCalledWith(['image', 'video']);
  });

  it('does not commit while boxes are being ticked', async () => {
    const onapply = await open([]);
    check(allBox(), false);
    await tick();
    check(optionFor('image'), true);
    await tick();
    expect(onapply).not.toHaveBeenCalled();
  });

  it('throws the draft away when dismissed without applying', async () => {
    const onapply = await open(['image']);
    check(optionFor('video'), true);
    await tick();
    // Light dismiss: a click outside the panel.
    click(document.body);
    await tick();
    expect(onapply).not.toHaveBeenCalled();
    expect(panel()).toBeNull();

    // Re-opening shows the APPLIED selection again, not the abandoned draft.
    click(toggle());
    await tick();
    expect(optionFor('video').checked).toBe(false);
    expect(optionFor('image').checked).toBe(true);
  });
});

describe('FeedKindFilter — the button states what is applied', () => {
  it('is inert with no filter and active with a real subset', async () => {
    const { unmount } = render(FeedKindFilter, { props: { selected: [], onapply: vi.fn() } });
    expect(toggle().dataset.active).toBeUndefined();
    unmount();

    render(FeedKindFilter, { props: { selected: ['image'], onapply: vi.fn() } });
    expect(toggle().dataset.active).toBe('true');
    expect(toggle().textContent).toContain('1');
  });

  it('is inert when the "subset" is every kind — that is not a filter', async () => {
    render(FeedKindFilter, {
      props: { selected: [...FILTERABLE_KINDS], onapply: vi.fn() },
    });
    expect(toggle().dataset.active).toBeUndefined();
  });
});

describe('FeedKindFilter — dismissal follows the ViewControls convention', () => {
  it('closes on Escape', async () => {
    await open();
    expect(panel()).not.toBeNull();
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    await tick();
    expect(panel()).toBeNull();
  });

  it('does NOT close on a click inside the panel', async () => {
    await open();
    click(optionFor('image'));
    await tick();
    expect(panel()).not.toBeNull();
  });

  it('does not treat a drag that STARTED inside as an outside click', async () => {
    await open();
    // pointerdown inside, click delivered with body as the common
    // ancestor — the #1105 case `pressedInside` exists for.
    optionFor('image').dispatchEvent(new MouseEvent('pointerdown', { bubbles: true }));
    click(document.body);
    await tick();
    expect(panel()).not.toBeNull();
  });

  it('listens on click, never on pointerdown, for the dismissal itself', async () => {
    // pointerdown outside must not dismiss on its own; only the click
    // that follows does. Dismissing on pointerdown reflows the bar
    // between down and up and eats the press the user aimed at (#1105).
    await open();
    document.body.dispatchEvent(new MouseEvent('pointerdown', { bubbles: true }));
    await tick();
    expect(panel()).not.toBeNull();
    click(document.body);
    await tick();
    expect(panel()).toBeNull();
  });
});
