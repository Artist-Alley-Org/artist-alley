// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1020 — the browse bar reveals when the pointer approaches the bottom
// of the window.
//
// The interesting assertion in here is the one that looks redundant:
// `chromeScroll.hidden` must STILL be true after the reveal. That field
// is shared with the header (+layout.svelte derives the navbar's
// `chromeHidden` from it), so the obvious implementation — calling
// `chromeScroll.reveal()` — would slide the TOP navbar back down
// whenever the pointer neared the BOTTOM edge. The reveal has to be a
// local term composed into the footer's own `hidden`, and the only way
// a test can tell the two implementations apart is by asserting on the
// store the navbar reads.

import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import ViewControls from './ViewControls.svelte';
import { chromeScroll } from '$stores/chromeScroll.svelte';

/** The class the component translates the bar off-screen with. */
const HIDDEN = 'chrome-hidden-bottom';

let realMatchMedia: typeof window.matchMedia;

/** Point matchMedia at one modality. `(pointer: fine)` is the gate the
 *  component reads; everything else keeps happy-dom's non-matching
 *  default (notably `prefers-reduced-motion`, which the STORE reads). */
function usePointer(kind: 'fine' | 'coarse') {
  window.matchMedia = ((q: string) => ({
    matches: q === `(pointer: ${kind})`,
    media: q,
    onchange: null,
    addListener() {},
    removeListener() {},
    addEventListener() {},
    removeEventListener() {},
    dispatchEvent: () => false,
  })) as unknown as typeof window.matchMedia;
}

/** happy-dom ships PointerEvent, but fall back to MouseEvent so this
 *  doesn't become a runtime-shape test. */
function pointerMove(clientY: number, pointerType = 'mouse') {
  const Ctor = (globalThis as { PointerEvent?: typeof PointerEvent }).PointerEvent;
  const ev = Ctor
    ? new Ctor('pointermove', { clientY, pointerType, bubbles: true })
    : Object.assign(new MouseEvent('pointermove', { clientY, bubbles: true }), { pointerType });
  window.dispatchEvent(ev);
}

/** happy-dom has no `:focus-visible`, and the component's whole point is
 *  that it distinguishes keyboard focus from a mouse click — so the
 *  selector is answered for the duration of one dispatch. The REAL
 *  `:focus-visible` behaviour is covered by driving Chromium; this pins
 *  which branch the component takes for each answer. */
function withFocusVisible(visible: boolean, fn: () => void) {
  const real = Element.prototype.matches;
  Element.prototype.matches = function (sel: string) {
    if (sel === ':focus-visible') return visible;
    return real.call(this, sel);
  };
  try {
    fn();
  } finally {
    Element.prototype.matches = real;
  }
}

function bar(): HTMLElement {
  const el = document.querySelector('[data-testid="view-controls"]');
  if (!el) throw new Error('view-controls did not render');
  return el as HTMLElement;
}

/** Anywhere inside the reveal zone. The zone is derived from the bar's
 *  measured height, which happy-dom reports as 0, so it falls back to
 *  the 44px floor + 16px offset + 32px margin = 92px. */
const NEAR_BOTTOM = () => window.innerHeight - 4;
const MID_SCREEN = () => Math.round(window.innerHeight / 2);

