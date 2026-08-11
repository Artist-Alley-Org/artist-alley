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
