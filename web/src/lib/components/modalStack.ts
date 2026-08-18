// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/** Which modal is on top (#1207).
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
  if (at >= 0) modalStack.splice(at, 1);
}