describe('ViewControls — bottom-edge reveal (#1020)', () => {
  beforeEach(() => {
    realMatchMedia = window.matchMedia;
    chromeScroll.hidden = false;
  });
  afterEach(() => {
    window.matchMedia = realMatchMedia;
    chromeScroll.hidden = false;
  });

  it('reveals on pointer approach WITHOUT touching the shared store', async () => {
    usePointer('fine');
    render(ViewControls);
    await tick();

    // Scrolled down: the store hides both the navbar and this bar.
    chromeScroll.hidden = true;
    await tick();
    expect(bar().classList.contains(HIDDEN)).toBe(true);

    // Pointer approaches the bottom edge — the bar comes back...
    pointerMove(NEAR_BOTTOM());
    await tick();
    expect(bar().classList.contains(HIDDEN)).toBe(false);

    // ...and the navbar does NOT. This is the assertion that rules out
    // chromeScroll.reveal(): +layout.svelte derives the header's
    // `chromeHidden` from exactly this field.
    expect(chromeScroll.hidden).toBe(true);
  });

  it('re-hides when the pointer leaves the zone, handing control back', async () => {
    usePointer('fine');
    render(ViewControls);
    await tick();

    chromeScroll.hidden = true;
    pointerMove(NEAR_BOTTOM());
    await tick();
    expect(bar().classList.contains(HIDDEN)).toBe(false);

    pointerMove(MID_SCREEN());
    await tick();
    expect(bar().classList.contains(HIDDEN)).toBe(true);
    expect(chromeScroll.hidden).toBe(true);
  });

  it('does not reveal while the store says the bar belongs on screen', async () => {
    // The reveal is one term of an AND, not an override: with the store
    // showing the bar, moving the pointer around must not toggle
    // anything.
    usePointer('fine');
    render(ViewControls);
    await tick();

    pointerMove(NEAR_BOTTOM());
    await tick();
    expect(bar().classList.contains(HIDDEN)).toBe(false);
    expect(chromeScroll.hidden).toBe(false);
  });

  it('gives COARSE pointers nothing at all', async () => {
    // A touch device has no hover to trigger this with, and a reveal
    // strip across the bottom of a phone sits exactly where thumbs
    // rest. Modality, not pixels — same gate as browseView's coarse
    // default. Note the events below are still delivered: the listener
    // is simply never installed.
    usePointer('coarse');
    render(ViewControls);
    await tick();

    chromeScroll.hidden = true;
    await tick();
    expect(bar().classList.contains(HIDDEN)).toBe(true);

    pointerMove(NEAR_BOTTOM());
    await tick();
    expect(bar().classList.contains(HIDDEN)).toBe(true);
    expect(chromeScroll.hidden).toBe(true);
  });

  it('ignores TOUCH pointermove on a device with a fine primary pointer', async () => {
    // A touchscreen laptop reports `(pointer: fine)` for its mouse and
    // still delivers touch pointermove while a finger drags the feed.
    usePointer('fine');
    render(ViewControls);
    await tick();

    chromeScroll.hidden = true;
    await tick();

    pointerMove(NEAR_BOTTOM(), 'touch');
    await tick();
    expect(bar().classList.contains(HIDDEN)).toBe(true);
  });

  it('reveals on KEYBOARD focus, and hides when focus leaves', async () => {
    // Keyboard parity: tabbing to a control that stays off-screen is
    // reaching for something you cannot see.
    usePointer('fine');
    render(ViewControls);
    await tick();

    chromeScroll.hidden = true;
    await tick();
    expect(bar().classList.contains(HIDDEN)).toBe(true);

    const button = bar().querySelector('button');
    if (!button) throw new Error('no control to focus');
    withFocusVisible(true, () =>
      button.dispatchEvent(new FocusEvent('focusin', { bubbles: true })));
    await tick();
    expect(bar().classList.contains(HIDDEN)).toBe(false);
    expect(chromeScroll.hidden).toBe(true);

    button.dispatchEvent(new FocusEvent('focusout', { bubbles: true, relatedTarget: null }));
    await tick();
    expect(bar().classList.contains(HIDDEN)).toBe(true);
  });

  it('does NOT pin the bar when a mouse click focuses a control', async () => {
    // focus-within would: clicking the sort toggle leaves :focus on the
    // button, and the bar would then refuse to hide on the next
    // scroll-down until the user clicked somewhere else. `:focus-visible`
    // is the "arrived by keyboard" distinction that avoids it.
    usePointer('fine');
    render(ViewControls);
    await tick();

    chromeScroll.hidden = true;
    await tick();

    const button = bar().querySelector('button');
    if (!button) throw new Error('no control to focus');
    withFocusVisible(false, () =>
      button.dispatchEvent(new FocusEvent('focusin', { bubbles: true })));
    await tick();
    expect(bar().classList.contains(HIDDEN)).toBe(true);
  });

  it('Escape dismisses the reveal without moving the pointer (WCAG 1.4.13)', async () => {
    usePointer('fine');
    render(ViewControls);
    await tick();

    chromeScroll.hidden = true;
    pointerMove(NEAR_BOTTOM());
    await tick();
    expect(bar().classList.contains(HIDDEN)).toBe(false);

    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    await tick();
    expect(bar().classList.contains(HIDDEN)).toBe(true);

    // Leaving and re-approaching reveals again — dismissal is for this
    // approach, not a permanent opt-out.
    pointerMove(MID_SCREEN());
    await tick();
    pointerMove(NEAR_BOTTOM());
    await tick();
    expect(bar().classList.contains(HIDDEN)).toBe(false);
  });
});

