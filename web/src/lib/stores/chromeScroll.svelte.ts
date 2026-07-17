// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Scroll-direction state for the auto-hiding chrome (header + browse
// footer). Both hide on the way down and come back instantly on the way
// up, so an image-heavy page gets the whole viewport while you're
// reading it.
//
// This is an ultrawide feature as much as a phone one: a 3840×1080 32:9
// panel has less vertical room than a tablet, and it's the axis the
// chrome eats.
//
// # Why JS
//
// `scroll-state(scrolled:)` is the right primitive — it's the only one
// that knows scroll *direction* — but it's Chrome/Edge 144+ with no
// Safari and no Firefox, i.e. inert on every iPhone, which is the exact
// device this feature is for. `animation-timeline` isn't Baseline
// either and flashes when you reverse direction. So: JS, one listener,
// and only compositable properties out the other side.
//
// # One listener, not two
//
// The browse footer already listened to `main`'s scrollTop for its
// back-to-top button. Rather than add a second listener, that state
// moved here and both consumers read from it. A `main` scroll fires
// this handler once, not once per component.

import { browser } from '$app/environment';

/** Past this much scroll, back-to-top is worth offering. Unchanged
 *  from the value the footer used when it owned this listener. */
const SCROLLED_THRESHOLD = 200;

/** Ignore direction flips smaller than this. Without it, the elastic
 *  overscroll at the top/bottom of iOS Safari and every stray trackpad
 *  jitter toggles the chrome. Small enough to still feel immediate. */
const DIRECTION_EPSILON = 6;

/** Don't hide the chrome until the user is past the header's own
 *  height — hiding it while still near the top just flickers. */
const HIDE_AFTER = 96;

class ChromeScrollState {
  /** True once scrolled far enough for back-to-top to make sense. */
  scrolled = $state(false);
  /** True when the chrome should be off-screen (scrolling down, past
   *  HIDE_AFTER). Consumers translate this into a transform. */
  hidden = $state(false);

  #lastY = 0;
  #consumers = 0;
  #el: HTMLElement | null = null;
  #onScroll: (() => void) | null = null;

  /** Attach to the app-shell's <main>, which is the scroll context for
   *  normal pages. Ref-counted: the header and the footer both call
   *  this, the listener is installed once, and it's removed when the
   *  last consumer detaches.
   *
   *  Returns a teardown for use as an $effect return. */
  attach(): () => void {
    if (!browser) return () => {};
    this.#consumers += 1;
    if (this.#consumers === 1) this.#install();
    return () => {
      this.#consumers -= 1;
      if (this.#consumers === 0) this.#uninstall();
    };
  }

  #install(): void {
    const main = document.querySelector('main');
    if (!main) return;
    this.#el = main;
    this.#lastY = main.scrollTop;

    // Reduced motion: the chrome never hides, so there's no movement to
    // transition. Still track `scrolled` — back-to-top isn't motion.
    const reduced = window.matchMedia?.('(prefers-reduced-motion: reduce)').matches ?? false;

    this.#onScroll = () => {
      const y = this.#el?.scrollTop ?? 0;
      this.scrolled = y > SCROLLED_THRESHOLD;

      const dy = y - this.#lastY;
      if (Math.abs(dy) < DIRECTION_EPSILON) return;
      this.#lastY = y;

      if (reduced) {
        this.hidden = false;
        return;
      }
      // Reveal tracks the thumb: any upward movement past the epsilon
      // shows the chrome on the very next frame. No debounce, no
      // timeout — a delay here is what makes this pattern feel broken.
      this.hidden = dy > 0 && y > HIDE_AFTER;
    };

    this.#onScroll();
    main.addEventListener('scroll', this.#onScroll, { passive: true });
  }

  #uninstall(): void {
    if (this.#el && this.#onScroll) {
      this.#el.removeEventListener('scroll', this.#onScroll);
    }
    this.#el = null;
    this.#onScroll = null;
    this.hidden = false;
  }
}

export const chromeScroll = new ChromeScrollState();
