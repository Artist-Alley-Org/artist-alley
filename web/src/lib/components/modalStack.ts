// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/** Which modal is on top (#1207), and whether ANY is open (#1223).
 *
 *  Two questions, one array, and they are asked at different depths:
 *  Escape needs the token on the END, the background scroll lock needs
 *  the array to be EMPTY or not. Both are answered here rather than by a
 *  second counter beside it, because a counter and a stack that
 *  disagreed would be a lock nobody could release — and closes are not
 *  reliably in reverse order, which is the whole reason this pops by
 *  identity. See lockBackgroundScroll at the foot of the file.
 *
 *  [Modal.svelte] listens for Escape on the DOCUMENT, so every open
 *  instance hears every press. That was invisible while only one modal
 *  was ever on screen; #1207's cover editor opens from inside the
 *  collection edit modal, and two instances both closing on one Escape
 *  loses the curator's unsaved form to a keystroke that should have
 *  stepped back exactly one level.
 *
 *  A plain array of opaque tokens in open order. Not a Svelte store and
 *  not reactive: nothing RENDERS from it — it is read inside an event
 *  handler at the moment of the keypress, which is the only moment the
 *  answer matters, and making it reactive would invite a component to
 *  paint from it and turn stacking into a layout input.
 *
 *  A module-level singleton is correct here rather than context: modals
 *  portal out of their declared position, so the "stack" they belong to
 *  is the document, not any component subtree.
 */
export const modalStack: object[] = [];

/** Put a modal on top. Idempotent — an already-stacked token is not
 *  duplicated, so a re-run effect cannot push the same modal twice and
 *  leave a phantom entry that outlives it. */
export function pushModal(token: object): void {
  if (modalStack.includes(token)) return;
  modalStack.push(token);
  // FIRST modal on, not "a modal opened" — see lockBackgroundScroll.
  if (modalStack.length === 1) lockBackgroundScroll();
}

/** Remove a modal from the stack, wherever it sits.
 *
 *  By identity rather than by popping the end: modals do not reliably
 *  close in reverse order — a parent can be closed programmatically
 *  while a child is still open (navigating away, a save that dismisses
 *  everything) — and popping blindly would then remove the WRONG token
 *  and leave the closed modal on the stack forever, permanently
 *  swallowing Escape for everything after it.
 */
export function popModal(token: object): void {
  const at = modalStack.indexOf(token);
  if (at < 0) return;
  modalStack.splice(at, 1);
  // LAST modal off. Modal.svelte calls popModal from both its close
  // branch and onDestroy, so this runs for tokens that are not on the
  // stack too; the guard above is what keeps those from releasing a
  // lock somebody else still holds.
  if (modalStack.length === 0) releaseBackgroundScroll();
}

// ── THE BACKGROUND SCROLL LOCK (#1223) ───────────────────────────────
//
// ⚠️ THE ISSUE SAYS "the document scroller" AND THIS APP HAS NONE. The
// shell is `<div class="h-dvh … overflow-hidden">` with `<main
// class="flex-1 overflow-y-auto">` inside it (+layout.svelte), so
// `document.scrollingElement.scrollTop` is 0 on every route and always
// has been. What scrolls behind an open dialog is <main>, which is also
// what scrollSnapshot.ts and chromeScroll.svelte.ts already reach for by
// the same selector.
//
// ⚠️ AND Modal.svelte IS NOT A NATIVE `<dialog>` — it is a portalled
// `<div role="dialog">`, so there is no native inertness here to lean
// on and never was. The overlay is `position: fixed` and parented to
// <body>, which is why the wheel does NOT always reach <main>: Chrome
// resolves the scroll chain for a fixed element against the viewport,
// and only reaches <main> when <main> has been promoted to the
// document's effective root scroller — which happens exactly when the
// auto-hiding chrome is up and <main> fills the viewport. Measured, and
// it is what makes this look like an intermittent bug:
//
//   390x780   panel 690 tall — wheel over the backdrop moved <main>
//             400 -> 3400 with the dialog still open
//   1280x600  panel 567 tall — same, 400 -> 2831
//   1920x1080 panel 586 tall — no movement
//
// `overflow: hidden` on the scroller is the whole mechanism. The box
// stays a scroll container, so `scrollTop` survives the lock and the
// release (measured: 400 / 400 / 400), and the inner scrollers — the
// dialog's own body, the cover picker's grid — are untouched, including
// under touch, which cannot scroll an `overflow: hidden` box either.
//
// ⛔ IT IS APPLIED AS A CLASS ON <html>, NOT AS AN INLINE STYLE ON
// <main>, and that is not a preference — an inline style DOES NOT
// SURVIVE THERE. <main> carries `style={showChrome ? margin-top:… }`
// (+layout.svelte), which Svelte writes as the whole `style` ATTRIBUTE,
// so every time the auto-hiding chrome moves it rewrites the attribute
// and takes any foreign inline property with it. The first version of
// this lock did exactly that and was measured working when the test
// applied it and failing when the component did: scrolling <main> hides
// the chrome, the rewrite lands, and the wheel then moved the page 400 →
// 3400 with the dialog still open and the lock apparently in place. A
// class on the document element is owned by nobody's render.
//
// ⚠️ THE GUTTER IS MEASURED, NOT ASSUMED, which is why it is a SECOND
// class rather than part of the first. Removing a classic scrollbar
// reflows everything behind the dialog by its width; reserving one where
// the platform draws OVERLAY scrollbars introduces the very shift it is
// meant to prevent. `scrollbar-gutter: stable` reserves exactly a
// classic scrollbar's width and is honoured on an `overflow: hidden` box
// (measured: 0 → 15px), so it goes on only when the element was actually
// showing one. The `html { scrollbar-gutter: stable }` in app.css does
// not cover this — that rule is on the document, and the document is not
// what scrolls.

/** Marks the document as holding at least one open modal. The rules
 *  live in app.css beside the other document-level ones. */
const LOCK_CLASS = 'aa-modal-open';
/** Applied only when the locked scroller was drawing a classic
 *  scrollbar, so the lock does not move the page sideways. */
const GUTTER_CLASS = 'aa-modal-gutter';

function appScroller(): HTMLElement | null {
  if (typeof document === 'undefined') return null;
  // Same selector scrollSnapshot.ts and chromeScroll.svelte.ts use. The
  // FIRST match is the app shell's: /admin/integrations/api embeds
  // Scalar, which renders a `<main>` of its own — inside ours.
  return document.querySelector('main');
}

function lockBackgroundScroll(): void {
  const el = appScroller();
  if (!el) return;
  // Measured BEFORE the class lands, while the scrollbar (if the
  // platform draws one) is still there to measure.
  const gutter = el.offsetWidth - el.clientWidth;
  const root = document.documentElement;
  root.classList.add(LOCK_CLASS);
  root.classList.toggle(GUTTER_CLASS, gutter > 0);
}

function releaseBackgroundScroll(): void {
  if (typeof document === 'undefined') return;
  document.documentElement.classList.remove(LOCK_CLASS, GUTTER_CLASS);
}
