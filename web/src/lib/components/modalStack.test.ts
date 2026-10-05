// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// THE BACKGROUND SCROLL LOCK RELEASES ON THE LAST CLOSE (#1223).
//
// The lock is driven by the modal stack rather than by a boolean,
// because "a modal closed" and "no modal is open" are different
// questions and only the second one may release it. The case that
// separates them is two dialogs on screen at once, and the case that
// separates a stack from a counter is closing them OUT OF ORDER — which
// popModal has always had to handle (a parent can be dismissed
// programmatically while its child is still up), and which a naive
// `pop()` would get wrong by removing the other token.
//
// Asserted here rather than in the Playwright suite for a reason worth
// stating: no product surface currently raises two shared Modals at
// once. #1207's cover editor did and #1213 folded it into a second PAGE
// of one dialog; PostHost raises ShareEntityModal from inside the
// viewer's own native <dialog>, which is one shared Modal, not two. So a
// browser assertion would have to fabricate the stack anyway, and this
// asserts the rule where the rule lives. The wheel, touch and
// scroll-position behaviour is driven for real in
// scripts/dogfood/ui/tests/standalone/modal-scroll-lock-1223.spec.ts.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { modalStack, pushModal, popModal } from './modalStack';

const LOCK = 'aa-modal-open';
const GUTTER = 'aa-modal-gutter';

/** The app shell's scroller. The lock measures it and the CSS rule
 *  targets it, so its absence is a real case (SSR, a bare test DOM). */
function mountMain(gutter = 0): HTMLElement {
  const main = document.createElement('main');
  document.body.appendChild(main);
  // happy-dom reports 0 for both, which is the overlay-scrollbar answer.
  // A classic scrollbar is the other branch and has to be posed.
  Object.defineProperty(main, 'offsetWidth', { value: 1000, configurable: true });
  Object.defineProperty(main, 'clientWidth', { value: 1000 - gutter, configurable: true });
  return main;
}

describe('modalStack — the background scroll lock', () => {
  beforeEach(() => {
    // The stack is a module-level singleton by design, so it survives
    // between tests in this file. Empty it by identity, the way the
    // component does.
    for (const token of [...modalStack]) popModal(token);
    document.documentElement.classList.remove(LOCK, GUTTER);
    document.body.innerHTML = '';
  });

  afterEach(() => {
    document.documentElement.classList.remove(LOCK, GUTTER);
  });

  it('locks on the FIRST open and releases on the LAST close', () => {
    mountMain();
    const a = {};
    const b = {};

    pushModal(a);
    expect(document.documentElement.classList.contains(LOCK)).toBe(true);

    pushModal(b);
    expect(
      document.documentElement.classList.contains(LOCK),
      'the second open must not double-lock into something a single close releases',
    ).toBe(true);

    // ⛔ CLOSE THE FIRST ONE, not the last. A parent dismissed while its
    // child is still up is the case popModal pops by identity for, and
    // the lock has to survive it — the page behind is still covered.
    popModal(a);
    expect(
      document.documentElement.classList.contains(LOCK),
      'closing the underlying dialog released the lock while one was still open',
    ).toBe(true);
    expect(modalStack).toEqual([b]);

    popModal(b);
    expect(document.documentElement.classList.contains(LOCK)).toBe(false);
    expect(modalStack).toHaveLength(0);
  });

  it('leaks no lock across repeated open/close cycles', () => {
    mountMain();
    const token = {};
    for (let i = 0; i < 3; i += 1) {
      pushModal(token);
      expect(document.documentElement.classList.contains(LOCK)).toBe(true);
      popModal(token);
      expect(document.documentElement.classList.contains(LOCK)).toBe(false);
    }
  });

  it('ignores a pop for a token that is not on the stack', () => {
    // Modal.svelte pops from its close branch AND from onDestroy, so
    // this happens on every dismissal. A pop that released regardless
    // would unlock the page under a dialog that is still open.
    mountMain();
    const open = {};
    const gone = {};
    pushModal(open);
    popModal(gone);
    expect(document.documentElement.classList.contains(LOCK)).toBe(true);
    expect(modalStack).toEqual([open]);
  });

  it('does not push the same token twice', () => {
    mountMain();
    const token = {};
    pushModal(token);
    pushModal(token);
    expect(modalStack).toHaveLength(1);
    // And the single close still releases — a phantom second entry would
    // leave the page locked forever.
    popModal(token);
    expect(document.documentElement.classList.contains(LOCK)).toBe(false);
  });

  it('reserves the gutter only when the scroller was drawing a scrollbar', () => {
    // Overlay scrollbars: nothing to reserve, and reserving would move
    // the page sideways.
    mountMain(0);
    const overlay = {};
    pushModal(overlay);
    expect(document.documentElement.classList.contains(GUTTER)).toBe(false);
    popModal(overlay);

    document.body.innerHTML = '';
    mountMain(15);
    const classic = {};
    pushModal(classic);
    expect(
      document.documentElement.classList.contains(GUTTER),
      'a classic scrollbar was removed without reserving its width — everything ' +
        'behind the dialog shifts 15px',
    ).toBe(true);
    popModal(classic);
    expect(document.documentElement.classList.contains(GUTTER)).toBe(false);
  });

  it('is inert when there is no app scroller to lock', () => {
    // No <main>: SSR, or a route that has not mounted the shell yet.
    // Nothing to measure, nothing to lock, and no throw.
    const token = {};
    expect(() => pushModal(token)).not.toThrow();
    expect(document.documentElement.classList.contains(LOCK)).toBe(false);
    popModal(token);
  });
});