// ── Light dismiss for the view switcher (#1096) ──────────────────────
//
// The panel used to close only by re-pressing the toggle that opened it,
// so it followed the user around the page. It now closes on a press
// anywhere outside — WITHOUT eating that press, which is the part worth
// a test: dismissal that costs a throwaway click is the thing users
// complain about in the menus that do it.
describe('ViewControls — light dismiss (#1096)', () => {
  beforeEach(() => {
    realMatchMedia = window.matchMedia;
    chromeScroll.hidden = false;
  });
  afterEach(() => {
    window.matchMedia = realMatchMedia;
    chromeScroll.hidden = false;
  });

  function pointerDownOn(target: EventTarget): Event {
    const Ctor = (globalThis as { PointerEvent?: typeof PointerEvent }).PointerEvent;
    const ev = Ctor
      ? new Ctor('pointerdown', { bubbles: true, cancelable: true })
      : new MouseEvent('pointerdown', { bubbles: true, cancelable: true });
    target.dispatchEvent(ev);
    return ev;
  }

  /** The switcher toggle — found by the state it publishes rather than
   *  by copy, so this survives an i18n or icon change. */
  function toggle(): HTMLButtonElement {
    const el = bar().querySelector('button[aria-expanded]');
    if (!el) throw new Error('no switcher toggle');
    return el as HTMLButtonElement;
  }
  const isOpen = () => toggle().getAttribute('aria-expanded') === 'true';

  async function open() {
    usePointer('fine');
    render(ViewControls);
    await tick();
    toggle().click();
    await tick();
    expect(isOpen()).toBe(true);
  }

  it('closes on a press outside, and lets that press through', async () => {
    await open();

    // Something else on the page — a tile in the feed.
    const tile = document.createElement('button');
    document.body.appendChild(tile);
    const ev = pointerDownOn(tile);
    await tick();

    expect(isOpen()).toBe(false);
    // THE POINT: the press that dismissed the panel is still a press on
    // the tile. Nothing was prevented and nothing was stopped, so the
    // click that follows it opens the tile.
    expect(ev.defaultPrevented).toBe(false);
    expect(ev.cancelBubble).toBe(false);
    tile.remove();
  });

  it('stays open for a press on its own controls', async () => {
    await open();
    const views = [...bar().querySelectorAll('button')].filter((b) => b !== toggle());
    expect(views.length).toBeGreaterThan(0);
    pointerDownOn(views[0]);
    await tick();
    expect(isOpen()).toBe(true);
  });

  // The regression this shape invites: dismissing on the toggle's OWN
  // pointerdown closes the panel a beat before its click reopens it, and
  // the toggle stops working.
  it('leaves the toggle toggling', async () => {
    await open();
    pointerDownOn(toggle());
    toggle().click();
    await tick();
    expect(isOpen()).toBe(false);
    toggle().click();
    await tick();
    expect(isOpen()).toBe(true);
  });

  it('closes on Escape and hands focus back to the toggle', async () => {
    await open();
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    await tick();
    expect(isOpen()).toBe(false);
    expect(document.activeElement).toBe(toggle());
  });

  // The listener is only installed while the panel is open — the shape
  // BrowseFooter's Escape handler already uses. A window listener that
  // outlives the thing it serves is how a component starts reacting to
  // presses on pages it is no longer part of.
  it('holds a window listener only while it is open', async () => {
    usePointer('fine');
    const added: string[] = [];
    const removed: string[] = [];
    const realAdd = window.addEventListener;
    const realRemove = window.removeEventListener;
    /* eslint-disable @typescript-eslint/no-explicit-any */
    window.addEventListener = ((type: string, ...rest: any[]) => {
      if (type === 'pointerdown') added.push(type);
      return (realAdd as any).call(window, type, ...rest);
    }) as typeof window.addEventListener;
    window.removeEventListener = ((type: string, ...rest: any[]) => {
      if (type === 'pointerdown') removed.push(type);
      return (realRemove as any).call(window, type, ...rest);
    }) as typeof window.removeEventListener;
    /* eslint-enable @typescript-eslint/no-explicit-any */
    try {
      render(ViewControls);
      await tick();
      expect(added.length).toBe(0);
      toggle().click();
      await tick();
      expect(added.length).toBe(1);
      expect(removed.length).toBe(0);
      toggle().click();
      await tick();
      expect(removed.length).toBe(1);
    } finally {
      window.addEventListener = realAdd;
      window.removeEventListener = realRemove;
    }
  });
});
